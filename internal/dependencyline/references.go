package dependencyline

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var referencePattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)

func CanonicalReference(value, repository string) (string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "https://github.com/") {
		value = strings.Replace(strings.TrimPrefix(value, "https://github.com/"), "/issues/", "#", 1)
	}
	if strings.HasPrefix(value, "#") {
		value = repository + value
	}
	match := referencePattern.FindStringSubmatch(value)
	if len(match) != 3 {
		return "", fmt.Errorf("invalid dependency reference %q", value)
	}
	if _, err := strconv.Atoi(match[2]); err != nil {
		return "", fmt.Errorf("invalid dependency number: %w", err)
	}
	return strings.ToLower(value), nil
}

// Declarations returns dependency text outside fenced examples. An unfinished
// fence returns false so callers that append declarations cannot hide new text.
func Declarations(body string) ([]string, bool) {
	var declarations []string
	fence := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			if fence == "" {
				end := len(trimmed) - len(strings.TrimLeft(trimmed, trimmed[:1]))
				fence = trimmed[:end]
			} else if len(trimmed) >= len(fence) && strings.Trim(trimmed, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		text, ok := Match(line)
		if !ok {
			continue
		}

		declarations = append(declarations, text)
	}
	if fence != "" {
		return declarations, false
	}
	return declarations, true
}

func References(body, repository string) ([]string, error) {
	declarations, complete := Declarations(body)
	if !complete {
		return nil, errors.New("unterminated code fence hides dependency declarations")
	}
	var refs []string
	for _, text := range declarations {
		for _, value := range strings.FieldsFunc(text, func(r rune) bool { return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\r' }) {
			ref, err := CanonicalReference(strings.Trim(value, "`"), repository)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	return refs, nil
}

func Append(body, repository, reference string) (string, error) {
	ref, err := CanonicalReference(reference, repository)
	if err != nil {
		return "", err
	}
	refs, err := References(body, repository)
	if err != nil {
		return "", err
	}
	if slices.Contains(refs, ref) {
		return body, nil
	}
	return body + "\n\nDepends on: " + ref + "\n", nil
}
