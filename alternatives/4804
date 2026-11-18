package runner

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/forgeavailability"
)

func TestClassifyForgeDeliverableError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		operationClass string
		operation      string
		message        string
		approvalDenied bool
		wantClass      string
		wantTyped      bool
	}{
		{name: "git 503", operation: "git push", message: "HTTP 503: unavailable", wantClass: forgeavailability.ClassServer, wantTyped: true},
		{name: "git timeout", operation: "git push", message: "operation timed out", wantClass: forgeavailability.ClassTimeout, wantTyped: true},
		{name: "git DNS", operation: "git push", message: "Could not resolve host: github.com", wantClass: forgeavailability.ClassTransport, wantTyped: true},
		{name: "pull request 502", operation: "codex_apps/github.create_pull_request", message: "HTTP 502: bad gateway", wantClass: forgeavailability.ClassServer, wantTyped: true},
		{name: "pull request approval denied", operationClass: "pull_request", operation: "codex_apps/github.create_pull_request", message: "tool approval declined", approvalDenied: true, wantClass: forgeavailability.ClassTransport, wantTyped: true},
		{name: "non fast forward", operation: "git push", message: "[rejected] feature -> feature (non-fast-forward)"},
		{name: "forbidden", operation: "git push", message: "HTTP 403: forbidden"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			operationClass := tt.operationClass
			if operationClass == "" {
				operationClass = "push"
			}
			deliverableErr := &DeliverableCommandError{
				OperationClass: operationClass,
				Operation:      tt.operation,
				Status:         "failed",
				Message:        tt.message,
				ApprovalDenied: tt.approvalDenied,
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

func TestCompoundPushCLIAuth(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"git push", "post-push command", "gh pr view"} {
		t.Run(operation, func(t *testing.T) {
			failure := &DeliverableCommandError{Operation: operation, Message: "32bbf98..9773c9e HEAD -> detent/example\nlist pull request labels: exit status 4: To get started with GitHub CLI, please run: gh auth login"}
			got := classifyForgeDeliverableError(failure, "github.com", true)
			if _, ok := forgeavailability.As(got); ok {
				t.Fatalf("opened outage: %v", got)
			}
			if _, ok := AsWorkerGitHubTokenResolutionError(got); !ok {
				t.Fatalf("not instance token resolution: %v", got)
			}
			if !errors.Is(got, failure) {
				t.Fatal("lost command evidence")
			}
		})
	}
}

func TestForgeDeliverableHostProvenance(t *testing.T) {
	t.Parallel()
	for _, detail := range []string{"Authentication failed", "HTTP 503: unavailable"} {
		t.Run(detail, func(t *testing.T) {
			failure := &DeliverableCommandError{OperationClass: "push", Operation: "git push", Arguments: "git push && curl https://detent.dev/status", Message: detail}
			got := classifyForgeDeliverableError(failure, "github.com", false)
			outage, ok := forgeavailability.As(got)
			if !ok || outage.Scope.Host != "github.com" {
				t.Fatalf("outage = %#v", outage)
			}
		})
	}
}
