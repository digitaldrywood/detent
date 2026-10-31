package issueorigin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

type Origin struct {
	Kind        string `yaml:"origin_kind"`
	Instance    string `yaml:"instance_identity"`
	Source      string `yaml:"source_ref"`
	Fingerprint string `yaml:"fingerprint"`
}

var block = regexp.MustCompile("(?m)^```detent-origin\\s*\\n([\\s\\S]*?)^```[ \\t]*$")
var audit = regexp.MustCompile(`<!--\s*detent-audit-fp[:=\s]+([^\s>]+)\s*-->`)

func Parse(body string) (Origin, bool) {
	for _, match := range block.FindAllStringSubmatch(body, -1) {
		var origin Origin
		if yaml.Unmarshal([]byte(match[1]), &origin) == nil && validKind(origin.Kind) && strings.TrimSpace(origin.Fingerprint) != "" {
			return origin, true
		}
	}
	if match := audit.FindStringSubmatch(body); len(match) > 1 {
		return Origin{Kind: "audit", Fingerprint: match[1]}, true
	}
	return Origin{}, false
}

func validKind(kind string) bool {
	switch kind {
	case "routine", "lesson", "worker", "doctor", "audit":
		return true
	default:
		return false
	}
}

func Instance() string {
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	return fmt.Sprintf("%s/%d", host, os.Getpid())
}

func Fingerprint(problem string) string {
	digest := sha256.Sum256([]byte(strings.Join(strings.Fields(strings.ToLower(problem)), " ")))
	return hex.EncodeToString(digest[:])
}

func Stamp(body string, origin Origin) string {
	if origin.Fingerprint == "" {
		if existing, ok := Parse(body); ok {
			origin.Fingerprint = existing.Fingerprint
		}
	}
	return stamp(body, origin)
}

func stamp(body string, origin Origin) string {
	if origin.Instance == "" {
		origin.Instance = Instance()
	}
	encoded, err := yaml.Marshal(origin)
	if err != nil {
		return body
	}
	body = strings.TrimSpace(block.ReplaceAllString(body, ""))
	return body + "\n\n```detent-origin\n" + string(encoded) + "```"
}

func Preserve(body, previous string) string {
	if origin, ok := Parse(previous); ok {
		return stamp(block.ReplaceAllString(body, ""), origin)
	}
	return body
}

func Marker(body string) string {
	if origin, ok := Parse(body); ok {
		return origin.Kind
	}
	return "operator"
}

func Occurrence(body string) string {
	return "## New machine occurrence\n\n" + strings.TrimSpace(body)
}
