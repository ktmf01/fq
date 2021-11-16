package mpeg

// ISO/IEC 14496-15, 5.3.3.1.2 Syntax

import (
	"github.com/wader/fq/format"
	"github.com/wader/fq/format/registry"
	"github.com/wader/fq/pkg/decode"
)

var avcNALUFormat []*decode.Format

func init() {
	registry.MustRegister(&decode.Format{
		Name:        format.AVC_AU,
		Description: "H.264/AVC Access Unit",
		DecodeFn:    avcAUDecode,
		RootArray:   true,
		RootName:    "access_unit",
		Dependencies: []decode.Dependency{
			{Names: []string{format.AVC_NALU}, Formats: &avcNALUFormat},
		},
	})
}

func avcAUDecode(d *decode.D, in interface{}) interface{} {
	avcIn, ok := in.(format.AvcIn)
	if !ok {
		d.Fatal("avcIn required")
	}

	for d.NotEnd() {
		d.FieldStruct("nalu", func(d *decode.D) {
			l := d.FieldU("length", int(avcIn.LengthSize)*8)
			d.FieldFormatLen("nalu", int64(l)*8, avcNALUFormat, nil)
		})
	}

	return nil
}
