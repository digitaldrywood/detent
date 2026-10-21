package projectcolor

import (
	"hash/fnv"
	"strings"
)

var palette = []string{
	"#0072b2",
	"#d55e00",
	"#009e73",
	"#cc79a7",
	"#5a6fda",
	"#b45f06",
	"#6f4e7c",
	"#00857c",
	"#b0005b",
	"#33691e",
}

func Normalize(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) != 4 && len(value) != 7 {
		return "", false
	}
	if value[0] != '#' {
		return "", false
	}
	for _, r := range value[1:] {
		if !hexDigit(r) {
			return "", false
		}
	}
	value = strings.ToLower(value)
	if len(value) == 7 {
		return value, true
	}
	return "#" + string([]byte{value[1], value[1], value[2], value[2], value[3], value[3]}), true
}

func ColorFor(projectID string, configured string) string {
	if color, ok := Normalize(configured); ok {
		return color
	}
	return ColorForID(projectID)
}

func ColorForID(projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return palette[0]
	}
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(projectID))
	return palette[int(hash.Sum32()%uint32(len(palette)))]
}

func hexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}
