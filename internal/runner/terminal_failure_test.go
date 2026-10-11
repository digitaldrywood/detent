package runner

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/backendcapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
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
		{"ordinary", errors.New("count 116 failed\nprivate continuation"), "", "count 116 failed"},
		{"workspace", fmt.Errorf("%w: /private/worktree token=secret-value", ErrWorkspacePreparation), "workspace_hook", "workspace preparation failed: [redacted] [redacted]"},
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
