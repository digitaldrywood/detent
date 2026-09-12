package releaseprovenance

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	Schema           = 1
	annotationPrefix = "<!-- detent-release-provenance:"
	annotationSuffix = " -->"
)

type Check struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	CheckRunID int64  `json:"check_run_id,omitempty"`
}

type Manifest struct {
	Schema     int     `json:"schema"`
	Repository string  `json:"repository"`
	Tag        string  `json:"tag"`
	Commit     string  `json:"commit"`
	Checks     []Check `json:"checks"`
}

func Validate(manifest Manifest, repository string, tag string, commit string) error {
	if manifest.Schema != Schema {
		return fmt.Errorf("release provenance schema = %d, want %d", manifest.Schema, Schema)
	}
	if strings.TrimSpace(manifest.Repository) == "" || manifest.Repository != strings.TrimSpace(repository) {
		return fmt.Errorf("release provenance repository = %q, want %q", manifest.Repository, strings.TrimSpace(repository))
	}
	if strings.TrimSpace(manifest.Tag) == "" || manifest.Tag != strings.TrimSpace(tag) {
		return fmt.Errorf("release provenance tag = %q, want %q", manifest.Tag, strings.TrimSpace(tag))
	}
	manifestCommit, err := fullCommit(manifest.Commit)
	if err != nil {
		return fmt.Errorf("release provenance commit: %w", err)
	}
	if strings.TrimSpace(commit) != "" {
		expectedCommit, err := fullCommit(commit)
		if err != nil {
			return fmt.Errorf("expected release commit: %w", err)
		}
		if manifestCommit != expectedCommit {
			return fmt.Errorf("release provenance commit = %q, want %q", manifestCommit, expectedCommit)
		}
	}
	if len(manifest.Checks) == 0 {
		return errors.New("release provenance has no mandatory check evidence")
	}
	seen := make(map[string]struct{}, len(manifest.Checks))
	for _, check := range manifest.Checks {
		name := strings.TrimSpace(check.Name)
		if name == "" {
			return errors.New("release provenance contains an unnamed mandatory check")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("release provenance contains duplicate mandatory check %q", name)
		}
		seen[name] = struct{}{}
		if !strings.EqualFold(strings.TrimSpace(check.Status), "completed") || !strings.EqualFold(strings.TrimSpace(check.Conclusion), "success") {
			return fmt.Errorf("release provenance mandatory check %q is %s/%s, want completed/success", name, strings.TrimSpace(check.Status), strings.TrimSpace(check.Conclusion))
		}
	}
	return nil
}

func Parse(raw []byte, repository string, tag string, commit string) (Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode release provenance: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Manifest{}, err
	}
	if err := Validate(manifest, repository, tag, commit); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func Marshal(manifest Manifest) ([]byte, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode release provenance: %w", err)
	}
	return append(raw, '\n'), nil
}

func Annotation(manifest Manifest) (string, error) {
	if err := Validate(manifest, manifest.Repository, manifest.Tag, manifest.Commit); err != nil {
		return "", err
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode release provenance annotation: %w", err)
	}
	return annotationPrefix + string(raw) + annotationSuffix, nil
}

func FromTagMessage(message string, repository string, tag string, commit string) (Manifest, error) {
	var manifest Manifest
	found := false
	for line := range strings.SplitSeq(message, "\n") {
		line = strings.TrimSpace(line)
		if payload, matched := strings.CutPrefix(line, annotationPrefix); matched {
			if found {
				return Manifest{}, errors.New("release tag contains multiple provenance annotations")
			}
			raw, closed := strings.CutSuffix(payload, annotationSuffix)
			if !closed {
				return Manifest{}, errors.New("release provenance annotation is not closed")
			}
			parsed, err := Parse([]byte(raw), repository, tag, commit)
			if err != nil {
				return Manifest{}, err
			}
			manifest = parsed
			found = true
		}
	}
	if !found {
		return Manifest{}, errors.New("release tag is missing provenance annotation")
	}
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode release provenance: multiple JSON values")
		}
		return fmt.Errorf("decode release provenance: %w", err)
	}
	return nil
}

func fullCommit(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 40 && len(value) != 64 {
		return "", fmt.Errorf("%q is not a full git commit", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("%q is not a hexadecimal git commit", value)
	}
	return value, nil
}
