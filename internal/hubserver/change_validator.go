package hubserver

import (
	"net/http"
	"strings"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
)

func securityAuditRejection(message, field string) error {
	rejection := runnerauth.PublicationRejection{Category: "security_audit", Field: field}
	if !rejection.Valid() {
		rejection.Field = "review"
	}
	return &nativeError{Code: "invalid_request", Message: message, status: http.StatusUnprocessableEntity, Details: map[string]any{"rejection": rejection}}
}

func securityAuditEvaluationField(evaluation securityaudit.Evaluation) string {
	if evaluation.Reason == securityaudit.ReasonDiffTooLarge {
		return "run.diff_bytes"
	}
	field, _, _ := strings.Cut(evaluation.Detail, " ")
	if (runnerauth.PublicationRejection{Category: "security_audit", Field: field}).Valid() {
		return field
	}
	return "run"
}
