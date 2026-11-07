package dispatch

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeRuntimeHeartbeatDuringQuietCommand(t *testing.T) {
	if os.Getenv("DETENT_RUNTIME_HEARTBEAT_WORKER") == "1" {
		if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, batching := range []bool{false, true} {
		t.Run(strconv.FormatBool(batching), func(t *testing.T) {
			var hubTime atomic.Int64
			hubTime.Store(time.Now().UTC().Truncate(time.Microsecond).UnixNano())
			clock := func() time.Time { return time.Unix(0, hubTime.Load()).UTC() }
			h := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}},
				{Name: "Done", Terminal: true},
			}, true, false, hubserver.Config{InitialAdminToken: []byte(nativeChangeAdminToken), Conversation: &hubserver.ConversationConfig{Enabled: true}, Clock: clock})
			issue := h.createInProgress(t, "Quiet worker")
			h.projectBoard()
			fixtureClient(h.admin.Transport()).baseURL.Scheme = "https"
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			file, err := runnerauth.Initialize(path, fixtureClient(h.admin.Transport()).baseURL.String())
			if err != nil {
				t.Fatal(err)
			}
			enrollment, err := h.admin.Transport().CreateRunnerEnrollment(t.Context(), h.organization, runnerauth.EnrollmentRequest{
				Binding: file.Identity.Binding, ProjectIDs: []tracker.ProjectID{h.project},
				Operations: []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}, TTLSeconds: 60,
			})
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{Binding: file.Identity.Binding, Credential: file.Credential, Hostname: "worker", DisplayName: "Worker", Capacity: 1, Version: "test", BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}}
			if err := fixtureRunnerRequest(t, h.admin.Transport(), enrollment.Token, http.MethodPost, "/api/v2/organizations/"+string(h.organization)+"/runner-enrollments/redeem", redemption, &file.Identity); err != nil {
				t.Fatal(err)
			}
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			client, err := newTestClient(t, Config{URL: file.HubURL, IdentityFile: path, HTTPClient: fixtureClient(h.admin.Transport()).httpClient})
			if err != nil {
				t.Fatal(err)
			}
			const ttl = 600 * time.Second
			h.scheduler, err = newTestScheduler(client, SchedulerConfig{OrganizationID: h.organization, NativeProjects: map[string]tracker.ProjectID{"local": h.project}, Machine: Machine{ID: file.Identity.MachineID, Hostname: "worker", Capacity: 1, Version: "test", BackendIsolation: redemption.BackendIsolation}, HeartbeatInterval: 30 * time.Second, LeaseTTL: ttl})
			if err != nil {
				t.Fatal(err)
			}
			h.claim(t, issue.ID)
			h.scheduler.executionFactory = func(host ExecutionHost, view ExecutionClaim) Execution {
				view.Lease.RunDetailBatching = batching
				return nativechange.NewExecution(host, view)
			}
			execution := h.scheduler.RunExecution(issue.ID).(interface {
				Execution
				runner.WorkerLifetimeExecution
			})
			command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNativeRuntimeHeartbeatDuringQuietCommand$")
			command.Env = append(os.Environ(), "DETENT_RUNTIME_HEARTBEAT_WORKER=1", "GOCOVERDIR="+t.TempDir())
			input, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			ready, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			procgroup.Configure(t.Context(), command)
			if err := procgroup.Start(command); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					if err := command.Process.Kill(); err != nil {
						t.Error(err)
					}
					_ = procgroup.Wait(command)
				}
			})
			readyMessage := make([]byte, len("ready\n"))
			if _, err := io.ReadFull(ready, readyMessage); err != nil || string(readyMessage) != "ready\n" {
				t.Fatalf("worker readiness = %q, %v", readyMessage, err)
			}
			worker, err := procgroup.Inspect(command)
			if err != nil {
				t.Fatal(err)
			}
			if err := execution.KeepWorker(func(context.Context) error { return nil }, nil); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Truncate(time.Microsecond)
			hubTime.Store(at.UnixNano())
			now := at
			h.scheduler.now = func() time.Time { return now }
			if err := execution.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{LocalAttemptID: 42, Generation: 1, Phase: "implementation", HeartbeatAt: at, WorkerLaunch: &tracker.NativeWorkerLaunch{PID: worker.PID, GroupID: worker.GroupID, ProcessStart: worker.StartedAt, PolicyID: h.descriptor.ID, ExecutionVersion: "test", ControlVersion: "test", ExecutionCommit: strings.Repeat("a", 40), ExecutionSHA256: strings.Repeat("b", 64), Protocol: 1, SelectedAt: at}}); err != nil {
				t.Fatal(err)
			}
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			assertRuntime := func(heartbeat time.Time, sequence int64, expired bool) {
				t.Helper()
				evidence, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), "")
				if err != nil || evidence.Attempt == nil {
					t.Fatalf("runtime=%+v error=%v", evidence, err)
				}
				a := evidence.Attempt
				if a.Runtime == nil || !a.Runtime.HeartbeatAt.Equal(heartbeat) || a.Sequence != sequence || !a.Current || (a.RuntimeFreshness == "expired") != expired {
					t.Fatalf("elapsed=%v runtime=%+v attempt=%+v want heartbeat=%v sequence=%d expired=%v", time.Since(at), a.Runtime, a, heartbeat, sequence, expired)
				}
				var view runnerauth.Runner
				if err := h.admin.Transport().Request(t.Context(), http.MethodGet, "/api/v2/organizations/"+string(h.organization)+"/runners/"+file.Identity.RunnerID+"/routing", nil, &view); err != nil {
					t.Fatal(err)
				}
				wantHealth := "online"
				if expired {
					wantHealth = "offline"
				}
				if view.Health != wantHealth {
					t.Fatalf("runner health=%s problems=%+v want %s", view.Health, view.Problems, wantHealth)
				}
			}
			for elapsed := 50 * time.Second; elapsed <= 3*ttl; elapsed += 50 * time.Second {
				now = at.Add(elapsed)
				hubTime.Store(now.UnixNano())
				if _, err := h.scheduler.RenewClaim(t.Context(), issue.ID, now); err != nil {
					current, readErr := h.native.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
					alive, probeErr := procgroup.Alive(worker)
					t.Fatalf("renew at %s: %v; state=%s eligible=%v dependencies=%t read=%v alive=%t probe=%v", elapsed, err, current.State, current.SchedulingEligible, current.DependencyReady, readErr, alive, probeErr)
				}
				assertRuntime(at.Add(elapsed/(ttl/3)*(ttl/3)), 1+int64(elapsed/(ttl/3)), false)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := procgroup.Wait(command); err == nil {
				t.Fatal("killed command succeeded")
			}
			waited = true
			claim := h.scheduler.nativeClaims[issue.ID]
			if renewable, err := h.scheduler.nativeClaimRenewable(t.Context(), claim); err != nil || renewable {
				t.Fatalf("dead worker renewable=%v error=%v", renewable, err)
			}
			for range 13 {
				now = now.Add(50 * time.Second)
				hubTime.Store(now.UnixNano())
				if _, err := claim.source.NativeClient().Renew(t.Context(), claim.lease, int64(ttl/time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			assertRuntime(at.Add(3*ttl), 10, true)
		})
	}
}
