#!/usr/bin/env bash
# Integration test for tapefuse.
#
# Tests the full workflow: init → mount → write → verify → umount → commit →
# kill daemon → start daemon → load → mount → verify content survives reload.
#
# By default a file-backed tape image is used so the test runs without any
# special hardware.  Set TAPEFUSE_TEST_DEVICE to a real or MHVTL SCSI tape
# device (e.g. /dev/nst0) to run against a physical (or virtual) tape drive.
#
# Requirements: ltape binary on PATH or in ../bin, fusermount available.
#
# Usage:
#   ./scripts/mhvtl_integration_test.sh [--device /dev/nst0]

set -euo pipefail

# ── Argument parsing ────────────────────────────────────────────────────────
DEVICE="${TAPEFUSE_TEST_DEVICE:-}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --device) DEVICE="$2"; shift 2 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done

# ── Binary resolution ───────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(dirname "$SCRIPT_DIR")"

LTAPE=""
for candidate in "$(which ltape 2>/dev/null)" "$REPO_ROOT/bin/ltape" "/tmp/ltape_inttest_bin"; do
  if [[ -x "$candidate" ]]; then
    LTAPE="$candidate"
    break
  fi
done
if [[ -z "$LTAPE" ]]; then
  echo "ltape binary not found; building …"
  go build -o /tmp/ltape_inttest_bin "$REPO_ROOT/cmd/ltape"
  LTAPE=/tmp/ltape_inttest_bin
fi
echo "Using ltape binary: $LTAPE"

# ── Temporary state ─────────────────────────────────────────────────────────
WORK_DIR="$(mktemp -d)"
MOUNT_POINT="$WORK_DIR/mnt"
LETTER="q"

if [[ -z "$DEVICE" ]]; then
  TAPE_IMAGE="$WORK_DIR/test_tape"
  DEVICE="$TAPE_IMAGE"
  echo "Using file-backed tape image: $TAPE_IMAGE"
else
  TAPE_IMAGE=""
  echo "Using SCSI tape device: $DEVICE"
fi

# ── Helpers ─────────────────────────────────────────────────────────────────
ltape() { "$LTAPE" "$@"; }

fail() { echo "FAIL: $*" >&2; exit 1; }

cleanup() {
  set +e
  ltape umount "$LETTER" 2>/dev/null
  ltape killdaemon 2>/dev/null
  fusermount -u "$MOUNT_POINT" 2>/dev/null
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT

wait_for_daemon() {
  local retries=20
  while (( retries-- > 0 )); do
    if [[ -S /tmp/ltape/ltaped.sock ]]; then return 0; fi
    sleep 0.2
  done
  fail "daemon did not start within 4 s"
}

# ── Test body ───────────────────────────────────────────────────────────────
mkdir -p "$MOUNT_POINT"

echo "── Step 1: start daemon ──"
ltape startdaemon
wait_for_daemon

echo "── Step 2: init tape ──"
ltape assign "$DEVICE" "$LETTER"
ltape indexread -f "$LETTER"

echo "── Step 3: mount ──"
ltape mount "$LETTER" "$MOUNT_POINT"

echo "── Step 4: write files ──"
printf 'hello tape world\n'   > "$MOUNT_POINT/file1.txt"
printf 'second file content\n' > "$MOUNT_POINT/file2.txt"
cp /etc/hostname "$MOUNT_POINT/hostname.txt"

echo "── Step 5: verify immediately (tests Flush synchrony) ──"
GOT=$(cat "$MOUNT_POINT/file1.txt")
[[ "$GOT" == "hello tape world" ]] || fail "file1.txt immediate content mismatch: '$GOT'"

GOT=$(cat "$MOUNT_POINT/file2.txt")
[[ "$GOT" == "second file content" ]] || fail "file2.txt immediate content mismatch: '$GOT'"

HOST_EXPECTED=$(cat /etc/hostname)
GOT=$(cat "$MOUNT_POINT/hostname.txt")
[[ "$GOT" == "$HOST_EXPECTED" ]] || fail "hostname.txt immediate content mismatch"

# File sizes must be non-zero
SIZE1=$(stat -c%s "$MOUNT_POINT/file1.txt")
(( SIZE1 > 0 )) || fail "file1.txt has size 0 (content not written)"

echo "── Step 6: umount ──"
ltape umount "$LETTER"

echo "── Step 7: commit index to tape ──"
ltape commitindex "$LETTER"

echo "── Step 8: kill daemon ──"
ltape killdaemon
sleep 0.5

echo "── Step 9: start fresh daemon ──"
ltape startdaemon
wait_for_daemon

echo "── Step 10: reload tape ──"
ltape assign "$DEVICE" "$LETTER"
ltape indexread -f "$LETTER"

echo "── Step 11: remount ──"
ltape mount "$LETTER" "$MOUNT_POINT"

echo "── Step 12: verify content after reload ──"
GOT=$(cat "$MOUNT_POINT/file1.txt")
[[ "$GOT" == "hello tape world" ]] || fail "file1.txt post-reload content mismatch: '$GOT'"

GOT=$(cat "$MOUNT_POINT/file2.txt")
[[ "$GOT" == "second file content" ]] || fail "file2.txt post-reload content mismatch: '$GOT'"

GOT=$(cat "$MOUNT_POINT/hostname.txt")
[[ "$GOT" == "$HOST_EXPECTED" ]] || fail "hostname.txt post-reload content mismatch"

SIZE1_R=$(stat -c%s "$MOUNT_POINT/file1.txt")
(( SIZE1_R > 0 )) || fail "file1.txt has size 0 after reload (index not committed)"

echo "── Step 13: cleanup ──"
ltape umount "$LETTER"
ltape killdaemon

echo ""
echo "All integration tests PASSED ✓"
