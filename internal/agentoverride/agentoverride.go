package agentoverride

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

type Override struct {
	Model  string
	Effort string
}

type blockYAML struct {
	Schema int    `yaml:"schema"`
	Model  string `yaml:"model"`
	Effort string `yaml:"effort"`
}

func FromIssueBody(body string) (Override, bool, error) {
	content, ok := lastBlock(body)
	if !ok {
		return Override{}, false, nil
	}

	var raw blockYAML
	decoder := yaml.NewDecoder(strings.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&raw); err != nil {
		return Override{}, true, fmt.Errorf("parse detent-agent YAML: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return Override{}, true, errors.New("parse detent-agent YAML: multiple YAML documents are not supported")
	} else if !errors.Is(err, io.EOF) {
		return Override{}, true, errors.New("parse detent-agent YAML: multiple YAML documents are not supported")
	}
	if raw.Schema != 1 {
		return Override{}, true, errors.New("detent-agent schema must be 1")
	}

	return Override{
		Model:  strings.TrimSpace(raw.Model),
		Effort: strings.TrimSpace(raw.Effort),
	}, true, nil
}

func lastBlock(body string) (string, bool) {
	var last string
	found := false
	inFence := false
	fenceChar := byte(0)
	fenceLen := 0
	lines := []string{}

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !inFence {
			char, length, ok := fenceOpening(trimmed)
			if !ok {
				continue
			}
			inFence = true
			fenceChar = char
			fenceLen = length
			lines = lines[:0]
			continue
		}
		if fenceClosing(trimmed, fenceChar, fenceLen) {
			last = strings.Join(lines, "\n")
			found = true
			inFence = false
			continue
		}
		lines = append(lines, line)
	}

	return last, found
}

func fenceOpening(line string) (byte, int, bool) {
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
	if len(fields) == 0 || fields[0] != "detent-agent" {
		return 0, 0, false
	}
	return char, length, true
}

func fenceClosing(line string, char byte, length int) bool {
	if len(line) < length {
		return false
	}
	index := 0
	for index < len(line) && line[index] == char {
		index++
	}
	return index >= length && strings.TrimSpace(line[index:]) == ""
}
