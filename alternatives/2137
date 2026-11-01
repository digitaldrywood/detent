package hubserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

const nativeVersionSnapshotInterval = 32

type nativeVersionPatch struct {
	Op      string          `json:"op"`
	Path    string          `json:"path"`
	Value   json.RawMessage `json:"value,omitempty"`
	Keys    []string        `json:"keys,omitempty"`
	Digests string          `json:"digests,omitempty"`
}

func nativeVersionObject(raw []byte) ([]string, map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, nil, err
	}
	if token != json.Delim('{') {
		return nil, nil, errors.New("history record must be an object")
	}
	var keys []string
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, nil, errors.New("invalid history member")
		}
		if _, exists := fields[key]; exists {
			return nil, nil, errors.New("duplicate history member")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, err
		}
		keys = append(keys, key)
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, nil, errors.New("trailing history data")
	}
	return keys, fields, nil
}

func nativeVersionObjectBytes(keys []string, fields map[string]json.RawMessage) ([]byte, error) {
	if len(keys) != len(fields) {
		return nil, errors.New("history member count mismatch")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	seen := make(map[string]bool, len(keys))
	for index, key := range keys {
		value, ok := fields[key]
		if !ok || seen[key] {
			return nil, errors.New("history member mismatch")
		}
		seen[key] = true
		if index > 0 {
			out.WriteByte(',')
		}
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out.Write(encoded)
		out.WriteByte(':')
		out.Write(value)
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

func nativeVersionDigests(keys []string, fields map[string]json.RawMessage) []byte {
	digests := make([]byte, 0, len(keys)*sha256.Size)
	for _, key := range keys {
		digest := sha256.Sum256(fields[key])
		digests = append(digests, digest[:]...)
	}
	return digests
}

func nativeVersionBasis(raw []byte, encoding string) ([]string, []byte, error) {
	switch encoding {
	case "snapshot":
		keys, fields, err := nativeVersionObject(raw)
		if err != nil {
			return nil, nil, err
		}
		return keys, nativeVersionDigests(keys, fields), nil
	case "patch":
		var operations []nativeVersionPatch
		if err := json.Unmarshal(raw, &operations); err != nil {
			return nil, nil, err
		}
		if len(operations) == 0 {
			return nil, nil, errors.New("missing history patch metadata")
		}
		digests, err := base64.StdEncoding.DecodeString(operations[0].Digests)
		if err != nil {
			return nil, nil, err
		}
		if len(digests) != len(operations[0].Keys)*sha256.Size {
			return nil, nil, errors.New("invalid history patch metadata")
		}
		return operations[0].Keys, digests, nil
	default:
		return nil, nil, errors.New("unknown history encoding")
	}
}

func encodeNativeVersion(previous []byte, previousEncoding string, raw []byte, revision int64) ([]byte, string, error) {
	keys, fields, err := nativeVersionObject(raw)
	if err != nil {
		return nil, "", err
	}
	canonical, err := nativeVersionObjectBytes(keys, fields)
	if err != nil {
		return nil, "", err
	}
	compact, err := json.Marshal(json.RawMessage(raw))
	if err != nil {
		return nil, "", err
	}
	if len(previous) == 0 || revision == 1 || revision%nativeVersionSnapshotInterval == 0 || !bytes.Equal(canonical, raw) || !bytes.Equal(compact, raw) {
		return raw, "snapshot", nil
	}
	oldKeys, oldDigests, err := nativeVersionBasis(previous, previousEncoding)
	if err != nil {
		return nil, "", err
	}
	digests := nativeVersionDigests(keys, fields)
	old := make(map[string][]byte, len(oldKeys))
	for index, key := range oldKeys {
		old[key] = oldDigests[index*sha256.Size : (index+1)*sha256.Size]
	}
	operations := []nativeVersionPatch{}
	pointer := func(key string) string {
		return "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
	}
	for index, key := range keys {
		digest, exists := old[key]
		if exists && bytes.Equal(digest, digests[index*sha256.Size:(index+1)*sha256.Size]) {
			continue
		}
		op := "replace"
		if !exists {
			op = "add"
		}
		operations = append(operations, nativeVersionPatch{Op: op, Path: pointer(key), Value: fields[key]})
	}
	for _, key := range oldKeys {
		if _, exists := fields[key]; !exists {
			operations = append(operations, nativeVersionPatch{Op: "remove", Path: pointer(key)})
		}
	}
	if len(operations) == 0 {
		return raw, "snapshot", nil
	}
	operations[0].Keys = keys
	operations[0].Digests = base64.StdEncoding.EncodeToString(digests)
	encoded, err := json.Marshal(operations)
	if err == nil && len(encoded) >= len(raw) {
		return raw, "snapshot", nil
	}
	return encoded, "patch", err
}

func applyNativeVersionPatch(previous, patch []byte) ([]byte, error) {
	_, fields, err := nativeVersionObject(previous)
	if err != nil {
		return nil, err
	}
	var operations []nativeVersionPatch
	if err := json.Unmarshal(patch, &operations); err != nil {
		return nil, err
	}
	keys, digests, err := nativeVersionBasis(patch, "patch")
	if err != nil {
		return nil, err
	}
	for _, operation := range operations {
		if !strings.HasPrefix(operation.Path, "/") || strings.Contains(operation.Path[1:], "/") {
			return nil, errors.New("invalid history patch path")
		}
		key := strings.ReplaceAll(strings.ReplaceAll(operation.Path[1:], "~1", "/"), "~0", "~")
		_, exists := fields[key]
		switch operation.Op {
		case "replace", "remove":
			if !exists {
				return nil, errors.New("history patch member is missing")
			}
		case "add":
		default:
			return nil, errors.New("unsupported history patch operation")
		}
		if operation.Op == "remove" {
			delete(fields, key)
		} else {
			if !json.Valid(operation.Value) {
				return nil, errors.New("invalid history patch value")
			}
			fields[key] = operation.Value
		}
	}
	if !bytes.Equal(digests, nativeVersionDigests(keys, fields)) {
		return nil, errors.New("history patch digest mismatch")
	}
	return nativeVersionObjectBytes(keys, fields)
}

func reconstructNativeVersion(ctx context.Context, query nativeQueryer, organization, project, item, record string, revision int64) ([]byte, error) {
	var start int64
	var raw []byte
	err := query.QueryRowContext(ctx, `SELECT v.revision, v.record_json FROM collaboration_versions v
JOIN collaboration_versions target ON target.organization_id=v.organization_id AND target.project_id=v.project_id
AND target.work_item_id=v.work_item_id AND target.record_id=v.record_id
WHERE target.organization_id=? AND target.project_id=? AND target.work_item_id=? AND target.record_id=? AND target.revision=?
AND v.revision<=target.revision AND v.encoding='snapshot'
ORDER BY v.revision DESC LIMIT 1`, organization, project, item, record, revision).Scan(&start, &raw)
	if err != nil {
		return nil, err
	}
	if start == revision {
		return raw, nil
	}
	if revision-start >= nativeVersionSnapshotInterval {
		return nil, errors.New("history checkpoint interval exceeded")
	}
	rows, err := query.QueryContext(ctx, `SELECT revision, encoding, record_json FROM collaboration_versions
WHERE organization_id=? AND project_id=? AND work_item_id=? AND record_id=? AND revision>? AND revision<=? ORDER BY revision`, organization, project, item, record, start, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var next int64
		var encoding string
		var encoded []byte
		if err := rows.Scan(&next, &encoding, &encoded); err != nil {
			return nil, err
		}
		if next != start+1 {
			return nil, sql.ErrNoRows
		}
		switch encoding {
		case "snapshot":
			raw = slices.Clone(encoded)
		case "patch":
			raw, err = applyNativeVersionPatch(raw, encoded)
			if err != nil {
				return nil, fmt.Errorf("apply history revision %d: %w", next, err)
			}
		default:
			return nil, errors.New("unknown history encoding")
		}
		start = next
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if start != revision {
		return nil, sql.ErrNoRows
	}
	return raw, nil
}
