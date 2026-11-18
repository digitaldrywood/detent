package neutralimport

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

const VersionSnapshotInterval = 32

type versionPatch struct {
	Op      string          `json:"op"`
	Path    string          `json:"path"`
	Value   json.RawMessage `json:"value,omitempty"`
	Keys    []string        `json:"keys,omitempty"`
	Digests string          `json:"digests,omitempty"`
}

func VersionObject(raw []byte) ([]string, map[string]json.RawMessage, error) {
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

func VersionObjectBytes(keys []string, fields map[string]json.RawMessage) ([]byte, error) {
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

func versionDigests(keys []string, fields map[string]json.RawMessage) []byte {
	digests := make([]byte, 0, len(keys)*sha256.Size)
	for _, key := range keys {
		digest := sha256.Sum256(fields[key])
		digests = append(digests, digest[:]...)
	}
	return digests
}

func versionBasis(raw []byte, encoding string) ([]string, []byte, error) {
	switch encoding {
	case "snapshot":
		keys, fields, err := VersionObject(raw)
		if err != nil {
			return nil, nil, err
		}
		return keys, versionDigests(keys, fields), nil
	case "patch":
		var operations []versionPatch
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

func EncodeVersion(previous []byte, previousEncoding string, raw []byte, revision int64) ([]byte, string, error) {
	keys, fields, err := VersionObject(raw)
	if err != nil {
		return nil, "", err
	}
	canonical, err := VersionObjectBytes(keys, fields)
	if err != nil {
		return nil, "", err
	}
	compact, err := json.Marshal(json.RawMessage(raw))
	if err != nil {
		return nil, "", err
	}
	if len(previous) == 0 || revision == 1 || revision%VersionSnapshotInterval == 0 || !bytes.Equal(canonical, raw) || !bytes.Equal(compact, raw) {
		return raw, "snapshot", nil
	}
	oldKeys, oldDigests, err := versionBasis(previous, previousEncoding)
	if err != nil {
		return nil, "", err
	}
	digests := versionDigests(keys, fields)
	old := make(map[string][]byte, len(oldKeys))
	for index, key := range oldKeys {
		old[key] = oldDigests[index*sha256.Size : (index+1)*sha256.Size]
	}
	operations := []versionPatch{}
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
		operations = append(operations, versionPatch{Op: op, Path: pointer(key), Value: fields[key]})
	}
	for _, key := range oldKeys {
		if _, exists := fields[key]; !exists {
			operations = append(operations, versionPatch{Op: "remove", Path: pointer(key)})
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

func ApplyVersionPatch(previous, patch []byte) ([]byte, error) {
	_, fields, err := VersionObject(previous)
	if err != nil {
		return nil, err
	}
	var operations []versionPatch
	if err := json.Unmarshal(patch, &operations); err != nil {
		return nil, err
	}
	keys, digests, err := versionBasis(patch, "patch")
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
	if !bytes.Equal(digests, versionDigests(keys, fields)) {
		return nil, errors.New("history patch digest mismatch")
	}
	return VersionObjectBytes(keys, fields)
}
