package core

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

type mp4Box struct {
	offset     int64
	size       int64
	headerSize int64
	typ        string
	header     []byte
	data       []byte
}

func scanMP4TopLevel(source Source) ([]mp4Box, error) {
	var boxes []mp4Box
	var seenMoov, seenMdat bool
	for offset := int64(0); offset < source.Size(); {
		var header [16]byte
		n, err := source.ReadAt(header[:8], offset)
		if err != nil && !(err == io.EOF && n == 8) {
			return nil, fmt.Errorf("read MP4 box at %d: %w", offset, err)
		}
		size := int64(binary.BigEndian.Uint32(header[:4]))
		headerSize := int64(8)
		if size == 1 {
			if _, err := source.ReadAt(header[8:16], offset+8); err != nil {
				return nil, fmt.Errorf("read extended MP4 box at %d: %w", offset, err)
			}
			size64 := binary.BigEndian.Uint64(header[8:16])
			if size64 > math.MaxInt64 {
				return nil, fmt.Errorf("MP4 box too large at %d", offset)
			}
			size = int64(size64)
			headerSize = 16
		} else if size == 0 {
			size = source.Size() - offset
		}
		if size < headerSize || size > source.Size()-offset {
			return nil, fmt.Errorf("invalid MP4 box %q at %d: size=%d", string(header[4:8]), offset, size)
		}
		box := mp4Box{
			offset: offset, size: size, headerSize: headerSize, typ: string(header[4:8]),
			header: append([]byte(nil), header[:headerSize]...),
		}
		if box.typ == "ftyp" || box.typ == "moov" {
			if box.size > maxMetadataElementSize {
				return nil, fmt.Errorf("MP4 %s box exceeds metadata limit: %d", box.typ, box.size)
			}
			box.data = make([]byte, box.size)
			if _, err := source.ReadAt(box.data, box.offset); err != nil {
				return nil, fmt.Errorf("read MP4 %s: %w", box.typ, err)
			}
		}
		boxes = append(boxes, box)
		seenMoov = seenMoov || box.typ == "moov"
		seenMdat = seenMdat || box.typ == "mdat"
		offset += size
		if seenMoov && seenMdat {
			break
		}
	}
	return boxes, nil
}

func probeMP4(source Source) (*Plan, error) {
	boxes, err := scanMP4TopLevel(source)
	if err != nil {
		return nil, err
	}
	var hasMoov bool
	var extents []Extent
	for _, box := range boxes {
		switch box.typ {
		case "moov", "ftyp":
			extents = append(extents, Extent{Offset: box.offset, Data: box.data})
			hasMoov = hasMoov || box.typ == "moov"
		default:
			extents = append(extents, Extent{Offset: box.offset, Data: box.header})
		}
	}
	if !hasMoov {
		return nil, fmt.Errorf("%w: MP4 has no moov box", ErrUnsupported)
	}
	return NewPlan(source.Size(), extents)
}
