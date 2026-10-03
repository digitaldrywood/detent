package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"regexp"
	"strings"
)

type problem struct {
	Key      string
	Summary  string
	Evidence string
}

var (
	ansiEscape      = regexp.MustCompile(`\x1b\[[0-9;]*m`)
	logTime         = regexp.MustCompile(`^\d{4}-\d\d-\d\dT[0-9:.]+Z\s+`)
	testFail        = regexp.MustCompile(`^\s*--- FAIL: (\S+) \(`)
	packageEnd      = regexp.MustCompile(`^FAIL\s+(\S+)`)
	diagnostic      = regexp.MustCompile(`^(\S+\.go):(\d+):(\d+):\s+(.+)$`)
	gosecDiagnostic = regexp.MustCompile(`^\[(\S+\.go):(\d+)\] - (G\d{3} \(CWE-\d+\): .+)$`)
	assertion       = regexp.MustCompile(`(?m)^\s+\S+\.go:\d+:`)
)

// parseProblems accepts both ordinary go test output and interleaved -json
// events. Text test names are qualified only by their package's FAIL summary;
// truncated or ambiguous output deliberately falls back to job identity.
func parseProblems(log, workspace string) []problem {
	var problems []problem
	var pending []problem
	seen := map[string]bool{}
	outputs := map[string]string{}
	add := func(p problem) {
		if !seen[p.Key] {
			seen[p.Key] = true
			problems = append(problems, p)
		}
	}
	for raw := range strings.SplitSeq(log, "\n") {
		line := strings.TrimSuffix(logTime.ReplaceAllString(ansiEscape.ReplaceAllString(raw, ""), ""), "\r")
		var event struct {
			Action  string
			Package string
			Test    string
			Output  string
		}
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &event) == nil && event.Package != "" {
			key := event.Package + ":" + event.Test
			if event.Action == "output" && event.Test != "" {
				outputs[key] = diagnosticExcerpt(outputs[key] + event.Output)
			}
			if event.Action == "fail" && event.Test != "" {
				add(testProblem(event.Package, event.Test, outputs[key]+line))
			}
			if event.Action == "pass" || event.Action == "fail" {
				delete(outputs, key)
			}
			// Package-attributed JSON output cannot join the text parser's
			// pending tests: other package events may be interleaved.
			if event.Action == "output" && event.Test == "" {
				if p, ok := sourceProblem(strings.TrimSpace(event.Output), workspace); ok {
					add(p)
				}
			}
			continue
		}
		if match := testFail.FindStringSubmatch(line); match != nil {
			pending = append(pending, problem{Key: match[1], Evidence: line})
			continue
		}
		if match := packageEnd.FindStringSubmatch(line); match != nil {
			for _, p := range pending {
				add(testProblem(match[1], p.Key, p.Evidence))
			}
			pending = nil
			continue
		}
		if len(pending) > 0 {
			last := &pending[len(pending)-1]
			last.Evidence = diagnosticExcerpt(last.Evidence + "\n" + line)
		} else if p, ok := sourceProblem(line, workspace); ok {
			add(p)
		}
	}
	// A parent with no assertion of its own summarizes its failed subtests.
	// Retain a parent that also reported an independent assertion failure.
	var leaves []problem
	for _, p := range problems {
		parent := false
		if strings.HasPrefix(p.Key, "go-test:") {
			for _, other := range problems {
				if strings.HasPrefix(other.Key, p.Key+"/") && !assertion.MatchString(p.Evidence) {
					parent = true
					break
				}
			}
		}
		if !parent {
			leaves = append(leaves, p)
		}
	}
	return leaves
}

func testProblem(pkg, name, evidence string) problem {
	return problem{Key: "go-test:" + pkg + ":" + name, Summary: name + " failed in " + pkg, Evidence: evidence}
}

func sourceProblem(line, workspace string) (problem, bool) {
	match := diagnostic.FindStringSubmatch(line)
	if match == nil {
		match = gosecDiagnostic.FindStringSubmatch(line)
		if match == nil {
			return problem{}, false
		}
		match = []string{match[0], match[1], match[2], "", match[3]}
	}
	if workspace != "" {
		match[1] = strings.TrimPrefix(match[1], strings.TrimRight(workspace, "/")+"/")
	}
	match[1] = path.Clean(match[1])
	// Absolute runner/tool-cache paths are not a repository-stable identity.
	if strings.HasPrefix(match[1], "/") || strings.ContainsAny(match[1], `\:`) || strings.HasPrefix(match[1], "../") {
		return problem{}, false
	}
	return problem{Key: "go-diagnostic:" + match[1] + ":" + match[2] + ":" + match[3] + ":" + match[4], Summary: line, Evidence: line}, true
}

// Keep issue/comment bodies well within GitHub's limit even for verbose tests.
func diagnosticExcerpt(evidence string) string {
	const limit = 8000
	if len(evidence) > limit {
		return "[earlier output truncated]\n" + evidence[len(evidence)-limit:]
	}
	return evidence
}

func legacyJobFingerprint(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}
