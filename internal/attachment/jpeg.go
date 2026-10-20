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
		offset += length
		metadata := marker == 0xfe || marker >= 0xe1 && marker <= 0xef && marker != 0xe2 && marker != 0xee
		if !metadata {
			cleaned = append(cleaned, content[start:offset]...)
		}
		inScan = marker == 0xda
	}
	return nil, ErrInvalid
}
