package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hostmetrics"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRoutingChangeDuringIsolationProbe(t *testing.T) {
	for _, tc := range []struct {
		name                                string
		restore, pauseAfterProbe, unchanged bool
	}{
		{name: "changed policy"},
		{name: "restored policy", restore: true},
		{name: "completed probe at unchanged routing", pauseAfterProbe: true, unchanged: true},
		{name: "overlapping probes at unchanged routing", unchanged: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testLateIsolationProbe(t, tc.restore, tc.pauseAfterProbe, tc.unchanged) })
	}
}

func TestIsolationProbeWithEarlierRoutingSnapshot(t *testing.T) {
	current := api.ProbeRouting{Policy: isolation.Policy{Tier: isolation.NativeTrusted}, Generation: 1}
	stale := api.ProbeRouting{Policy: isolation.Policy{Tier: isolation.Sandbox}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	scheduler := Scheduler{now: time.Now, isolationReport: func(ctx context.Context, policy isolation.Policy) (isolation.Report, []runnerauth.Problem) {
		if policy.Tier == current.Tier {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return isolation.Report{"example/codex": {policy.Tier}}, nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		defer close(done)
		scheduler.probeIsolation(ctx, current)
	}()
	defer func() {
		cancel()
		<-done
	}()
	<-entered
	scheduler.probeIsolation(ctx, stale)
	close(release)
	<-done
	result := scheduler.currentIsolation()
	if !result.Routing.Matches(current) || !result.Report.Supports(isolation.NativeTrusted) {
		t.Fatalf("probe with current routing lost to a stale routing snapshot: %+v", result)
	}
}

func testLateIsolationProbe(t *testing.T, restore, pauseAfterProbe, unchanged bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_test"
	file.Identity.ProjectIDs = []tracker.ProjectID{"prj_a", "prj_b"}
	file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	snapshot := runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Example", State: "active", CapacityLimit: 2}.Normalized()}
	var reports []api.MachineHeartbeatReport
	lostResponse := errors.New("heartbeat response lost")
	var loseNextResponse bool
	transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		var result any
		switch {
		case r.URL.Path == "/api/v2/capabilities":
			result = nativeCapabilities{ProtocolMajors: []int{2}, EventSchemas: []int{1}, Features: []string{"native_issues", "scoped_collaboration", "repository_policy", tracker.NativeHostMetricsCapability}}
		case strings.HasSuffix(r.URL.Path, "/projects/prj_a"), strings.HasSuffix(r.URL.Path, "/projects/prj_b"):
			result = tracker.NativeProject{Profile: "native"}
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			var report api.MachineHeartbeatReport
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				return nil, err
			}
			reports = append(reports, report)
			if loseNextResponse {
				loseNextResponse = false
				return nil, lostResponse
			}
			result = snapshot
		default:
			return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
		}
		err := json.NewEncoder(w).Encode(result)
		return w.Result(), err
	})
	client, err := newTestClient(t, Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var blockNext atomic.Bool
	pause := func(ctx context.Context) bool {
		if blockNext.Swap(false) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return true
		}
		return false
	}
	old := isolation.Report{"example/codex": {isolation.Sandbox}}
	current := isolation.Report{"example/codex": {isolation.NativeTrusted}}
	var incomplete, withdrawn bool
	var beforeSend func()
	now := time.Now()
	scheduler, err := newTestScheduler(client, SchedulerConfig{
		OrganizationID: "org_test", NativeProjects: map[string]tracker.ProjectID{"a": "prj_a", "b": "prj_b"},
		Machine:           Machine{ID: file.Identity.MachineID, Hostname: "example", Capacity: 2, Version: "test"},
		HeartbeatInterval: time.Second, LeaseTTL: time.Minute, Now: func() time.Time { return now },
		AvailableCapacity: func(ctx context.Context, capacity int) int {
			if pauseAfterProbe {
				pause(ctx)
			}
			if beforeSend != nil {
				beforeSend()
				beforeSend = nil
			}
			return capacity
		},
		IsolationReport: func(ctx context.Context, policy isolation.Policy) (isolation.Report, []runnerauth.Problem) {
			if !pauseAfterProbe && pause(ctx) {
				return old, nil
			}
			if incomplete {
				p := runnerauth.NewProblem("backend_probe_failed")
				p.Subject = "example/codex"
				return isolation.Report{"example/codex": {}}, []runnerauth.Problem{p}
			}
			if len(policy.HostServices) > 0 || withdrawn {
				problem := runnerauth.NewProblem("tier_unavailable")
				problem.Subject = "example/codex/sandbox"
				return current, []runnerauth.Problem{problem}
			}
			return old, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	heartbeatB := func() {
		t.Helper()
		now = now.Add(time.Minute)
		if err := scheduler.ensureNativeMachine(t.Context(), scheduler.nativeProjects["b"]); err != nil {
			t.Fatal(err)
		}
	}
	heartbeatB()
	if last := reports[len(reports)-1]; last.Capacity != 2 || !reflect.DeepEqual(last.BackendIsolation, old) {
		t.Fatalf("initial heartbeat = %+v", last)
	}
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	blockNext.Store(true)
	done := make(chan struct{})
	var lateErr error
	go func() {
		defer close(done)
		lateErr = scheduler.ensureNativeMachine(ctx, scheduler.nativeProjects["a"])
	}()
	defer func() {
		cancel()
		<-done
	}()
	select {
	case <-entered:
	case <-done:
		t.Fatalf("project A returned before reaching the pause: %v", lateErr)
	}
	reports = nil
	if unchanged {
		withdrawn = true
	} else {
		snapshot.Revision++
		servicePath := filepath.Join(t.TempDir(), "example.sock")
		snapshot.Routing.HostServices = []string{"unix:" + filepath.ToSlash(strings.TrimPrefix(servicePath, filepath.VolumeName(servicePath)))}
		heartbeatB()
		if len(reports) != 2 || reports[0].Capacity != 2 || reports[1].Capacity != 2 || !reflect.DeepEqual(reports[0].BackendIsolation, old) || len(reports[1].BackendIsolation) != 0 {
			t.Errorf("warm heartbeat did not withdraw old capabilities at unchanged capacity: %+v", reports)
		}
	}
	if unchanged {
		checkFlush := func() {
			t.Helper()
			before := len(reports)
			if err := scheduler.flushHostMetrics(t.Context(), []hostmetrics.Summary{{SampleCount: 1}}); err != nil {
				t.Fatal(err)
			}
			if len(reports) != before+1 || !reflect.DeepEqual(reports[before].BackendIsolation, current) || !slices.ContainsFunc(reports[before].Problems, func(p runnerauth.Problem) bool {
				return p.Code == "tier_unavailable" && p.Subject == "example/codex/sandbox"
			}) {
				t.Errorf("final metrics after withdrawal = %+v", reports[before:])
			}
		}
		beforeSend = func() {
			if pauseAfterProbe {
				checkFlush()
			}
			loseNextResponse = true
		}
		now = now.Add(time.Minute)
		if err := scheduler.ensureNativeMachine(t.Context(), scheduler.nativeProjects["b"]); !errors.Is(err, lostResponse) {
			t.Fatalf("withdrawal send = %v, want lost response", err)
		}
		if last := reports[len(reports)-1]; !reflect.DeepEqual(last.BackendIsolation, current) {
			t.Fatalf("withdrawal before lost response = %+v", last)
		}
		checkFlush()
	} else {
		heartbeatB()
		if last := reports[len(reports)-1]; !reflect.DeepEqual(last.BackendIsolation, current) {
			t.Fatalf("current probe heartbeat = %+v", last)
		}
	}
	if restore {
		snapshot.Revision++
		snapshot.Routing.HostServices = nil
		heartbeatB()
		withdrawn = true
		heartbeatB()
	}
	reports = nil
	close(release)
	<-done
	if lateErr != nil {
		t.Fatal(lateErr)
	}
	if len(reports) != 1 || !reflect.DeepEqual(reports[0].BackendIsolation, current) || !slices.ContainsFunc(reports[0].Problems, func(p runnerauth.Problem) bool {
		return p.Code == "tier_unavailable" && p.Subject == "example/codex/sandbox"
	}) {
		t.Errorf("late project A heartbeat replaced project B capabilities: %+v", reports)
	}
	incomplete = true
	heartbeatB()
	if last := reports[len(reports)-1]; !reflect.DeepEqual(last.BackendIsolation, current) {
		t.Errorf("incomplete current probe lost the last completed current report: %+v", last)
	}
}

func TestIsolationProbeCancellation(t *testing.T) {
	for _, test := range []struct {
		name             string
		cached, complete bool
		changedPolicy    isolation.Policy
		workflowFailure  bool
		innerTimeout     bool
		completedPeer    string
		workflowLoad     bool
		cancel, killGit  bool
	}{
		{name: "unobserved cancellation"},
		{name: "cached cancellation", cached: true},
		{name: "tier changed", cached: true, changedPolicy: isolation.Policy{Tier: isolation.NativeTrusted}},
		{name: "services changed", cached: true, changedPolicy: isolation.Policy{Tier: isolation.Sandbox, HostServices: []string{"tcp:127.0.0.1:8080"}}},
		{name: "cached inner timeout", cached: true, innerTimeout: true},
		{name: "cached missing backend with incomplete peer", cached: true, innerTimeout: true, completedPeer: "backend_missing"},
		{name: "cached missing sandbox with incomplete peer", cached: true, innerTimeout: true, completedPeer: "tier_unavailable"},
		{name: "cached completed native peer with incomplete peer", cached: true, innerTimeout: true, completedPeer: "native_only"},
		{name: "cached missing backend before aggregate timeout", cached: true, completedPeer: "backend_missing"},
		{name: "cached missing sandbox before aggregate timeout", cached: true, completedPeer: "tier_unavailable"},
		{name: "cached cancellation preserves workflow failure", cached: true, workflowFailure: true},
		{name: "cached expired workflow load", cached: true, workflowLoad: true},
		{name: "cached canceled workflow load", cached: true, workflowLoad: true, cancel: true},
		{name: "cached killed workflow load", cached: true, workflowLoad: true, killGit: true},
		{name: "cached killed workflow load preserves earlier failure", cached: true, workflowFailure: true, workflowLoad: true, killGit: true},
		{name: "completed native only", complete: true},
		{name: "refresh native only", cached: true, complete: true},
	} {
		cached := test.cached
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configured := globalconfig.Project{ID: "example-project", Workdir: root, Workflow: filepath.Join(root, "WORKFLOW.md"), WorkflowRef: "origin/HEAD"}
			if test.killGit {
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
				t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "absent"))
				t.Setenv("GIT_ALLOW_PROTOCOL", "git")
				for _, args := range [][]string{{"init", "--quiet", "--template="}, {"remote", "add", "origin", "git://" + listener.Addr().String() + "/example.git"}} {
					if output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
						t.Fatalf("prepare workflow repository: %v\n%s", err, output)
					}
				}
			}
			run := func(t *testing.T) {
				completed := time.Now()
				now := completed
				calls := 0
				followup := false
				holds, releases := 0, 0
				parent, cancel := context.WithCancel(t.Context())
				if test.killGit {
					cancel()
					parent, cancel = context.WithTimeout(t.Context(), time.Second)
				}
				defer cancel()
				scheduler := &Scheduler{now: func() time.Time { return now }, leaseHold: func(context.Context) (func(), error) {
					holds++
					return func() { releases++ }, nil
				}, isolationReport: func(ctx context.Context, _ isolation.Policy) (isolation.Report, []runnerauth.Problem) {
					calls++
					if cached && calls == 1 {
						report := isolation.Report{"example-project/codex": {isolation.NativeTrusted, isolation.Sandbox}}
						if test.completedPeer != "" {
							report["example-project/claude"] = []string{isolation.NativeTrusted, isolation.Sandbox}
						}
						return report, nil
					}
					if test.complete {
						return isolation.Report{"example-project/codex": {isolation.NativeTrusted}}, nil
					}
					problem := runnerauth.Problem{Code: "backend_probe_failed", Subject: "example-project/codex", Check: "codex login status", Message: "Probe incomplete", ErrorOutput: "context deadline exceeded", FixHint: "Check the runner probe", ReportedAt: time.Now()}
					problems := []runnerauth.Problem{problem}
					report := isolation.Report{"example-project/codex": {}}
					if test.completedPeer != "" {
						report["example-project/claude"] = []string{}
						peer := runnerauth.NewProblem(test.completedPeer)
						peer.Subject = "example-project/claude"
						if followup {
							peer = problem
							peer.Subject = "example-project/claude"
						} else if test.completedPeer != "backend_missing" {
							report["example-project/claude"] = []string{isolation.NativeTrusted}
							peer.Subject += "/sandbox"
						}
						if followup || test.completedPeer != "native_only" {
							problems = append(problems, peer)
						}
					}
					if test.workflowFailure {
						problems = append(problems, runnerauth.WorkflowLoadProblems("example-project", errors.New("parse workflow: invalid settings (signal: killed)"))...)
					}
					if test.workflowLoad {
						wantCause := context.DeadlineExceeded
						if test.cancel {
							cancel()
							wantCause = context.Canceled
						}
						if !test.killGit {
							<-ctx.Done()
						}
						_, err := project.LoadWorkflowContext(ctx, configured)
						if !errors.Is(err, wantCause) {
							t.Fatalf("workflow load lost probe cause %v: %v", wantCause, err)
						}
						if test.killGit {
							var exitErr *exec.ExitError
							if !errors.As(err, &exitErr) || runtime.GOOS != "windows" && exitErr.Exited() {
								t.Fatalf("workflow git was not killed by the probe deadline: %v", err)
							}
						}
						problems = append(problems, runnerauth.WorkflowLoadProblems(configured.ID, err)...)
					}
					if !test.innerTimeout && !followup {
						<-ctx.Done()
					}
					return report, problems
				}}
				policy := api.ProbeRouting{Policy: isolation.Policy{Tier: isolation.Sandbox}}
				if cached {
					scheduler.probeIsolation(parent, policy)
					now = now.Add(time.Minute)
				}
				if test.changedPolicy.Tier != "" {
					policy.Policy = test.changedPolicy
				}
				scheduler.probeIsolation(parent, policy)
				report, problems := scheduler.lastIsolation.Report, scheduler.lastIsolation.Problems
				if test.changedPolicy.Tier != "" {
					if report.Supports(isolation.Sandbox) || report.Supports(isolation.NativeTrusted) || len(problems) == 0 || problems[len(problems)-1].CachedCapabilities {
						t.Fatalf("interrupted probe reused earlier routing: report=%v problems=%+v", report, problems)
					}
					return
				}
				if holds != calls || releases != holds {
					t.Fatalf("probe hold leaked: calls=%d holds=%d releases=%d", calls, holds, releases)
				}
				if test.complete {
					if !report.Supports(isolation.NativeTrusted) || report.Supports(isolation.Sandbox) || len(problems) != 0 {
						t.Fatalf("native result unavailable or manufactured sandbox: report=%+v problems=%+v", report, problems)
					}
					if !scheduler.lastCompleteProbeAt.Equal(now) {
						t.Fatalf("completed native result was not refreshed: %v", scheduler.lastCompleteProbeAt)
					}
					return
				}
				if report.Supports(isolation.NativeTrusted) != (cached && test.completedPeer != "backend_missing") {
					t.Fatalf("probe changed readiness contrary to completed checks: %+v", report)
				}
				wantReport := isolation.Report{"example-project/codex": {isolation.NativeTrusted, isolation.Sandbox}}
				if test.completedPeer != "" {
					wantReport["example-project/claude"] = []string{}
					if test.completedPeer != "backend_missing" {
						wantReport["example-project/claude"] = []string{isolation.NativeTrusted}
					}
				}
				if cached && (!reflect.DeepEqual(report, wantReport) || !scheduler.lastCompleteProbeAt.Equal(completed)) {
					t.Fatalf("cancelled workflow load changed the completed probe: report=%+v at %v", report, scheduler.lastCompleteProbeAt)
				}
				wantProblems := 2
				if cached && !test.workflowFailure {
					wantProblems = 1
				}
				if test.completedPeer != "" && test.completedPeer != "native_only" {
					wantProblems++
					if problems[0].Code != test.completedPeer {
						t.Fatalf("completed prerequisite failure was dropped: %+v", problems)
					}
				}
				if len(problems) != wantProblems {
					t.Fatalf("partial probe problems were not consolidated: %+v", problems)
				}
				if test.workflowFailure && (problems[0].Code != "settings_invalid" || problems[0].ErrorOutput != "parse workflow: invalid settings (signal: killed)") {
					t.Fatalf("genuine workflow failure was dropped: %+v", problems)
				}
				observation := problems[len(problems)-1]
				if observation.CachedCapabilities != cached {
					t.Fatalf("cached capability provenance lost: %+v", observation)
				}
				if observation.Code != "backend_probe_failed" || observation.Check != "runner isolation probe" || !observation.ReportedAt.Equal(now) || observation.FixCommand != "" {
					t.Fatalf("incomplete probe observation=%+v", observation)
				}
				if cached && !strings.Contains(observation.Message, completed.UTC().Format(time.RFC3339Nano)) {
					t.Fatalf("cached capability observation time lost: %+v", observation)
				}
				if !cached && !scheduler.lastCompleteProbeAt.IsZero() {
					t.Fatal("incomplete capabilities were cached as completed")
				}
				if test.completedPeer != "" {
					followup = true
					now = now.Add(time.Minute)
					scheduler.probeIsolation(t.Context(), policy)
					if !reflect.DeepEqual(scheduler.lastIsolation.Report, wantReport) {
						t.Fatalf("later incomplete checks restored revoked capabilities: %+v, want %+v", scheduler.lastIsolation.Report, wantReport)
					}
				}
			}
			if test.killGit {
				run(t)
			} else {
				synctest.Test(t, run)
			}
		})
	}
}

func TestIsolationProbeRecoversAfterTimeouts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		scheduler := &Scheduler{now: time.Now, isolationReport: func(ctx context.Context, _ isolation.Policy) (isolation.Report, []runnerauth.Problem) {
			calls++
			if calls <= 3 {
				<-ctx.Done()
				return isolation.Report{"example-project/codex": {}}, nil
			}
			timer := time.NewTimer(20 * time.Second)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return isolation.Report{"example-project/codex": {}}, nil
			case <-timer.C:
				return isolation.Report{"example-project/codex": {isolation.NativeTrusted}}, nil
			}
		}}
		for attempt := 1; attempt <= 4; attempt++ {
			scheduler.probeIsolation(t.Context(), api.ProbeRouting{})
			report, problems := scheduler.lastIsolation.Report, scheduler.lastIsolation.Problems
			if attempt < 4 {
				if report.Supports(isolation.NativeTrusted) || len(problems) == 0 {
					t.Fatalf("incomplete probe advertised readiness: %+v %+v", report, problems)
				}
				continue
			}
			if !report.Supports(isolation.NativeTrusted) || len(problems) != 0 {
				t.Fatalf("slow probe did not restore readiness: %+v %+v", report, problems)
			}
			if problems := api.IsolationProblems(report, isolation.NativeTrusted, nil); len(problems) != 0 {
				t.Fatalf("recovered runner still refuses its tier: %+v", problems)
			}
		}
	})
}
