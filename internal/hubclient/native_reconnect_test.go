package hubclient

import (
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRunnerReconnectsRetainedAttempt(t *testing.T) {
	t.Parallel()
	for _, outcome := range []string{"", "interrupted"} {
		t.Run("previous outcome "+outcome, func(t *testing.T) {
			t.Parallel()
			h := newNativeChangeHub(t, true)
			h.admin.client.baseURL.Scheme = "https"
			issue := h.createInProgress(t, "Retained execution")
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			identityURL := *h.admin.client.baseURL
			identityURL.Scheme = "https"
			file, err := runnerauth.Initialize(path, identityURL.String())
			if err != nil {
				t.Fatal(err)
			}
			enrollment, err := h.admin.client.CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{
				Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project},
				Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 60,
			})
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: "retained", DisplayName: "Retained", Capacity: 1, Version: "test", OS: "darwin", Architecture: "arm64", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
			var identity runnerauth.Identity
			if err := h.admin.client.runnerRequest(t.Context(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &identity); err != nil {
				t.Fatal(err)
			}
			file.Identity = identity
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			newScheduler := func() *Scheduler {
				t.Helper()
				client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: h.admin.client.httpClient})
				if err != nil {
					t.Fatal(err)
				}
				scheduler, err := NewScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: identity.MachineID, Hostname: redemption.Hostname, Capacity: 1, Version: "test", BackendIsolation: redemption.BackendIsolation}, HeartbeatInterval: 30 * time.Second, LeaseTTL: 10 * time.Minute})
				if err != nil {
					t.Fatal(err)
				}
				return scheduler
			}
			otherScheduler := h.scheduler
			h.scheduler = newScheduler()
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			executionIdentity := tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}
			if err := execution.Start(t.Context(), executionIdentity); err != nil {
				t.Fatal(err)
			}
			if err := execution.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: 41, Generation: 1, HeartbeatAt: time.Now(), Phase: "implementation"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", WorkspaceDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ExternalEffect: "none", EffectState: "none"}
			if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			if outcome != "" {
				if err := execution.Finish(t.Context(), outcome); err != nil {
					t.Fatal(err)
				}
			}
			original := execution.claim.lease
			sequence := execution.data.Sequence
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "orchestrator_release"); err != nil {
				t.Fatal(err)
			}
			candidates, err := otherScheduler.FetchCandidateIssues(t.Context(), orchestrator.SchedulingRequest{Policy: h.descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
			if err != nil || len(candidates) != 0 {
				t.Fatalf("foreign claim = %+v, %v", candidates, err)
			}
			h.scheduler = newScheduler()
			h.claim(t, issue.ID)
			resumed := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if !sameCompletionLease(resumed.claim.lease, original) || resumed.data.Sequence != sequence || resumed.worktreeState != "dirty" || resumed.worktreeHead != checkpoint.HeadSHA {
				t.Fatalf("reconnect replaced authority or checkpoint: %+v", resumed)
			}
			if err := resumed.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 2, HeartbeatAt: time.Now(), Phase: "implementation"}); err != nil {
				t.Fatal(err)
			}
			if err := resumed.Start(t.Context(), executionIdentity); err != nil {
				t.Fatal(err)
			}
			if err := resumed.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			if err := resumed.Finish(t.Context(), "failed"); err != nil {
				t.Fatal(err)
			}
			if err := h.scheduler.ReleaseClaim(t.Context(), issue.ID, "orchestrator_release"); err != nil {
				t.Fatal(err)
			}
			if _, err := resumed.claim.source.client.Renew(t.Context(), original, 600); !nativeLeaseLost(err) {
				t.Fatalf("finished execution retained lease: %v", err)
			}
		})
	}
}
