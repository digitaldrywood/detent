package update

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestWorkerReleaseFailureReasonIsInformativeAndSafe(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want string
		omit []string
	}{
		{
			name: "legacy parser additive field refusal",
			err:  errors.New(`verify worker release: decode release provenance: json: unknown field "validation_override"`),
			want: `Execution release preparation failed: release provenance contains unsupported field "validation_override"`,
		},
		{
			name: "provenance commit mismatch",
			err:  errors.New(`verify worker release: release provenance commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"`),
			want: "Execution release preparation failed: release provenance commit does not match",
		},
		{
			name: "unknown field with unsafe contents",
			err:  errors.New(`decode release provenance: json: unknown field "token=secret /private/item"`),
			want: "Execution release preparation failed: release provenance contains an unsupported field",
			omit: []string{"secret", "/private/item"},
		},
		{
			name: "arbitrary download error",
			err:  errors.New("download https://example.invalid/private?token=secret"),
			want: workerReleasePreparationFailure,
			omit: []string{"https://", "secret", "/private"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := workerReleaseFailureReason(test.err)
			if got != test.want {
				t.Fatalf("workerReleaseFailureReason() = %q, want %q", got, test.want)
			}
			for _, forbidden := range test.omit {
				if strings.Contains(got, forbidden) {
					t.Fatalf("failure reason leaked %q: %s", forbidden, got)
				}
			}
		})
	}
}

func TestWorkerReleaseRefusalPublishesSafeProvenanceDetail(t *testing.T) {
	artifacts, _, publish := workerArtifactFixture(t)
	publish("1.0.1")
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	running := runnerauth.BuildEvidence{Version: "1.0.0", Commit: testPreviousCommit, Source: "release", SHA256: strings.Repeat("a", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
	scheduler, err := NewScheduler(SchedulerConfig{
		CheckInterval:   time.Hour,
		StatePath:       filepath.Join(t.TempDir(), "scheduler.json"),
		Updater:         artifacts.service,
		WorkerArtifacts: artifacts,
		RunningBuild:    running,
		ReserveDrain:    func(context.Context) (func(), error) { return nil, nil },
		RequestRestart:  func(string) bool { return true },
		WorkerPreflight: func(context.Context, string) error {
			return errors.New(`verify worker release: decode release provenance: json: unknown field "validation_override"`)
		},
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	observed := scheduler.EnrolledUpdate(t.Context(), running, nil)
	if observed == nil || !observed.Supported {
		t.Fatalf("initial update observation = %+v, want supported", observed)
	}
	request := runnerauth.UpdateRequest{ID: "request-provenance-refusal", Service: "detent", Version: "1.0.1", Release: true, FollowHub: true, ExpectedBuildRevision: observed.Revision, RequestedAt: now}
	observed = scheduler.EnrolledUpdate(t.Context(), running, &request)
	if observed == nil || observed.Receipt == nil || observed.Receipt.Status != "refused" {
		t.Fatalf("update receipt = %+v, want refused", observed)
	}
	want := `Execution release preparation failed: release provenance contains unsupported field "validation_override"`
	if observed.Receipt.FailureReason != want {
		t.Fatalf("receipt FailureReason = %q, want %q", observed.Receipt.FailureReason, want)
	}
}
