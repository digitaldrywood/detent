package tracker

import "testing"

func TestPublicationFailureRedactsUntrustedFields(t *testing.T) {
	for _, field := range []string{"operation", "code", "status", "request", "body", "output"} {
		t.Run(field, func(t *testing.T) {
			failure := PublicationFailure{Operation: "security_audit/review", Code: "http_rejection", HTTPStatus: 413, RequestBytes: 1024}
			switch field {
			case "operation":
				failure.Operation = "secret"
			case "code":
				failure.Code = "secret"
			case "status":
				failure.HTTPStatus = 123456
			case "request":
				failure.RequestBytes = -1
			case "body":
				failure.BodyBytes = -1
			case "output":
				failure.OutputBytes = -1
			}
			if got := (NativeTerminalFailure{Publication: &failure}).Public(); got.Publication != nil {
				t.Fatalf("retained invalid diagnostics: %+v", got.Publication)
			}
		})
	}
}
