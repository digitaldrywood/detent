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
	ReviewerVersion             = "2"
	MaxDispositionEvidenceBytes = 16 * 1024

	AuthenticationSubscription = "chatgpt_subscription"
	AuthenticationRejected     = "non_subscription"
	AuthenticationNotRun       = "not_run"

	ExitStatusSuccess = "success"
	ExitStatusFailed  = "failed"

	VerdictPass = "pass"
	VerdictFail = "fail"

	DispositionFalsePositive = "false_positive"

	ReasonReady              = "ready"
	ReasonMissing            = "missing"
	ReasonStale              = "stale"
	ReasonFailed             = "failed"
	ReasonDiffTooLarge       = "diff_too_large"
	ReasonEmptyOutput        = "empty_output"
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
	Status   string `json:"status,omitempty"`
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
	AllFindings  []Finding
	Running      bool
	Allowed      bool
	Reason       string
	Detail       string
	RunID        int64
	Findings     []Finding
	ActualBytes  int
	MaxDiffBytes int
}

func ReviewerDigest() string {
	return reviewerDigest(trustedReviewerInstructions+"\x00"+chunkInstructions, trustedToolInstructions)
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
		evaluation.Detail = "run.id is missing or invalid"
		return evaluation
	}
	if detail := keyMismatch(run, key); detail != "" {
		evaluation.Detail = detail
		evaluation.Reason = ReasonStale
		return evaluation
	}
	if run.ReviewerVersion != ReviewerVersion || run.ReviewerDigest != ReviewerDigest() {
		evaluation.Reason = ReasonReviewerMismatch
		evaluation.Detail = "run.reviewer_version or run.reviewer_digest differs from the trusted reviewer"
		return evaluation
	}
	if size, ok := ParseDiffTooLargeFailure(run.Failure); ok && run.ExitStatus == ExitStatusFailed {
		evaluation.Reason = ReasonDiffTooLarge
		evaluation.ActualBytes = size.ActualBytes
		evaluation.MaxDiffBytes = size.LimitBytes
		return evaluation
	}
	if run.ExitStatus != ExitStatusSuccess {
		evaluation.Detail = "run.exit_status is not success"
		evaluation.Reason = ReasonFailed
		return evaluation
	}
	if run.OutputBytes <= 0 || strings.TrimSpace(run.OutputDigest) == "" {
		evaluation.Reason = ReasonEmptyOutput
		if run.OutputBytes <= 0 {
			evaluation.Detail = "run.output_bytes is missing or invalid"
		} else {
			evaluation.Detail = "run.output_digest is missing"
		}
		return evaluation
	}
	if detail := successfulRunFailure(run); detail != "" {
		evaluation.Detail = detail
		evaluation.Reason = ReasonFailed
		return evaluation
	}

	evaluation.AllFindings = run.Findings
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

func (evaluation Evaluation) Diagnostic() string {
	if evaluation.Detail == "" {
		return evaluation.Reason
	}
	return evaluation.Reason + ": " + evaluation.Detail
}

func successfulRunFailure(run Run) string {
	switch {
	case strings.TrimSpace(run.InvocationID) == "":
		return "run.invocation_id is missing"
	case run.WorkerPID <= 0:
		return "run.worker_pid is missing or invalid"
	case run.WorkerPGID <= 0:
		return "run.worker_pgid is missing or invalid"
	case run.WorkerStartedAt.IsZero():
		return "run.worker_started_at is missing"
	case run.StartedAt.IsZero():
		return "run.started_at is missing"
	case run.CompletedAt.IsZero():
		return "run.completed_at is missing"
	case run.CompletedAt.Before(run.StartedAt):
		return "run.completed_at precedes run.started_at"
	case len(strings.TrimSpace(run.OutputDigest)) != sha256.Size*2:
		return "run.output_digest is invalid"
	case strings.TrimSpace(run.Summary) == "":
		return "run.summary is missing"
	}
	if _, err := hex.DecodeString(strings.TrimSpace(run.OutputDigest)); err != nil {
		return "run.output_digest is invalid"
	}
	switch run.Verdict {
	case VerdictPass:
		if len(unresolvedFindings(run.Findings, nil, "", []string{"p1", "p2", "p3"})) == 0 {
			return ""
		}
	case VerdictFail:
		if len(unresolvedFindings(run.Findings, nil, "", []string{"p1", "p2", "p3"})) > 0 {
			return ""
		}
	}
	return "run.verdict is invalid or inconsistent with run.findings"
}

func keyMismatch(run Run, key Key) string {
	if strings.TrimSpace(key.BaseSHA) == "" {
		return "key.base_sha is missing"
	}
	if strings.TrimSpace(key.HeadSHA) == "" {
		return "key.head_sha is missing"
	}
	switch {
	case strings.TrimSpace(run.ProjectID) != strings.TrimSpace(key.ProjectID):
		return "run.project_id differs from key.project_id"
	case !strings.EqualFold(strings.TrimSpace(run.Repository), strings.TrimSpace(key.Repository)):
		return "run.repository differs from key.repository"
	case run.PRNumber != key.PRNumber:
		return "run.pr_number differs from key.pr_number"
	case strings.TrimSpace(run.BaseSHA) != strings.TrimSpace(key.BaseSHA):
		return "run.base_sha differs from key.base_sha"
	case strings.TrimSpace(run.HeadSHA) != strings.TrimSpace(key.HeadSHA):
		return "run.head_sha differs from key.head_sha"
	default:
		return ""
	}
}

// BlockingFindings returns the findings whose severity blocks under blockOn,
// ignoring dispositions.
func BlockingFindings(findings []Finding, blockOn []string) []Finding {
	return unresolvedFindings(findings, nil, "", blockOn)
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
		if finding.Status == "resolved" {
			continue
		}
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
