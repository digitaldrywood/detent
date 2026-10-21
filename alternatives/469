package attachment

import (
	"bytes"
	"encoding/binary"
)

func stripJPEGMetadata(content []byte) ([]byte, error) {
	if len(content) < 2 || content[0] != 0xff || content[1] != 0xd8 {
		return nil, ErrInvalid
	}
	cleaned := make([]byte, 0, len(content))
	cleaned = append(cleaned, content[:2]...)
	inScan := false
	for offset := 2; offset < len(content); {
		if inScan {
			next := bytes.IndexByte(content[offset:], 0xff)
			if next < 0 {
				return nil, ErrInvalid
			}
			cleaned = append(cleaned, content[offset:offset+next]...)
			offset += next
		}
		start := offset
		if content[offset] != 0xff {
			return nil, ErrInvalid
		}
		for offset < len(content) && content[offset] == 0xff {
			offset++
		}
		if offset == len(content) {
			return nil, ErrInvalid
		}
		marker := content[offset]
		offset++
		if marker == 0x00 || marker >= 0xd0 && marker <= 0xd7 {
			if !inScan {
				return nil, ErrInvalid
			}
			cleaned = append(cleaned, content[start:offset]...)
			continue
		}
		if marker == 0xd9 {
			return append(cleaned, content[start:offset]...), nil
		}
		if len(content)-offset < 2 {
			return nil, ErrInvalid
		}
		length := int(binary.BigEndian.Uint16(content[offset:]))
		if length < 2 || length > len(content)-offset {
			return nil, ErrInvalid
		}
		payload := content[offset+2 : offset+length]
		offset += length
		switch {
		case marker >= 0xe0 && marker <= 0xef:
			if colorData := jpegColorData(marker, payload); colorData != nil {
				cleaned = append(cleaned, 0xff, marker)
				cleaned = binary.BigEndian.AppendUint16(cleaned, uint16(len(colorData)+2))
				cleaned = append(cleaned, colorData...)
			}
		case marker != 0xfe:
			cleaned = append(cleaned, content[start:offset]...)
		}
		inScan = marker == 0xda
	}
	return nil, ErrInvalid
}

func jpegColorData(marker byte, payload []byte) []byte {
	switch marker {
	case 0xe0:
		if bytes.HasPrefix(payload, []byte("JFIF\x00")) {
			return []byte("JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
		}
	case 0xe2:
		if len(payload) > 14 && bytes.HasPrefix(payload, []byte("ICC_PROFILE\x00")) && payload[12] > 0 && payload[12] <= payload[13] {
			return payload
		}
	case 0xee:
		if len(payload) >= 12 && bytes.HasPrefix(payload, []byte("Adobe")) && payload[11] <= 2 {
			return payload[:12]
		}
	}
	return nil
}
