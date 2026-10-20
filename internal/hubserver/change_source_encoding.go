package hubserver

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const changeSourceMagic = "\x00DCS"

func encodeChangeSource(bundle []byte) ([]byte, error) {
	var compressed bytes.Buffer
	compressed.WriteString(changeSourceMagic)
	compressed.Write([]byte{1, 1})
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(bundle); err != nil {
		return nil, errors.New("compress change source failed")
	}
	if err := writer.Close(); err != nil {
		return nil, errors.New("finish change source compression failed")
	}
	if compressed.Len() < len(bundle) {
		return compressed.Bytes(), nil
	}
	return append([]byte(changeSourceMagic+"\x01\x00"), bundle...), nil
}

func decodeChangeSource(stored []byte, recordedBytes int64) ([]byte, error) {
	if recordedBytes <= 0 || recordedBytes > tracker.MaxChangeSourceBytes {
		return nil, errors.New("change source size is invalid")
	}
	if !bytes.HasPrefix(stored, []byte(changeSourceMagic)) {
		if int64(len(stored)) != recordedBytes {
			return nil, errors.New("change source size does not match metadata")
		}
		return stored, nil
	}
	if len(stored) < 6 || stored[4] != 1 {
		return nil, errors.New("change source frame is unsupported")
	}
	payload := stored[6:]
	switch stored[5] {
	case 0:
		if int64(len(payload)) != recordedBytes {
			return nil, errors.New("change source size does not match metadata")
		}
		return payload, nil
	case 1:
		input := bytes.NewReader(payload)
		reader, err := gzip.NewReader(input)
		if err != nil {
			return nil, errors.New("change source compressed frame is invalid")
		}
		reader.Multistream(false)
		bundle, err := io.ReadAll(io.LimitReader(reader, recordedBytes+1))
		closeErr := reader.Close()
		if err != nil || closeErr != nil || int64(len(bundle)) != recordedBytes || input.Len() != 0 {
			return nil, errors.New("change source compressed frame is corrupt or exceeds its size")
		}
		return bundle, nil
	default:
		return nil, errors.New("change source frame encoding is unsupported")
	}
}
