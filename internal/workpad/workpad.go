package workpad

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	SourceStructured   = "structured"
	SourceProse        = "prose"
	SourceProseSection = "prose_section"
	SourceProsePhrase  = "prose_phrase"

	StatusInProgress = "in_progress"
	StatusBlocked    = "blocked"
	StatusComplete   = "complete"
)

var refPattern = regexp.MustCompile(`^(?:([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#([1-9][0-9]*)$`)

type Signal struct {
	Source      string            `json:"source,omitempty" yaml:"source,omitempty"`
	CommentURL  string            `json:"comment_url,omitempty" yaml:"comment_url,omitempty"`
	Status      string            `json:"status,omitempty" yaml:"status,omitempty"`
	Blockers    []Blocker         `json:"blockers,omitempty" yaml:"blockers,omitempty"`
	HumanAction string            `json:"human_action,omitempty" yaml:"human_action,omitempty"`
	Fields      map[string]string `json:"fields,omitempty" yaml:"fields,omitempty"`
	Invalid     *Invalid          `json:"invalid,omitempty" yaml:"invalid,omitempty"`
}

type Blocker struct {
	Ref        string `json:"ref,omitempty" yaml:"ref,omitempty"`
	Identifier string `json:"identifier,omitempty" yaml:"identifier,omitempty"`
	Reason     string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

type Invalid struct {
	Hash    string `json:"hash,omitempty" yaml:"hash,omitempty"`
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
	Content string `json:"content,omitempty" yaml:"content,omitempty"`
}

type statusBlockYAML struct {
	Schema      int               `yaml:"schema"`
	Status      string            `yaml:"status"`
	Blockers    []blockerYAML     `yaml:"blockers"`
	HumanAction *string           `yaml:"human_action"`
	Fields      map[string]string `yaml:"fields"`
}

type blockerYAML struct {
	Ref    string `yaml:"ref"`
	Reason string `yaml:"reason"`
}

func SignalFromComment(body string, commentURL string, repo string) (*Signal, bool) {
	content, ok := LastStatusBlock(body)
	if !ok {
		return nil, false
	}

	block, err := ParseStatusBlock(content, repo)
	if err != nil {
		return &Signal{
			Source:     SourceStructured,
			CommentURL: strings.TrimSpace(commentURL),
			Invalid: &Invalid{
				Hash:    ContentHash(content),
				Message: err.Error(),
				Content: content,
			},
		}, true
	}
	block.CommentURL = strings.TrimSpace(commentURL)
	return block, true
}

func LastStatusBlock(body string) (string, bool) {
	var last string
	found := false
	inFence := false
	fenceChar := byte(0)
	fenceLen := 0
	lines := []string{}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inFence {
			char, length, ok := statusFenceOpening(trimmed)
			if !ok {
				continue
			}
			inFence = true
			fenceChar = char
			fenceLen = length
			lines = lines[:0]
			continue
		}
		if statusFenceClosing(trimmed, fenceChar, fenceLen) {
			last = strings.Join(lines, "\n")
			found = true
			inFence = false
			continue
		}
		lines = append(lines, line)
	}

	return last, found
}

func ParseStatusBlock(content string, repo string) (*Signal, error) {
	var raw statusBlockYAML
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse detent-status YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return nil, errors.New("parse detent-status YAML: multiple YAML documents are not supported")
	} else if !errors.Is(err, io.EOF) {
		return nil, errors.New("parse detent-status YAML: multiple YAML documents are not supported")
	}

	problems := []string{}
	if raw.Schema != 1 {
		problems = append(problems, "schema must be 1")
	}
	raw.Status = strings.TrimSpace(raw.Status)
	switch raw.Status {
	case StatusInProgress, StatusBlocked, StatusComplete:
	default:
		if raw.Status == "" {
			problems = append(problems, "status must be one of in_progress, blocked, complete")
		} else {
			problems = append(problems, fmt.Sprintf("status %q must be one of in_progress, blocked, complete", raw.Status))
		}
	}

	humanAction := ""
	if raw.HumanAction != nil {
		humanAction = strings.TrimSpace(*raw.HumanAction)
	}
	blockers := make([]Blocker, 0, len(raw.Blockers))
	for index, blocker := range raw.Blockers {
		ref := strings.TrimSpace(blocker.Ref)
		identifier, err := ParseRef(ref, repo)
		if err != nil {
			problems = append(problems, fmt.Sprintf("blockers[%d].ref %q must be #N or owner/repo#N", index, ref))
			continue
		}
		blockers = append(blockers, Blocker{
			Ref:        ref,
			Identifier: identifier,
			Reason:     strings.TrimSpace(blocker.Reason),
		})
	}
	if raw.Status == StatusBlocked && len(blockers) == 0 && humanAction == "" {
		problems = append(problems, "status blocked requires at least one blocker ref or human_action")
	}
	fields := make(map[string]string, len(raw.Fields))
	fieldNames := make([]string, 0, len(raw.Fields))
	for name := range raw.Fields {
		fieldNames = append(fieldNames, name)
	}
	sort.Strings(fieldNames)
	for _, name := range fieldNames {
		value := strings.TrimSpace(raw.Fields[name])
		name = strings.TrimSpace(name)
		if name == "" {
			problems = append(problems, "fields must not contain a blank field name")
			continue
		}
		if value == "" {
			problems = append(problems, fmt.Sprintf("fields[%q] must not be blank", name))
			continue
		}
		fields[name] = value
	}
	if len(fields) == 0 {
		fields = nil
	}
	if len(problems) > 0 {
		return nil, errors.New(strings.Join(problems, "; "))
	}

	return &Signal{
		Source:      SourceStructured,
		Status:      raw.Status,
		Blockers:    blockers,
		HumanAction: humanAction,
		Fields:      fields,
	}, nil
}

func ParseRef(ref string, repo string) (string, error) {
	ref = strings.TrimSpace(ref)
	matches := refPattern.FindStringSubmatch(ref)
	if len(matches) != 3 {
		return "", fmt.Errorf("invalid ref %q", ref)
	}
	if matches[1] != "" {
		return matches[1] + "#" + matches[2], nil
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return "#" + matches[2], nil
	}
	return repo + "#" + matches[2], nil
}

func Reason(signal *Signal) string {
	if signal == nil {
		return ""
	}
	parts := make([]string, 0, len(signal.Blockers)+1)
	if humanAction := strings.TrimSpace(signal.HumanAction); humanAction != "" {
		parts = append(parts, humanAction)
	}
	for _, blocker := range signal.Blockers {
		ref := strings.TrimSpace(blocker.Identifier)
		if ref == "" {
			ref = strings.TrimSpace(blocker.Ref)
		}
		if ref == "" {
			continue
		}
		if reason := strings.TrimSpace(blocker.Reason); reason != "" {
			parts = append(parts, ref+": "+reason)
			continue
		}
		parts = append(parts, ref)
	}
	return strings.Join(parts, "; ")
}

func CloneSignal(signal *Signal) *Signal {
	if signal == nil {
		return nil
	}
	cloned := *signal
	cloned.Blockers = append([]Blocker(nil), signal.Blockers...)
	if signal.Fields != nil {
		cloned.Fields = make(map[string]string, len(signal.Fields))
		for name, value := range signal.Fields {
			cloned.Fields[name] = value
		}
	}
	if signal.Invalid != nil {
		invalid := *signal.Invalid
		cloned.Invalid = &invalid
	}
	return &cloned
}

func ContentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:16]
}

func statusFenceOpening(line string) (byte, int, bool) {
	if len(line) < 3 || (line[0] != '`' && line[0] != '~') {
		return 0, 0, false
	}
	char := line[0]
	length := 0
	for length < len(line) && line[length] == char {
		length++
	}
	if length < 3 {
		return 0, 0, false
	}
	fields := strings.Fields(strings.TrimSpace(line[length:]))
	if len(fields) == 0 || fields[0] != "detent-status" {
		return 0, 0, false
	}
	return char, length, true
}

func statusFenceClosing(line string, char byte, length int) bool {
	if len(line) < length {
		return false
	}
	index := 0
	for index < len(line) && line[index] == char {
		index++
	}
	return index >= length && strings.TrimSpace(line[index:]) == ""
}
