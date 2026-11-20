package nativechange

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/diagnostics"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// Dispatch must not call any workspace operation before acquiring a claim.
type providerPreviewWorkspace struct{ workspace.Backend }

func TestLocalProviderRevalidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	requirement := providercapacity.Requirement{Role: "code", Backend: "codex", Model: "sol"}
	selected := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "sol"}
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
	source := &NativeConnector{}
	lease := tracker.NativeLease{ID: "lease_test", WorkItemID: "wi_test", ProviderReservation: &providercapacity.Reservation{Requirement: requirement, Report: report}}
	for _, test := range []struct {
		name     string
		change   func(*providercapacity.Report)
		identity tracker.NativeExecutionIdentity
		wantCode string
	}{
		{"same account", func(*providercapacity.Report) {}, selected, ""},
		{"stale quota", func(r *providercapacity.Report) {
			r.Availability = "exhausted"
			r.ObservedAt = now.Add(-providercapacity.MaxAge)
		}, selected, ""},
		{"exhaustion", func(r *providercapacity.Report) { r.Availability = "exhausted" }, selected, ""},
		{"account change", func(r *providercapacity.Report) { r.AccountAlias = "other" }, selected, "unavailable"},
		{"shared pool change", func(r *providercapacity.Report) { r.SharedAccountAlias = "other" }, selected, "unavailable"},
		{"role changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "rework", Backend: "codex", Model: "sol"}, "provider_identity_mismatch_role"},
		{"backend changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "code", Backend: "other", Model: "sol"}, "provider_identity_mismatch_backend"},
		{"model changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "astra"}, "provider_identity_mismatch_model"},
		{"all changed", func(*providercapacity.Report) {}, tracker.NativeExecutionIdentity{Role: "rework", Backend: "other", Model: "private model"}, "provider_identity_mismatch_role_backend_model"},
		{"missing report source", nil, selected, "provider_report_source_missing"},
		{"removed account limit", func(r *providercapacity.Report) { r.MaxConcurrent = 0 }, selected, ""},
		{"invalid report", func(r *providercapacity.Report) { r.MaxConcurrent = -1 }, selected, "unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := report
			e := &nativeExecution{scheduler: &executionAuthority{now: func() time.Time { return now }}, claim: nativeClaim{source: source, lease: lease}}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			e.cancel = cancel
			if test.change != nil {
				test.change(&current)
				e.scheduler.providerReports = func() ([]providercapacity.Report, error) { return []providercapacity.Report{current}, nil }
			}
			remote, _ := nativeSSHExecution(t, t.Context(), e, t.TempDir())
			if got := remote.(runner.ProviderCapacityExecution).ProviderCapacity(); !reflect.DeepEqual(got, e.ProviderCapacity()) {
				t.Fatalf("SSH changed the central reservation: %#v", got)
			}
			err := e.validateProviderStart(test.identity)
			if test.wantCode == "" {
				if err != nil || ctx.Err() != nil {
					t.Fatalf("start error = %v, cancellation = %v", err, context.Cause(ctx))
				}
				return
			}
			if !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) || !errors.Is(context.Cause(ctx), err) {
				t.Fatalf("expected authority rejection, got %v, cancellation = %v", err, context.Cause(ctx))
			}
			claims := []executioncontract.ExecutionClaim{{Source: source, Lease: lease, Execution: e}}
			snapshot := runnerauth.ProjectDiagnostics{ObservedAt: now, Records: diagnostics.Claims(claims, source, now, time.Minute)}
			snapshot.Bound()
			page, err := snapshot.Page("project", "runner", string(lease.WorkItemID), "", "", 1, now)
			if err != nil || len(page.Records) != 1 {
				t.Fatalf("provider start diagnostic page: records=%d error=%v", len(page.Records), err)
			}
			record := page.Records[0]
			if record.HostOperation != "native.provider_start" || record.LatestErrorCode != test.wantCode || record.LatestError != "execution authority unavailable [private error text omitted]" {
				t.Fatalf("operator record = %+v, want redacted provider start with code %q", record, test.wantCode)
			}
		})
	}
}

func TestRecordedAttemptChecksProviderBeforeRestart(t *testing.T) {
	t.Parallel()
	identity := tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "test"}
	for _, role := range []string{"", "plan"} {
		t.Run("reserved "+role, func(t *testing.T) {
			var permit *providercapacity.Reservation
			if role != "" {
				permit = &providercapacity.Reservation{Requirement: providercapacity.Requirement{Role: role, Backend: "codex", Model: "test"}}
			}
			e := &nativeExecution{
				claim: nativeClaim{lease: tracker.NativeLease{ProviderReservation: permit}},
				scheduler: &executionAuthority{
					now:             time.Now,
					providerReports: func() ([]providercapacity.Report, error) { return nil, nil },
				},
				data: tracker.NativeRunData{Identity: &identity, Sequence: 1},
			}
			if err := e.Start(t.Context(), identity); !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatalf("stored attempt can start code without its provider permit: %v", err)
			}
		})
	}
}
