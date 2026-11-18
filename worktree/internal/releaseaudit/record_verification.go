package releaseaudit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func (e *execution) verifyRecord(ctx context.Context, phase string, record json.RawMessage) (json.RawMessage, bool, error) {
	canonical, err := json.Marshal(record)
	if err != nil {
		return nil, false, errors.New("release audit record is invalid JSON")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(canonical))
	response, err := e.invoke(ctx, phase, struct {
		Record json.RawMessage `json:"record"`
		Digest string          `json:"record_sha256"`
	}{Record: canonical, Digest: digest})
	if err != nil {
		return nil, false, err
	}
	var review struct {
		Decision    string          `json:"decision"`
		Digest      string          `json:"record_sha256"`
		Replacement json.RawMessage `json:"replacement,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(response.Output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&review) != nil || decoder.Decode(&struct{}{}) != io.EOF || review.Digest != digest {
		return nil, false, errors.New("release audit record review is invalid or refers to another report")
	}
	switch review.Decision {
	case "accept":
		if len(review.Replacement) != 0 {
			return nil, false, errors.New("release audit record acceptance also supplied a replacement")
		}
		return canonical, true, nil
	case "replace":
		if len(review.Replacement) == 0 {
			return nil, false, errors.New("release audit record replacement is missing")
		}
		return review.Replacement, false, nil
	default:
		return nil, false, errors.New("release audit record review has an unknown decision")
	}
}
