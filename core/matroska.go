package core

import (
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	ebmlHeaderID  = 0x1a45dfa3
	ebmlSegmentID = 0x18538067
	ebmlInfoID    = 0x1549a966
	ebmlTracksID  = 0x1654ae6b
	ebmlTagsID    = 0x1254c367
	ebmlClusterID = 0x1f43b675
)

type ebmlElement struct {
	id         uint64
	offset     int64
	headerSize int64
	size       int64
	unknown    bool
}

func readEBMLElement(source Source, offset int64) (ebmlElement, error) {
	var buf [12]byte
	n, err := source.ReadAt(buf[:], offset)
	if err != nil && err != io.EOF {
		return ebmlElement{}, err
	}
	if n == 0 {
		return ebmlElement{}, io.EOF
	}
	idLen, err := vintLength(buf[0], 4)
	if err != nil || n < idLen+1 {
		return ebmlElement{}, errors.New("invalid EBML element ID")
	}
	id := uint64(0)
	for _, b := range buf[:idLen] {
		id = id<<8 | uint64(b)
	}
	sizeLen, err := vintLength(buf[idLen], 8)
	if err != nil || n < idLen+sizeLen {
		return ebmlElement{}, errors.New("invalid EBML element size")
	}
	mask := byte(0xff >> sizeLen)
	size := uint64(buf[idLen] & mask)
	for _, b := range buf[idLen+1 : idLen+sizeLen] {
		size = size<<8 | uint64(b)
	}
	unknownValue := uint64(1)<<(7*sizeLen) - 1
	if size > math.MaxInt64 {
		return ebmlElement{}, errors.New("EBML element too large")
	}
	return ebmlElement{
		id: id, offset: offset, headerSize: int64(idLen + sizeLen), size: int64(size), unknown: size == unknownValue,
	}, nil
}

func vintLength(first byte, maxLength int) (int, error) {
	mask := byte(0x80)
	for length := 1; length <= maxLength; length++ {
		if first&mask != 0 {
			return length, nil
		}
		mask >>= 1
	}
	return 0, errors.New("invalid variable integer")
}

func encodeVINT(value uint64, length int) ([]byte, error) {
	if length < 1 || length > 8 || value >= uint64(1)<<(7*length)-1 {
		return nil, fmt.Errorf("value %d does not fit EBML VINT length %d", value, length)
	}
	out := make([]byte, length)
	for i := length - 1; i >= 0; i-- {
		out[i] = byte(value)
		value >>= 8
	}
	out[0] |= 1 << (8 - length)
	return out, nil
}

func probeMatroska(source Source) (*Plan, error) {
	header, err := readEBMLElement(source, 0)
	if err != nil || header.id != ebmlHeaderID || header.unknown {
		return nil, fmt.Errorf("invalid Matroska EBML header: %w", err)
	}
	headerEnd := header.offset + header.headerSize + header.size
	if headerEnd > source.Size() || headerEnd > 1<<20 {
		return nil, errors.New("truncated Matroska EBML header")
	}
	segment, err := readEBMLElement(source, headerEnd)
	if err != nil || segment.id != ebmlSegmentID {
		return nil, fmt.Errorf("Matroska segment not found: %w", err)
	}
	segmentBody := segment.offset + segment.headerSize
	segmentEnd := source.Size()
	if !segment.unknown && segment.size <= source.Size()-segmentBody {
		segmentEnd = segmentBody + segment.size
	}

	var infoRaw, tracksRaw, tagsRaw []byte
	metadataElement := func(id uint64) bool {
		return id == ebmlInfoID || id == ebmlTracksID || id == ebmlTagsID
	}

scanMetadata:
	for offset := segmentBody; offset < segmentEnd; {
		element, err := readEBMLElement(source, offset)
		if err != nil {
			break
		}
		if element.unknown {
			if element.id == ebmlClusterID {
				break
			}
			return nil, fmt.Errorf("unknown-sized Matroska element %#x", element.id)
		}
		if element.id == ebmlClusterID && len(infoRaw) != 0 && len(tracksRaw) != 0 {
			break
		}
		total := element.headerSize + element.size
		if total < element.headerSize || total > segmentEnd-offset {
			break
		}
		switch element.id {
		case ebmlInfoID, ebmlTracksID, ebmlTagsID:
			if total > maxMetadataElementSize {
				return nil, fmt.Errorf("Matroska metadata element %#x exceeds limit: %d", element.id, total)
			}
			raw := make([]byte, total)
			if _, err := source.ReadAt(raw, offset); err != nil {
				return nil, err
			}
			switch element.id {
			case ebmlInfoID:
				infoRaw = raw
			case ebmlTracksID:
				tracksRaw = raw
			case ebmlTagsID:
				tagsRaw = raw
			}
		}
		// Info and Tracks are sufficient for Jellyfin. Keep adjacent Tags,
		// but never jump across a large Void or media data to find them.
		if len(infoRaw) != 0 && len(tracksRaw) != 0 {
			if len(tagsRaw) != 0 || !metadataElement(element.id) {
				break scanMetadata
			}
		}
		offset += total
	}
	if len(infoRaw) == 0 || len(tracksRaw) == 0 {
		return nil, fmt.Errorf("%w: Matroska Info or Tracks missing", ErrUnsupported)
	}

	prefix, err := buildMatroskaPrefix(source, headerEnd, infoRaw, tracksRaw, tagsRaw)
	if err != nil {
		return nil, err
	}
	return NewPlan(source.Size(), []Extent{{Offset: 0, Data: prefix}})
}

func buildMatroskaPrefix(source Source, headerEnd int64, elements ...[]byte) ([]byte, error) {
	header := make([]byte, headerEnd)
	if _, err := source.ReadAt(header, 0); err != nil {
		return nil, err
	}
	const segmentHeaderSize = 12 // four-byte ID and eight-byte VINT size
	bodySize := source.Size() - headerEnd - segmentHeaderSize
	if bodySize < 9 {
		return nil, errors.New("Matroska object too small for sparse segment")
	}
	segmentSize, err := encodeVINT(uint64(bodySize), 8)
	if err != nil {
		return nil, err
	}
	prefix := append(header, 0x18, 0x53, 0x80, 0x67)
	prefix = append(prefix, segmentSize...)
	for _, element := range elements {
		prefix = append(prefix, element...)
	}
	// FFmpeg requires a Cluster before accepting a metadata-only Matroska.
	prefix = append(prefix,
		0x1f, 0x43, 0xb6, 0x75, // Cluster
		0x83,             // three-byte payload
		0xe7, 0x81, 0x00, // Timecode = 0
	)
	voidPayload := bodySize - int64(len(prefix)) + headerEnd + segmentHeaderSize - 9
	if voidPayload < 0 {
		return nil, errors.New("Matroska metadata does not fit sparse object")
	}
	voidSize, err := encodeVINT(uint64(voidPayload), 8)
	if err != nil {
		return nil, err
	}
	prefix = append(prefix, 0xec)
	prefix = append(prefix, voidSize...)
	return prefix, nil
}
