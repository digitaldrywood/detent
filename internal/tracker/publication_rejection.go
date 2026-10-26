package tracker

import (
	"slices"
	"strings"
)

type PublicationRejection struct {
	Category string `json:"category"`
	Field    string `json:"field,omitempty"`
}

func (r PublicationRejection) Valid() bool {
	switch r.Category {
	case "acceptance_validation":
		return slices.Contains([]string{"review", "body", "policy.validator", "validator.version_id", "validator.submitted", "validator.session_id", "validator.repository", "validator.base_sha", "validator.head_sha", "validator.diff_digest", "validator.score", "validator.criteria_evidence", "decision", "source.identity", "implementation_review"}, r.Field)
	case "security_audit":
		return slices.Contains([]string{"review", "body", "decision", "verdict", "policy.security_audit", "version_id", "session_id", "repository", "base_sha", "head_sha", "diff_digest", "source.identity", "execution_session", "separate_session", "run", "run.id", "run.issue_id", "run.project_id", "run.repository", "run.pr_number", "run.base_sha", "run.head_sha", "run.reviewer_version", "run.reviewer_digest", "run.exit_status", "run.diff_bytes", "run.output_bytes", "run.output_digest", "run.invocation_id", "run.worker_pid", "run.worker_pgid", "run.worker_started_at", "run.started_at", "run.completed_at", "run.summary", "run.verdict", "key.base_sha", "key.head_sha"}, r.Field)
	case "event_validation":
		return slices.Contains([]string{"", "schema_version", "type", "data", "data.runtime", "data.outcome", "data.artifact_ids", "data.steps", "data.steps.validator", "data.steps.validation", "data.steps.security_audit", "data.steps.completion_body", "data.steps.source", "data.steps.payload_artifact", "data.steps.payload_attachment"}, r.Field)
	case "runtime_validation":
		return slices.Contains([]string{"data.runtime", "data.runtime.worker_launch", "data.runtime.pipeline", "data.runtime.validation", "data.runtime.completion", "data.runtime.phases", "data.runtime.identity", "data.runtime.activity", "data.runtime.landing", "data.runtime.rest", "data.runtime.github"}, r.Field)
	case "execution_validation":
		return slices.Contains([]string{"data", "data.runtime.worker_launch.policy_id", "data.terminal_failure", "data.finalization", "data.evidence", "data.completion_body", "data.disposition", "data.identity", "data.handoff"}, r.Field)
	case "usage_validation":
		return r.Field == "data.usage"
	case "owner_validation":
		return slices.Contains([]string{"data", "data.finalization", "data.runtime.landing", "data.sequence"}, r.Field)
	case "version_validation":
		return slices.Contains([]string{"validation", "source", "policy_id"}, r.Field)
	case "source_verification":
		return r.Field == "source"
	case "body_decode":
		return r.Field == "" || slices.Contains([]string{"schema_version", "type", "data", "data.sequence", "data.runtime", "data.runtime.activity", "data.runtime.validation"}, r.Field)
	default:
		return false
	}
}

func (r *PublicationRejection) TerminalStage() string {
	if r == nil || !r.Valid() {
		return ""
	}
	switch r.Category {
	case "acceptance_validation":
		return "acceptance_publication"
	case "security_audit":
		return "security_audit_publication"
	case "event_validation", "runtime_validation", "execution_validation", "owner_validation", "usage_validation", "body_decode":
		return "event_publication"
	}
	return ""
}

func (r PublicationRejection) DiagnosticError() string {
	if !r.Valid() {
		return "[private error text omitted]"
	}
	return "publication rejected: category=" + r.Category + " field=" + r.Field + " [private error text omitted]"
}

func ParsePublicationRejection(value string) (PublicationRejection, bool) {
	value, ok := strings.CutPrefix(value, "publication rejected: category=")
	if !ok {
		return PublicationRejection{}, false
	}
	value, ok = strings.CutSuffix(value, " [private error text omitted]")
	if !ok {
		return PublicationRejection{}, false
	}
	category, field, ok := strings.Cut(value, " field=")
	r := PublicationRejection{Category: category, Field: field}
	return r, ok && r.Valid()
}
