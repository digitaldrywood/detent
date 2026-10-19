package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeTerminalFailureSignatureEvidence(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		err   error
		class string
		want  string
	}{
		{"absent", nil, "", ""},
		{"acceptance rejection", fmt.Errorf("%w: acceptance publication: %w", ErrValidatorInfrastructure, acceptanceRejectionError{}), "acceptance_publication", "publication rejected: category=acceptance_validation field=validator.session_id [private error text omitted]"},
		{"ordinary", errors.New("count 116 failed\nprivate continuation"), "", "count 116 failed"},
		{"workspace", fmt.Errorf("%w: /private/worktree token=secret-value", ErrWorkspacePreparation), "workspace_hook", "workspace preparation failed: [redacted] [redacted]"},
		{"hook diagnostic", &workspace.HookError{Hook: "after_create", Command: "SECRET=private-value missing-script", Dir: "/private/worktree", ExitCode: 127, Output: "script not found token=secret-value WORKSPACE_VALUE=private-value\nprivate continuation"}, "workspace_hook", "workspace hook after_create failed (exit status 127)"},
		{"unlabelled hook output", &workspace.HookError{Hook: "after_create", ExitCode: 127, Output: "correcthorsebatterystaple"}, "workspace_hook", "workspace hook after_create failed (exit status 127)"},
		{"issue configuration", &IssueConfigurationError{Field: "effort", Reason: "unsupported value"}, "issue_configuration", "agent override rejected: effort: unsupported value"},
		{"source recovery", fmt.Errorf("%w: publication source changed before reconciliation", ErrNativeRecoveryRequired), "source_recovery", "native checkpoint requires recovery: publication source changed before reconciliation"},
		{"startup timeout", ErrMergeWorkerStartupTimeout, "backend_startup", "merge worker startup timed out"},
		{"startup failure", backendcapacity.NewError(backendcapacity.Scope{}, backendcapacity.Details{Kind: backendcapacity.StartupFailureKind}, errors.New("start backend failed")), "backend_startup", "start backend failed"},
		{"protocol", &testProviderResponseError{operation: "turn/start"}, "protocol", "provider request refused"},
		{"provider body omitted", signatureProviderError{}, "protocol", "codex turn_start: count 2 failed [redacted]"},
		{"initialize", &testProviderResponseError{operation: "initialize"}, "backend_startup", "provider request refused"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := nativeTerminalFailure(test.err, now, false)
			if test.err == nil {
				if got != nil {
					t.Fatalf("unexpected failure %#v", got)
				}
				return
			}
			if got == nil || got.Error != test.want || got.ErrorClass != test.class || got.ObservedAt != now {
				t.Fatalf("failure=%#v", got)
			}
			if (test.name == "issue configuration" || test.name == "source recovery") && (got.Attribution != "issue" || got.Stage != "") {
				t.Fatalf("issue-owned error acquired workspace attribution: %#v", got)
			}
			if test.name == "acceptance rejection" && (got.Stage != "acceptance_publication" || got.Attribution != "instance" || got.Rejection == nil || got.Rejection.Field != "validator.session_id") {
				t.Fatalf("acceptance rejection lost: %+v", got)
			}
			if public := got.Public(); public.Error != got.Error || public.ErrorClass != got.ErrorClass {
				t.Fatalf("publication changed signature evidence: %#v", public)
			}
		})
	}
	t.Run("bounded text", func(t *testing.T) {
		got := nativeTerminalFailure(errors.New(strings.Repeat("x", 10000)), now, false)
		if len(got.Error) > tracker.NativeFinalizationTextLimit {
			t.Fatalf("unbounded error: %d", len(got.Error))
		}
	})
}

type signatureProviderError struct{}

func (signatureProviderError) Error() string {
	return "provider error: private response body"
}

func (signatureProviderError) BackendErrorBody() string {
	return "private response body"
}

func (signatureProviderError) BackendErrorMessage() string {
	return "count 2 failed token=private-secret"
}

func (signatureProviderError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	code := -32602
	return tracker.NativeTerminalFailure{Provider: "codex", Operation: "turn/start", RPCCode: &code}
}

type acceptanceRejectionError struct{}

func (acceptanceRejectionError) Error() string { return "private-payload token=private-credential" }

func (acceptanceRejectionError) NativeTerminalFailure() tracker.NativeTerminalFailure {
	return tracker.NativeTerminalFailure{Rejection: &tracker.PublicationRejection{Category: "acceptance_validation", Field: "validator.session_id"}}
}
