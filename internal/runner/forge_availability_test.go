package runner

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/forgeavailability"
)

func TestClassifyForgeDeliverableError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		operation string
		message   string
		wantClass string
		wantTyped bool
	}{
		{name: "git 503", operation: "git push", message: "HTTP 503: unavailable", wantClass: forgeavailability.ClassServer, wantTyped: true},
		{name: "git timeout", operation: "git push", message: "operation timed out", wantClass: forgeavailability.ClassTimeout, wantTyped: true},
		{name: "git DNS", operation: "git push", message: "Could not resolve host: github.com", wantClass: forgeavailability.ClassTransport, wantTyped: true},
		{name: "pull request 502", operation: "codex_apps/github.create_pull_request", message: "HTTP 502: bad gateway", wantClass: forgeavailability.ClassServer, wantTyped: true},
		{name: "non fast forward", operation: "git push", message: "[rejected] feature -> feature (non-fast-forward)"},
		{name: "forbidden", operation: "git push", message: "HTTP 403: forbidden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deliverableErr := &DeliverableCommandError{
				OperationClass: "push",
				Operation:      tt.operation,
				Status:         "failed",
				Message:        tt.message,
			}
			got := classifyForgeDeliverableError(deliverableErr, "github.com", false)
			availabilityErr, typed := forgeavailability.As(got)
			if typed != tt.wantTyped {
				t.Fatalf("typed = %v, want %v; error = %v", typed, tt.wantTyped, got)
			}
			if !tt.wantTyped {
				if !errors.Is(got, deliverableErr) {
					t.Fatalf("error = %v, want original deliverable failure", got)
				}
				return
			}
			if availabilityErr.Class != tt.wantClass || availabilityErr.Scope.Host != "github.com" {
				t.Fatalf("forge error = %#v, want class %q on github.com", availabilityErr, tt.wantClass)
			}
		})
	}
}
