package ltfs

import "encoding/xml"

type Label struct {
	XMLName     xml.Name        `xml:"ltfslabel"`
	Version     string          `xml:"version,attr"`
	Creator     string          `xml:"creator"`
	FormatTime  string          `xml:"formattime"`
	VolumeUUID  string          `xml:"volumeuuid"`
	Location    LabelLocation   `xml:"location"`
	Partitions  LabelPartitions `xml:"partitions"`
	BlockSize   int             `xml:"blocksize"`
	Compression bool            `xml:"compression"`
}

type LabelLocation struct {
	Partition string `xml:"partition"`
}

type LabelPartitions struct {
	Index string `xml:"index"`
	Data  string `xml:"data"`
}

func NewLabel(partition, volumeUUID string) *Label {
	return &Label{
		Version:    FormatVersion,
		Creator:    CreatorString,
		FormatTime: Now(),
		VolumeUUID: volumeUUID,
		Location: LabelLocation{
			Partition: partition,
		},
		Partitions: LabelPartitions{
			Index: "a",
			Data:  "b",
		},
		BlockSize:   524288,
		Compression: true,
	}
}

func ParseLabel(data []byte) (*Label, error) {
	var label Label
	if err := xml.Unmarshal(data, &label); err != nil {
		return nil, err
	}
	return &label, nil
}

func (l *Label) Marshal() ([]byte, error) {
	l.XMLName = xml.Name{Local: "ltfslabel"}
	l.Version = FormatVersion
	if l.Creator == "" {
		l.Creator = CreatorString
	}
	body, err := xml.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, err
	}
	out := append([]byte(xml.Header), body...)
	out = append(out, '\n')
	return out, nil
}
