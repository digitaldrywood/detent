package reviewseverity

import "strings"

func Contains(body string, severity string) bool {
	severity = strings.ToUpper(strings.TrimSpace(severity))
	if strings.TrimSpace(body) == "" || severity == "" {
		return false
	}
	bodyUpper := strings.ToUpper(body)
	if strings.Contains(bodyUpper, "["+severity+"]") {
		return true
	}
	for line := range strings.SplitSeq(body, "\n") {
		if lineContains(strings.ToUpper(trimLinePrefix(line)), severity) {
			return true
		}
	}
	return false
}

func BodySeverity(body string) string {
	if Contains(body, "P1") {
		return "P1"
	}
	if Contains(body, "P2") {
		return "P2"
	}
	return ""
}

func trimLinePrefix(line string) string {
	line = strings.TrimSpace(line)
	for {
		trimmed := strings.TrimSpace(trimLineMarker(line))
		if trimmed == line {
			return line
		}
		line = trimmed
	}
}

func trimLineMarker(line string) string {
	if strings.HasPrefix(line, "#") {
		index := 0
		for index < len(line) && line[index] == '#' {
			index++
		}
		if index < len(line) && isWhitespace(line[index]) {
			return line[index+1:]
		}
	}
	if len(line) >= 2 {
		switch line[0] {
		case '-', '*', '+':
			if isWhitespace(line[1]) {
				return line[2:]
			}
		}
	}
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index > 0 && index+1 < len(line) && (line[index] == '.' || line[index] == ')') && isWhitespace(line[index+1]) {
		return line[index+2:]
	}
	return line
}

func lineContains(line string, severity string) bool {
	if strings.HasPrefix(line, severity+":") {
		return true
	}
	badge := severity + " BADGE"
	return strings.HasPrefix(line, badge) && tokenBoundary(line, len(badge))
}

func tokenBoundary(line string, index int) bool {
	if index >= len(line) {
		return true
	}
	char := line[index]
	return (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_'
}

func isWhitespace(char byte) bool {
	return char == ' ' || char == '\t'
}
