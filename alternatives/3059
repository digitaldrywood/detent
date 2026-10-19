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
	case "event_validation":
		return slices.Contains([]string{"", "schema_version", "type", "data", "data.runtime", "data.outcome", "data.artifact_ids"}, r.Field)
	case "runtime_validation":
		return slices.Contains([]string{"data.runtime", "data.runtime.pipeline", "data.runtime.validation", "data.runtime.completion", "data.runtime.phases", "data.runtime.identity", "data.runtime.activity", "data.runtime.landing", "data.runtime.rest", "data.runtime.github"}, r.Field)
	case "execution_validation":
		return slices.Contains([]string{"data", "data.terminal_failure", "data.finalization", "data.evidence", "data.completion_body", "data.disposition", "data.identity", "data.handoff"}, r.Field)
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
