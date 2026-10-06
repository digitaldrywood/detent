package issueorigin

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

const DetentCloudProjectID = "prj_6d4919bebd73446798e6cd807feda10e"

var migrationCollision = regexp.MustCompile(`(?i)(?:goose: duplicate version|duplicate migration version)\s+(\d+)`)
var failedTest = regexp.MustCompile(`(?m)^\s*--- FAIL: (Test\S+) \(`)
var failedPackage = regexp.MustCompile(`(?m)^FAIL\s+(\S+)`)
var sourceDiagnostic = regexp.MustCompile(`(?m)^\s*((?:[^\s:]+/)*[^\s:]+\.go):\d+:\d+:\s+([^\n]+)$`)
var gosecDiagnostic = regexp.MustCompile(`(?m)^\[([^\s:]+\.go):\d+\] - \((G\d{3} \(CWE-\d+\): .+)\)$`)
var scheduledDiagnostic = regexp.MustCompile("Problem: `go-diagnostic:([^`]+)`")
var scheduledTest = regexp.MustCompile("Problem: `go-test:([^`]+)`")

func DefectFingerprint(body string) string {
	keys := defectFingerprints(body)
	if len(keys) == 1 {
		return keys[0]
	}
	return ""
}

func HasDefect(body, fingerprint string) bool {
	return slices.Contains(defectFingerprints(body), fingerprint)
}

func SameDefect(previous, incoming string) bool {
	before, found := Parse(previous)
	after, reported := Parse(incoming)
	if !found || !reported {
		return false
	}
	if before.Fingerprint == after.Fingerprint {
		return true
	}
	if before.Kind != "worker" && after.Kind != "worker" {
		return false
	}
	for _, key := range defectFingerprints(incoming) {
		if before.Fingerprint == key || HasDefect(previous, key) {
			return true
		}
	}
	return HasDefect(previous, after.Fingerprint)
}

func defectFingerprints(body string) []string {
	body = block.ReplaceAllString(body, "")
	var keys []string
	for _, match := range migrationCollision.FindAllStringSubmatch(body, -1) {
		keys = append(keys, Fingerprint("goose:duplicate-version:"+match[1]))
	}
	for _, match := range scheduledTest.FindAllStringSubmatch(body, -1) {
		keys = append(keys, Fingerprint("go-test:"+match[1]))
	}
	tests := failedTest.FindAllStringSubmatch(body, -1)
	var packages []string
	for _, match := range failedPackage.FindAllStringSubmatch(body, -1) {
		packages = append(packages, match[1])
	}
	slices.Sort(packages)
	packages = slices.Compact(packages)
	if len(packages) == 1 {
		for _, test := range tests {
			parent := false
			for _, other := range tests {
				parent = parent || strings.HasPrefix(other[1], test[1]+"/")
			}
			if !parent {
				keys = append(keys, Fingerprint("go-test:"+packages[0]+":"+test[1]))
			}
		}
	}
	for _, match := range scheduledDiagnostic.FindAllStringSubmatch(body, -1) {
		if key := diagnosticFingerprint(match[1]); key != "" {
			keys = append(keys, key)
		}
	}
	for _, expression := range []*regexp.Regexp{sourceDiagnostic, gosecDiagnostic} {
		for _, match := range expression.FindAllStringSubmatch(body, -1) {
			if key := diagnosticFingerprint(match[1] + ":0:0:" + strings.TrimSpace(match[2])); key != "" {
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

func diagnosticFingerprint(diagnostic string) string {
	parts := strings.SplitN(diagnostic, ":", 4)
	if len(parts) != 4 {
		return ""
	}
	file := path.Clean(parts[0])
	message := strings.TrimSpace(parts[3])
	if !strings.HasSuffix(file, ".go") || strings.HasPrefix(file, "/") || strings.HasPrefix(file, "../") || strings.ContainsAny(file, `\:`) || message == "" {
		return ""
	}
	return Fingerprint("go-diagnostic:" + file + ":" + message)
}
