package failureattribution

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

const (
	Change      = "change"
	Independent = "independent"
	Unknown     = "unknown"
)

type Observation struct {
	Command  string `json:"command"`
	HeadSHA  string `json:"head_sha"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	Complete bool   `json:"complete"`
}

type Evidence struct {
	BaseSHA          string `json:"base_sha"`
	HeadSHA          string `json:"head_sha"`
	Command          string `json:"command"`
	Diagnostic       string `json:"diagnostic"`
	HeadOutputDigest string `json:"head_output_digest"`
	BaseOutputDigest string `json:"base_output_digest"`
	BaseExitCode     int    `json:"base_exit_code"`
}

var sourceSHA = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func Classify(baseSHA, headSHA, diagnostic string, head, base Observation) (string, *Evidence) {
	if !sourceSHA.MatchString(baseSHA) || !sourceSHA.MatchString(headSHA) || baseSHA == headSHA || head.HeadSHA != headSHA || base.HeadSHA != baseSHA || head.Command == "" || head.Command != base.Command || !head.Complete || !base.Complete || !checkExit(head.ExitCode) || !checkExit(base.ExitCode) || head.ExitCode == 0 || strings.TrimSpace(diagnostic) == "" || !containsDiagnostic(head.Output, diagnostic) {
		return Unknown, nil
	}
	attribution := Unknown
	if base.ExitCode != 0 && containsDiagnostic(base.Output, diagnostic) {
		attribution = Independent
	} else if base.ExitCode == 0 {
		attribution = Change
	}
	if attribution == Unknown {
		return Unknown, nil
	}
	return attribution, &Evidence{BaseSHA: baseSHA, HeadSHA: headSHA, Command: head.Command, Diagnostic: diagnostic, HeadOutputDigest: Digest(head.Output), BaseOutputDigest: Digest(base.Output), BaseExitCode: base.ExitCode}
}

func checkExit(code int) bool { return code >= 0 && code < 126 }

func Digest(output string) string {
	sum := sha256.Sum256([]byte(output))
	return hex.EncodeToString(sum[:])
}

func Valid(value string, evidence *Evidence, base, head string) bool {
	if value == "" || value == Unknown {
		return evidence == nil
	}
	if value != Change && value != Independent || evidence == nil || !sourceSHA.MatchString(base) || !sourceSHA.MatchString(head) || evidence.BaseSHA != base || evidence.HeadSHA != head || base == head || strings.TrimSpace(evidence.Command) == "" || strings.TrimSpace(evidence.Diagnostic) == "" || !digestValid(evidence.HeadOutputDigest) || !digestValid(evidence.BaseOutputDigest) {
		return false
	}
	return value == Change && evidence.BaseExitCode == 0 || value == Independent && evidence.BaseExitCode > 0 && checkExit(evidence.BaseExitCode)
}

func digestValid(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func Aggregate(values []string) string {
	if len(values) == 0 {
		return Unknown
	}
	result := Independent
	for _, value := range values {
		if value == Change {
			return Change
		}
		if value != Independent {
			result = Unknown
		}
	}
	return result
}

func containsDiagnostic(output, diagnostic string) bool {
	output = strings.ReplaceAll(output, "\r\n", "\n")
	diagnostic = strings.Trim(diagnostic, "\r\n")
	return strings.Contains("\n"+output+"\n", "\n"+diagnostic+"\n")
}
