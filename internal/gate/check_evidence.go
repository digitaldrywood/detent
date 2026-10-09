package gate

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
)

const CheckEvidencePrefix = "detent-check-evidence: "
const ScheduledEvidenceFence = "```detent-checks\n"

type CheckEnvironment struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	GoVersion    string `json:"go_version"`
}

type CheckObservation struct {
	Scope                string           `json:"scope"`
	Command              string           `json:"command"`
	HeadSHA              string           `json:"head_sha"`
	TreeSHA              string           `json:"tree_sha"`
	Environment          CheckEnvironment `json:"environment"`
	ExitCode             int              `json:"exit_code"`
	StartedAt            time.Time        `json:"started_at"`
	FinishedAt           time.Time        `json:"finished_at"`
	DurationNS           int64            `json:"duration_ns"`
	DurationResolutionNS int64            `json:"duration_resolution_ns"`
}

type CommandEvidence struct {
	Environment CheckEnvironment   `json:"environment"`
	Checks      []CheckObservation `json:"checks"`
}

type ScheduledEvidence struct {
	Schema        int                `json:"schema"`
	Repository    string             `json:"repository"`
	OccurrenceKey string             `json:"occurrence_key"`
	RunID         string             `json:"run_id"`
	RunAttempt    string             `json:"run_attempt"`
	JobID         string             `json:"job_id"`
	JobName       string             `json:"job_name"`
	RunURL        string             `json:"run_url"`
	JobURL        string             `json:"job_url"`
	HeadSHA       string             `json:"head_sha"`
	Conclusion    string             `json:"conclusion"`
	StartedAt     time.Time          `json:"started_at,omitzero"`
	FinishedAt    time.Time          `json:"finished_at,omitzero"`
	Checks        []CheckObservation `json:"checks"`
}

var goToolchainVersion = regexp.MustCompile(`^go[0-9]+\.[0-9]+(\.[0-9]+)?((beta|rc)[0-9]+)?$`)
var checkScope = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{0,127}$`)
var publicCommand = regexp.MustCompile(`^[a-zA-Z0-9_. /:+,=-]{1,256}$`)
var privateCommandArgument = regexp.MustCompile(`(?i)(token|secret|password|credential|api_key)`)
var repositoryIdentity = regexp.MustCompile(`^[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+$`)
var decimalPattern = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var evidenceFingerprint = regexp.MustCompile(`^[a-f0-9]{64}$`)
var evidenceSHA = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func (e CheckEnvironment) Valid() bool {
	return (e.OS == "" || slices.Contains([]string{"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios", "js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows"}, e.OS)) &&
		(e.Architecture == "" || slices.Contains([]string{"386", "amd64", "arm", "arm64", "loong64", "mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le", "riscv64", "s390x", "wasm"}, e.Architecture)) &&
		(e.GoVersion == "" || goToolchainVersion.MatchString(e.GoVersion))
}

func (e CheckEnvironment) Known() bool {
	return e.Valid() && e.OS != "" && e.Architecture != "" && e.GoVersion != ""
}

func validCheck(c CheckObservation) bool {
	return evidenceSHA.MatchString(c.HeadSHA) && evidenceSHA.MatchString(c.TreeSHA) && validCheckTiming(c)
}

func validCheckTiming(c CheckObservation) bool {
	if (c.HeadSHA != "" && !evidenceSHA.MatchString(c.HeadSHA)) || (c.TreeSHA != "" && !evidenceSHA.MatchString(c.TreeSHA)) || !c.Environment.Valid() || c.ExitCode < 0 || c.ExitCode > 255 || c.StartedAt.IsZero() || c.FinishedAt.Before(c.StartedAt) || c.DurationNS < 0 || c.DurationResolutionNS < 0 {
		return false
	}
	if !checkScope.MatchString(c.Scope) || !publicCommand.MatchString(c.Command) || privateCommandArgument.MatchString(c.Command) {
		return false
	}
	for _, argument := range strings.Fields(c.Command) {
		if strings.HasPrefix(argument, "/") || strings.Contains(argument, ":/") || strings.Contains(argument, "=/") || strings.Contains(argument, "../") {
			return false
		}
	}
	return true
}

func CheckObservations(output string) []CheckObservation {
	return checkObservations(output, validCheck)
}

func CheckTimings(output string) []CheckObservation {
	return checkObservations(output, validCheckTiming)
}

func checkObservations(output string, valid func(CheckObservation) bool) []CheckObservation {
	checks := []CheckObservation{}
	for _, line := range strings.Split(output, "\n") {
		prefix, raw, ok := strings.Cut(line, CheckEvidencePrefix)
		if !ok || len(raw) > 4096 {
			continue
		}
		if prefix = strings.TrimSpace(prefix); prefix != "" {
			if _, err := time.Parse(time.RFC3339Nano, prefix); err != nil {
				continue
			}
		}
		var check CheckObservation
		if json.Unmarshal([]byte(raw), &check) == nil && valid(check) && len(checks) < 32 {
			checks = append(checks, check)
		}
	}
	return checks
}

func (e *CommandEvidence) Valid() bool {
	if e == nil {
		return true
	}
	if !e.Environment.Valid() || len(e.Checks) > 32 {
		return false
	}
	for _, c := range e.Checks {
		if !validCheck(c) {
			return false
		}
	}
	return true
}

func (e ScheduledEvidence) Stamp() string {
	raw, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	return "\n\n" + ScheduledEvidenceFence + string(raw) + "\n```"
}

func ParseScheduledEvidence(body string) []ScheduledEvidence {
	result := []ScheduledEvidence{}
	for len(result) < 16 {
		_, rest, ok := strings.Cut(body, ScheduledEvidenceFence)
		if !ok {
			break
		}
		raw, tail, ok := strings.Cut(rest, "\n```")
		if !ok {
			break
		}
		body = tail
		if len(raw) > 32*1024 {
			continue
		}
		var e ScheduledEvidence
		if json.Unmarshal([]byte(raw), &e) != nil || e.Schema != 1 || !evidenceSHA.MatchString(e.HeadSHA) || len(e.Checks) > 32 || !repositoryIdentity.MatchString(e.Repository) || !evidenceFingerprint.MatchString(e.OccurrenceKey) || !decimalIdentity(e.RunID) || !decimalIdentity(e.RunAttempt) || (e.JobID != "0" && !decimalIdentity(e.JobID)) || len(e.JobName) > 256 || !slices.Contains([]string{"failure", "cancelled", "timed_out", "skipped", "action_required", "startup_failure", "neutral", ""}, e.Conclusion) {
			continue
		}
		expectedRunURL := "https://github.com/" + e.Repository + "/actions/runs/" + e.RunID + "/attempts/" + e.RunAttempt
		if e.RunURL != expectedRunURL {
			continue
		}
		expectedJobURL := "https://github.com/" + e.Repository + "/actions/jobs/" + e.JobID
		if e.JobURL != expectedJobURL {
			e.JobURL = ""
		}
		valid := true
		for _, c := range e.Checks {
			if !validCheck(c) || c.HeadSHA != e.HeadSHA {
				valid = false
			}
		}
		if valid {
			result = append(result, e)
		}
	}
	return result
}

func decimalIdentity(v string) bool {
	return decimalPattern.MatchString(v)
}

type CheckComparison struct {
	Outcome     string `json:"outcome"`
	Content     string `json:"content"`
	Scope       string `json:"scope"`
	Environment string `json:"environment"`
	Chronology  string `json:"chronology"`
}

func CompareCheck(local *CommandResult, observedAt time.Time, scheduled ScheduledEvidence, failed *CheckObservation) CheckComparison {
	c := CheckComparison{Outcome: "unknown", Content: "unknown", Scope: "unknown", Environment: "unknown", Chronology: "unknown"}
	if local == nil {
		c.Outcome = "missing_local_evidence"
		return c
	}
	if local.ExitCode < 0 {
		return c
	}
	if local.ExitCode != 0 {
		c.Outcome = "local_nonzero"
		return c
	}
	if failed == nil {
		return c
	}
	if evidenceSHA.MatchString(local.TreeSHA) {
		c.Content = "different"
		if local.TreeSHA == failed.TreeSHA {
			c.Content = "same"
		}
	}
	if c.Content == "different" {
		c.Outcome = "different_content"
		return c
	}
	if local.Evidence == nil {
		return c
	}
	for _, check := range local.Evidence.Checks {
		if check.HeadSHA != local.HeadSHA || check.TreeSHA != local.TreeSHA {
			continue
		}
		c.Scope = "different"
		if check.Scope != failed.Scope || check.Command != failed.Command {
			continue
		}
		c.Scope = "same"
		if check.ExitCode != 0 {
			c.Outcome = "local_nonzero"
			return c
		}
		if check.Environment.Known() && failed.Environment.Known() {
			c.Environment = "different"
			if check.Environment == failed.Environment {
				c.Environment = "same"
			}
		}
		if !observedAt.IsZero() && !failed.StartedAt.IsZero() && !check.FinishedAt.After(observedAt) && observedAt.Before(failed.StartedAt) {
			c.Chronology = "local_before_scheduled"
		}
		break
	}
	switch {
	case c.Scope == "different":
		c.Outcome = "different_check_scope"
	case c.Environment == "different":
		c.Outcome = "different_environment"
	case c.Content == "same" && c.Scope == "same" && c.Environment == "same" && c.Chronology == "local_before_scheduled" && failed.ExitCode > 0 && scheduled.Conclusion == "failure":
		c.Outcome = "local_pass_scheduled_failure"
	}
	return c
}
