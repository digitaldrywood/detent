package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestNativeTerminalFailureSignatureEvidence(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	token := "ghp_" + strings.Repeat("1", 36)
	for _, test := range []struct {
		name    string
		err     error
		class   string
		want    string
		summary string
	}{
		{"absent", nil, "", "", ""},
		{"acceptance rejection", fmt.Errorf("%w: acceptance publication: %w", ErrValidatorInfrastructure, acceptanceRejectionError{}), "acceptance_publication", "publication rejected: category=acceptance_validation field=validator.session_id [private error text omitted]", ""},
		{"ordinary", errors.New("count 116 failed\nprivate continuation"), "", "count 116 failed", ""},
		{"workspace", fmt.Errorf("%w: /private/worktree token=secret-value", ErrWorkspacePreparation), "workspace_hook", "workspace preparation failed: [redacted] [redacted]", ""},
		{"hook diagnostic", &workspace.HookError{Hook: "after_create", Command: "SECRET=private-value missing-script", Dir: "/private/worktree", ExitCode: 127, Output: "script not found token=secret-value WORKSPACE_VALUE=private-value\nprivate continuation"}, "workspace_hook", "workspace hook after_create failed (exit status 127)", ""},
		{"missing hook shell", &workspace.HookError{Hook: "after_create", ExitCode: -1, Err: &shell.UnavailableError{Shell: "bash"}}, "workspace_hook", `workspace hook after_create failed (exit status -1): hook_shell_unavailable: shell "bash" is unavailable on this runner`, ""},
		{"unlabelled hook output", &workspace.HookError{Hook: "after_create", ExitCode: 127, Output: "correcthorsebatterystaple"}, "workspace_hook", "workspace hook after_create failed (exit status 127)", ""},
		{"issue configuration", &IssueConfigurationError{Field: "effort", Reason: "unsupported value"}, "issue_configuration", "agent override rejected: effort: unsupported value", ""},
		{"source recovery", fmt.Errorf("%w: publication source changed before reconciliation", ErrNativeRecoveryRequired), "source_recovery", "native checkpoint requires recovery: publication source changed before reconciliation", ""},
		{"startup timeout", ErrMergeWorkerStartupTimeout, "backend_startup", "merge worker startup timed out", ""},
		{"startup failure", backendcapacity.NewError(backendcapacity.Scope{}, backendcapacity.Details{Kind: backendcapacity.StartupFailureKind}, errors.New("start backend failed")), "backend_startup", "start backend failed", ""},
		{"protocol", &testProviderResponseError{operation: "turn/start"}, "protocol", "provider request refused", ""},
		{"provider body omitted", signatureProviderError{}, "protocol", "codex turn_start: count 2 failed [redacted]", ""},
		{"initialize", &testProviderResponseError{operation: "initialize"}, "backend_startup", "provider request refused", ""},
		{"landing push", fmt.Errorf("git push develop: %w", &workspace.CommandError{Command: "git", Args: []string{"-C", "/private/worktree", "push", "origin", "HEAD:refs/heads/develop"}, Err: errors.New("exit status 128"), Output: "fatal: Authentication failed for https://user:private-url-token@github.com/example-org/example-project\npassword=private-password Bearer private-bearer " + token}), "git_push", "git push develop: git -C [redacted] push origin HEAD:[redacted] failed: exit status 128", "fatal: Authentication failed for [redacted] [redacted] Bearer [redacted] [redacted]"},
		{"push without output", &workspace.CommandError{Command: "git", Args: []string{"push", "origin"}, Err: errors.New("exit status 128")}, "git_push", "git push origin failed: exit status 128", "Git push failed; no diagnostic output recorded"},
		{"classified push", fmt.Errorf("%w: %w", &workspace.LandRefusal{Kind: workspace.LandRefusalProtected, Reason: "Permission denied " + token}, &workspace.CommandError{Command: "git", Args: []string{"push", "origin"}, Err: errors.New("exit status 128"), Output: "Permission denied " + token}), "git_push", "base_protected: Permission denied [redacted]: git push origin failed: exit status 128", "Permission denied [redacted]"},
		{"fetch output remains private", &workspace.CommandError{Command: "git", Args: []string{"fetch", "origin"}, Err: errors.New("exit status 128"), Output: "private diagnostic"}, "", "git fetch origin failed: exit status 128", "Runner execution failed; private error output omitted"},
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
			if test.class == "workspace_hook" && (got.Attribution != "instance" || got.Stage != "workspace_preparation") {
				t.Fatalf("hook failure lost instance attribution: %#v", got)
			}
			if test.summary != "" && got.Summary != test.summary {
				t.Fatalf("summary = %q, want %q", got.Summary, test.summary)
			}
			if public := got.Public(); public.Error != got.Error || public.ErrorClass != got.ErrorClass || public.Summary != got.Summary {
				t.Fatalf("publication changed signature evidence: %#v", public)
			}
		})
	}
	t.Run("bounded text", func(t *testing.T) {
		for _, err := range []error{errors.New(strings.Repeat("x", 10000)), &workspace.CommandError{Command: "git", Args: []string{"push"}, Output: strings.Repeat("x", 10000), Err: errors.New("exit status 128")}} {
			got := nativeTerminalFailure(err, now, false)
			if len(got.Error) > tracker.NativeFinalizationTextLimit || len(got.Summary) > tracker.NativeFinalizationTextLimit {
				t.Fatalf("unbounded failure: error=%d summary=%d", len(got.Error), len(got.Summary))
			}
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
