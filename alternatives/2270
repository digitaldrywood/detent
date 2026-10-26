package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
)

type storageJSONReference struct {
	Organization string                      `json:"organization_id"`
	Project      string                      `json:"project_id"`
	Family       string                      `json:"family"`
	Row          int64                       `json:"row"`
	Reference    attachment.ArchiveReference `json:"reference"`
}

type storageJSONPayload struct {
	Organization string          `json:"organization_id"`
	Project      string          `json:"project_id"`
	Family       string          `json:"family"`
	Row          int64           `json:"row"`
	Data         json.RawMessage `json:"data"`
}

func resolveStorageJSON(ctx context.Context, raw []byte) ([]byte, error) {
	if explanationRead(ctx) {
		return raw, nil
	}
	return resolveStorageJSONWith(ctx, raw, readChangeContent)
}

func resolveStorageJSONWith(ctx context.Context, raw []byte, read func(context.Context, string, attachment.ArchiveReference) ([]byte, error)) ([]byte, error) {
	if !bytes.Contains(raw, []byte(`"_storage_archive"`)) {
		return raw, nil
	}
	var envelope struct {
		Archive *storageJSONReference `json:"_storage_archive"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, err
	}
	if envelope.Archive == nil {
		return raw, nil
	}
	r := envelope.Archive
	data, err := read(ctx, r.Organization, r.Reference)
	if err != nil {
		return nil, err
	}
	var payload storageJSONPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	if payload.Organization != r.Organization || payload.Project != r.Project || payload.Family != r.Family || payload.Row != r.Row || !json.Valid(payload.Data) {
		return nil, artifact.ErrIntegrity
	}
	expected, err := compactStorageJSON(payload.Data, *r)
	if err != nil {
		return nil, err
	}
	resolved, _, err := mergeStorageJSON(payload.Data, expected, raw)
	return resolved, err
}

func decodeStorageJSON(ctx context.Context, raw []byte, result any) error {
	data, err := resolveStorageJSON(ctx, raw)
	if err != nil {
		return fmt.Errorf("read converted content: %w", err)
	}
	return json.Unmarshal(data, result)
}

func compactStorageJSON(raw []byte, ref storageJSONReference) ([]byte, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, artifact.ErrIntegrity
	}
	for key, value := range object {
		switch key {
		case "policy":
			delete(object, key)
		case "output", "completion_body", "final_summary":
			if bytes.HasPrefix(bytes.TrimSpace(value), []byte(`"`)) {
				object[key] = json.RawMessage(`""`)
			}
		default:
			trimmed, err := trimStorageJSON(value)
			if err != nil {
				return nil, err
			}
			object[key] = trimmed
		}
	}
	if ref.Family == "conversation_events" {
		object["text"], object["data"] = json.RawMessage(`""`), json.RawMessage(`{}`)
	}
	encoded, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	object["_storage_archive"] = encoded
	return json.Marshal(object)
}

func trimStorageJSON(raw json.RawMessage) (json.RawMessage, error) {
	if bytes.HasPrefix(raw, []byte("{")) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
		for key, value := range object {
			if (key == "output" || key == "completion_body" || key == "final_summary" || key == "evidence" && len(value) > 4096) && bytes.HasPrefix(bytes.TrimSpace(value), []byte(`"`)) {
				object[key] = json.RawMessage(`""`)
				continue
			}
			trimmed, err := trimStorageJSON(value)
			if err != nil {
				return nil, err
			}
			object[key] = trimmed
		}
		return json.Marshal(object)
	}
	if bytes.HasPrefix(raw, []byte("[")) {
		var array []json.RawMessage
		if err := json.Unmarshal(raw, &array); err != nil {
			return nil, err
		}
		for i, value := range array {
			trimmed, err := trimStorageJSON(value)
			if err != nil {
				return nil, err
			}
			array[i] = trimmed
		}
		return json.Marshal(array)
	}
	return raw, nil
}

func mergeStorageJSON(original, expected, current json.RawMessage) (json.RawMessage, bool, error) {
	if bytes.Equal(bytes.TrimSpace(expected), bytes.TrimSpace(current)) {
		return original, false, nil
	}
	if !bytes.HasPrefix(bytes.TrimSpace(expected), []byte("{")) || !bytes.HasPrefix(bytes.TrimSpace(current), []byte("{")) {
		return current, true, nil
	}
	var source, before, after map[string]json.RawMessage
	for _, entry := range []struct {
		data   json.RawMessage
		target *map[string]json.RawMessage
	}{{original, &source}, {expected, &before}, {current, &after}} {
		if len(entry.data) == 0 {
			continue
		}
		if err := json.Unmarshal(entry.data, entry.target); err != nil {
			return nil, false, err
		}
	}
	if source == nil {
		source = map[string]json.RawMessage{}
	}
	changed := false
	for key := range before {
		if key == "_storage_archive" {
			continue
		}
		if _, ok := after[key]; !ok {
			delete(source, key)
			changed = true
		}
	}
	for key, value := range after {
		if key == "_storage_archive" {
			continue
		}
		previous, ok := before[key]
		if !ok {
			source[key] = value
			changed = true
			continue
		}
		resolved, updated, err := mergeStorageJSON(source[key], previous, value)
		if err != nil {
			return nil, false, err
		}
		if updated {
			source[key] = resolved
			changed = true
		}
	}
	if !changed {
		return original, false, nil
	}
	data, err := json.Marshal(source)
	return data, true, err
}
