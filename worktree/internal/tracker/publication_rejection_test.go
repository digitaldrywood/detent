package tracker

import (
	"strings"
	"testing"
)

func TestTerminalPublicationRejectionAllowlist(t *testing.T) {
	for _, test := range []struct {
		name      string
		rejection PublicationRejection
		valid     bool
	}{
		{"acceptance", PublicationRejection{Category: "acceptance_validation", Field: "validator.version_id"}, true},
		{"security audit", PublicationRejection{Category: "security_audit", Field: "execution_session"}, true},
		{"security audit reviewer digest", PublicationRejection{Category: "security_audit", Field: "run.reviewer_digest"}, true},
		{"event", PublicationRejection{Category: "event_validation", Field: "data.outcome"}, true},
		{"runtime", PublicationRejection{Category: "runtime_validation", Field: "data.runtime.activity"}, true},
		{"execution", PublicationRejection{Category: "execution_validation", Field: "data.finalization"}, true},
		{"owner", PublicationRejection{Category: "owner_validation", Field: "data.finalization"}, true},
		{"usage", PublicationRejection{Category: "usage_validation", Field: "data.usage"}, true},
		{"decode", PublicationRejection{Category: "body_decode"}, true},
		{"security audit private field", PublicationRejection{Category: "security_audit", Field: "private-payload"}, false},
		{"private category", PublicationRejection{Category: "private-payload", Field: "validator.version_id"}, false},
		{"private field", PublicationRejection{Category: "acceptance_validation", Field: "private-payload token=credential"}, false},
		{"other publication", PublicationRejection{Category: "version_validation", Field: "source"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := (NativeTerminalFailure{Rejection: &test.rejection, Error: "generic failure"}).Public()
			if (got.Rejection != nil) != test.valid {
				t.Fatalf("rejection=%+v", got.Rejection)
			}
			if strings.Contains(got.Error, "private-payload") || strings.Contains(got.Error, "credential") {
				t.Fatalf("private rejection leaked: %+v", got)
			}
			if test.valid {
				parsed, ok := ParsePublicationRejection(got.Error)
				if !ok || parsed != test.rejection || got.Stage != test.rejection.TerminalStage() || got.Stage == "" {
					t.Fatalf("rejection lost: %+v", got)
				}
				again := got.Public()
				if again.Rejection == nil || *again.Rejection != *got.Rejection || again.Error != got.Error {
					t.Fatalf("repeated redaction changed safe rejection: %+v", again)
				}
			}
		})
	}
}
