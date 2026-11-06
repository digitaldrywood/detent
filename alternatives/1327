package nativechange

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// Dispatch must not call any workspace operation before acquiring a claim.
type providerPreviewWorkspace struct{ workspace.Backend }

func TestLocalProviderRevalidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	requirement := providercapacity.Requirement{Role: "code", Backend: "codex", Model: "sol"}
	report := providercapacity.Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 1, Availability: "available", ObservedAt: now}
	for _, test := range []struct {
		name   string
		change func(*providercapacity.Report)
		model  string
		want   bool
	}{
		{"same account", func(*providercapacity.Report) {}, "sol", true},
		{"stale quota", func(r *providercapacity.Report) {
			r.Availability = "exhausted"
			r.ObservedAt = now.Add(-providercapacity.MaxAge)
		}, "sol", true},
		{"exhaustion", func(r *providercapacity.Report) { r.Availability = "exhausted" }, "sol", true},
		{"account change", func(r *providercapacity.Report) { r.AccountAlias = "other" }, "sol", false},
		{"shared pool change", func(r *providercapacity.Report) { r.SharedAccountAlias = "other" }, "sol", false},
		{"model changed", func(*providercapacity.Report) {}, "astra", false},
		{"removed account limit", func(r *providercapacity.Report) { r.MaxConcurrent = 0 }, "sol", true},
		{"invalid report", func(r *providercapacity.Report) { r.MaxConcurrent = -1 }, "sol", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := report
			test.change(&current)
			e := &nativeExecution{scheduler: &executionAuthority{now: func() time.Time { return now }, providerReports: func() ([]providercapacity.Report, error) { return []providercapacity.Report{current}, nil }}, claim: nativeClaim{lease: tracker.NativeLease{ProviderReservation: &providercapacity.Reservation{Requirement: requirement, Report: report}}}}
			remote, _ := nativeSSHExecution(t, t.Context(), e, t.TempDir())
			if got := remote.(runner.ProviderCapacityExecution).ProviderCapacity(); !reflect.DeepEqual(got, e.ProviderCapacity()) {
				t.Fatalf("SSH changed the central reservation: %#v", got)
			}
			err := e.validateProviderStart(tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: test.model})
			if (err == nil) != test.want || err != nil && !errors.Is(err, runner.ErrExecutionAuthorityUnavailable) {
				t.Fatal(err)
			}
		})
	}
}
