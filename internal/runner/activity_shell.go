package runner

import (
	"path/filepath"
	"strings"
)

// activityShellCommand decodes one native shell launcher for activity evidence.
// It never evaluates shell text. Unsupported launchers have no usable command;
// ordinary commands retain the existing classifier's handling.
func activityShellCommand(command string) string {
	first := strings.TrimSpace(command)
	if index := strings.IndexAny(first, " \t\r\n"); index >= 0 {
		first = first[:index]
	}
	switch filepath.Base(strings.Trim(first, "\"'")) {
	case "sh", "bash", "zsh":
	default:
		return command
	}
	words, ok := activityLiteralWords(command, 3)
	if !ok || len(words) != 3 {
		return ""
	}
	switch words[0] {
	case "/bin/sh", "/bin/bash", "/bin/zsh", "/usr/bin/sh", "/usr/bin/bash", "/usr/bin/zsh":
	default:
		return ""
	}
	switch words[1] {
	case "-c", "-lc", "-cl":
	default:
		return ""
	}
	// Keep expanding/opaque scripts uncertain too. Mixed literal commands still
	// reach the existing segment classifier, which does not attribute them.
	for _, segment := range shellCommandSegments(words[2]) {
		fields, ok := activityLiteralWords(segment, 128)
		if !ok || len(fields) == 0 {
			return ""
		}
		switch filepath.Base(fields[0]) {
		case "sh", "bash", "zsh":
			return "" // Do not recursively decode nested launchers.
		}
	}
	return words[2]
}

// activityLiteralWords accepts bounded literal shell words, including adjacent
// quoted fragments and escaped characters. Expansions, operators, comments and
// incomplete quoting are deliberately outside this parser's vocabulary.
func activityLiteralWords(command string, limit int) ([]string, bool) {
	if len(command) > 8192 || strings.IndexByte(command, 0) >= 0 {
		return nil, false
	}
	var words []string
	for index := 0; index < len(command); {
		if command[index] == ' ' || command[index] == '\t' {
			index++
			continue
		}
		if len(words) == limit {
			return nil, false
		}
		var word strings.Builder
		quote := byte(0)
		for index < len(command) {
			current := command[index]
			if quote == 0 && (current == ' ' || current == '\t') {
				break
			}
			index++
			if current == quote {
				quote = 0
				continue
			}
			if quote == '\'' {
				word.WriteByte(current)
				continue
			}
			if current == '\\' {
				if index == len(command) || command[index] == '\n' || command[index] == '\r' {
					return nil, false
				}
				next := command[index]
				if quote == '"' && next != '$' && next != '`' && next != '"' && next != '\\' {
					word.WriteByte(current) // Backslash is literal here in double quotes.
				} else {
					word.WriteByte(next)
					index++
				}
				continue
			}
			if current == '$' || current == '`' {
				return nil, false
			}
			if quote == 0 {
				if current == '\'' || current == '"' {
					quote = current
					continue
				}
				if strings.ContainsRune("\r\n;&|<>(){}*?[]~#", rune(current)) {
					return nil, false
				}
			}
			word.WriteByte(current)
		}
		if quote != 0 {
			return nil, false
		}
		words = append(words, word.String())
	}
	return words, true
}
