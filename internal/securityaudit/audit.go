package securityaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"time"
)

const (
	ReviewerVersion             = "1"
	MaxDispositionEvidenceBytes = 16 * 1024

	AuthenticationSubscription = "chatgpt_subscription"
	AuthenticationRejected     = "non_subscription"

	ExitStatusSuccess = "success"
	ExitStatusFailed  = "failed"

	VerdictPass = "pass"
	VerdictFail = "fail"

	DispositionFalsePositive = "false_positive"

	ReasonReady              = "ready"
	ReasonMissing            = "missing"
	ReasonStale              = "stale"
	ReasonFailed             = "failed"
	ReasonEmptyOutput        = "empty_output"
	ReasonMeteredAuth        = "metered_auth"
	ReasonUntrustedService   = "untrusted_service"
	ReasonReviewerMismatch   = "reviewer_mismatch"
	ReasonUnresolvedFindings = "unresolved_findings"
)

var ErrInvalidOutput = errors.New("invalid security audit output")

type Key struct {
	ProjectID  string
	Repository string
	PRNumber   int
	BaseSHA    string
	HeadSHA    string
}

type Finding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
}

type Result struct {
	Verdict  string    `json:"verdict"`
	Summary  string    `json:"summary"`
	Findings []Finding `json:"findings"`
}

type Run struct {
	ID                 int64
	InvocationID       string
	ProjectID          string
	IssueID            string
	Identifier         string
	IssueURL           string
	Repository         string
	PRNumber           int
	BaseSHA            string
	HeadSHA            string
	ServiceIdentity    string
	ReviewerVersion    string
	ReviewerDigest     string
	AuthenticationMode string
	WorkerPID          int
	WorkerPGID         int
	WorkerStartedAt    time.Time
	ProviderThreadID   string
	ProviderSessionID  string
	ExitStatus         string
	Failure            string
	OutputDigest       string
	OutputBytes        int
	Verdict            string
	Summary            string
	Findings           []Finding
	Attempt            int
	StartedAt          time.Time
	CompletedAt        time.Time
	RecordedAt         time.Time
}

type Disposition struct {
	ID              int64     `json:"id"`
	AuditRunID      int64     `json:"audit_run_id"`
	FindingID       string    `json:"finding_id"`
	Status          string    `json:"status"`
	Evidence        string    `json:"evidence"`
	ServiceIdentity string    `json:"service_identity"`
	RecordedAt      time.Time `json:"recorded_at"`
}

type Evaluation struct {
	Allowed  bool
	Reason   string
	RunID    int64
	Findings []Finding
}

func ReviewerDigest() string {
	return reviewerDigest(trustedReviewerInstructions, trustedToolInstructions)
}

func ServiceIdentity(projectID string) string {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return ""
	}
	return "detent:" + projectID
}

func reviewerDigest(reviewerInstructions, toolInstructions string) string {
	contract := "detent-security-audit-reviewer-v1\x00" + reviewerInstructions + "\x00" + toolInstructions
	sum := sha256.Sum256([]byte(contract))
	return hex.EncodeToString(sum[:])
}

func OutputDigest(output string) string {
	sum := sha256.Sum256([]byte(output))
	return hex.EncodeToString(sum[:])
}

func Evaluate(run Run, dispositions []Disposition, key Key, serviceIdentity string, blockOn []string) Evaluation {
	evaluation := Evaluation{RunID: run.ID}
	if run.ID <= 0 {
		evaluation.Reason = ReasonMissing
		return evaluation
	}
	if !sameKey(run, key) {
		evaluation.Reason = ReasonStale
		return evaluation
	}
	if strings.TrimSpace(run.ServiceIdentity) == "" || strings.TrimSpace(run.ServiceIdentity) != strings.TrimSpace(serviceIdentity) {
		evaluation.Reason = ReasonUntrustedService
		return evaluation
	}
	if run.ReviewerVersion != ReviewerVersion || run.ReviewerDigest != ReviewerDigest() {
		evaluation.Reason = ReasonReviewerMismatch
		return evaluation
	}
	if run.AuthenticationMode != AuthenticationSubscription {
		evaluation.Reason = ReasonMeteredAuth
		return evaluation
	}
	if run.ExitStatus != ExitStatusSuccess {
		evaluation.Reason = ReasonFailed
		return evaluation
	}
	if run.OutputBytes <= 0 || strings.TrimSpace(run.OutputDigest) == "" {
		evaluation.Reason = ReasonEmptyOutput
		return evaluation
	}
	if !validSuccessfulRun(run) {
		evaluation.Reason = ReasonFailed
		return evaluation
	}

	blocking := unresolvedFindings(run.Findings, dispositions, serviceIdentity, blockOn)
	if len(blocking) > 0 {
		evaluation.Reason = ReasonUnresolvedFindings
		evaluation.Findings = blocking
		return evaluation
	}

	evaluation.Allowed = true
	evaluation.Reason = ReasonReady
	return evaluation
}

func validSuccessfulRun(run Run) bool {
	if strings.TrimSpace(run.InvocationID) == "" || run.WorkerPID <= 0 || run.WorkerPGID <= 0 || run.WorkerStartedAt.IsZero() ||
		strings.TrimSpace(run.ProviderThreadID) == "" || strings.TrimSpace(run.ProviderSessionID) == "" ||
		run.StartedAt.IsZero() || run.CompletedAt.IsZero() || run.CompletedAt.Before(run.StartedAt) ||
		run.WorkerStartedAt.Before(run.StartedAt) || run.WorkerStartedAt.After(run.CompletedAt) ||
		len(strings.TrimSpace(run.OutputDigest)) != sha256.Size*2 || strings.TrimSpace(run.Summary) == "" {
		return false
	}
	if _, err := hex.DecodeString(strings.TrimSpace(run.OutputDigest)); err != nil {
		return false
	}
	switch run.Verdict {
	case VerdictPass:
		return len(run.Findings) == 0
	case VerdictFail:
		return len(run.Findings) > 0
	default:
		return false
	}
}

func sameKey(run Run, key Key) bool {
	return strings.TrimSpace(run.ProjectID) == strings.TrimSpace(key.ProjectID) &&
		strings.EqualFold(strings.TrimSpace(run.Repository), strings.TrimSpace(key.Repository)) &&
		run.PRNumber == key.PRNumber &&
		strings.TrimSpace(run.BaseSHA) == strings.TrimSpace(key.BaseSHA) &&
		strings.TrimSpace(run.HeadSHA) == strings.TrimSpace(key.HeadSHA)
}

func unresolvedFindings(findings []Finding, dispositions []Disposition, serviceIdentity string, blockOn []string) []Finding {
	blockedSeverities := normalizeSeverities(blockOn)
	latest := make(map[string]Disposition, len(dispositions))
	for _, disposition := range dispositions {
		if strings.TrimSpace(disposition.ServiceIdentity) != strings.TrimSpace(serviceIdentity) {
			continue
		}
		findingID := strings.TrimSpace(disposition.FindingID)
		if findingID == "" {
			continue
		}
		current, exists := latest[findingID]
		if !exists || disposition.RecordedAt.After(current.RecordedAt) || disposition.ID > current.ID {
			latest[findingID] = disposition
		}
	}

	unresolved := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		if !slices.Contains(blockedSeverities, strings.ToLower(strings.TrimSpace(finding.Severity))) {
			continue
		}
		disposition, exists := latest[strings.TrimSpace(finding.ID)]
		if exists && strings.EqualFold(strings.TrimSpace(disposition.Status), DispositionFalsePositive) && strings.TrimSpace(disposition.Evidence) != "" {
			continue
		}
		unresolved = append(unresolved, finding)
	}
	return unresolved
}

func normalizeSeverities(values []string) []string {
	if len(values) == 0 {
		values = []string{"p1", "p2"}
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || slices.Contains(out, value) {
			continue
		}
		out = append(out, value)
	}
	return out
}
