package hubclient

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestIsolationProblems(t *testing.T) {
	for _, test := range []struct {
		name, tier          string
		report              isolation.Report
		want                []string
		reason, alternative string
	}{
		{"healthy", isolation.Sandbox, isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, []string{}, "", ""},
		{"native works", isolation.NativeTrusted, isolation.Report{"codex": {isolation.NativeTrusted}}, []string{}, "", ""},
		{"sandbox unavailable", isolation.Sandbox, isolation.Report{"codex": {isolation.NativeTrusted}}, []string{"tier_unavailable"}, "Agent access sandbox is unavailable: codex did not report support for this tier.", "Full access"},
		{"backend missing", isolation.Sandbox, isolation.Report{"codex": {}}, []string{"backend_missing", "tier_unavailable"}, "", ""},
		{"no backend", isolation.Sandbox, nil, []string{"backend_missing", "tier_unavailable"}, "", ""},
		{"workflow invalid", isolation.Sandbox, isolation.Report{"project/workflow": {}}, []string{"settings_invalid", "tier_unavailable"}, "", ""},
		{"workflow backend healthy", isolation.Sandbox, isolation.Report{"project/workflow": {isolation.Sandbox, isolation.NativeTrusted}}, []string{}, "", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			codes := []string{}
			for _, problem := range isolationProblems(test.report, test.tier, nil) {
				codes = append(codes, problem.Code)
				if problem.Code == "tier_unavailable" && test.reason != "" && (problem.Message != test.reason || !strings.Contains(problem.FixHint, "switch Agent access to "+test.alternative)) {
					t.Fatalf("tier problem=%+v", problem)
				}
			}
			if !reflect.DeepEqual(codes, test.want) {
				t.Fatalf("codes=%v want=%v", codes, test.want)
			}
		})
	}
}

func TestRunnerHostServicesReachable(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := "tcp:" + listener.Addr().String()
	if !runnerHostServicesReachable(t.Context(), []string{address}) {
		t.Fatal("running service was reported unreachable")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if runnerHostServicesReachable(t.Context(), []string{address}) {
		t.Fatal("stopped service was reported reachable")
	}
	if !runnerHostServicesReachable(t.Context(), nil) {
		t.Fatal("empty service list is not a problem")
	}
}

func TestHeartbeatProblemsClear(t *testing.T) {
	source := &runnerCredentialSource{routing: &runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{IsolationTier: isolation.Sandbox}}}
	machine := Machine{BackendIsolation: isolation.Report{"codex": {isolation.NativeTrusted}}}
	first, _ := source.heartbeatProblems(t.Context(), machine)
	second, _ := source.heartbeatProblems(t.Context(), machine)
	if len(first) != 1 || len(second) != 1 || !first[0].FirstSeen.Equal(second[0].FirstSeen) {
		t.Fatalf("problem timestamps changed: %+v %+v", first, second)
	}
	source.rejectSettings(true)
	machine.BackendIsolation["codex"] = []string{isolation.Sandbox, isolation.NativeTrusted}
	problems, rejected := source.heartbeatProblems(t.Context(), machine)
	if len(problems) != 0 || !rejected {
		t.Fatalf("problems=%+v rejected=%v", problems, rejected)
	}
	source.rejectSettings(false)
	_, rejected = source.heartbeatProblems(t.Context(), machine)
	if rejected {
		t.Fatal("settings rejection did not clear")
	}
}

func TestHeartbeatIncompleteProbe(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "unobserved", true: "cached"}[cached], func(t *testing.T) {
			source := &runnerCredentialSource{routing: &runnerauth.RoutingSnapshot{Routing: runnerauth.Routing{IsolationTier: isolation.NativeTrusted}}}
			observed := time.Now().Add(-time.Minute)
			problem := runnerauth.Problem{Code: "backend_probe_failed", Subject: "example-project/codex", ProjectID: "example-project", Check: "codex login status", Message: "Probe cancelled", ErrorOutput: "context canceled", FixHint: "Check runner probe execution", ReportedAt: observed}
			machine := Machine{BackendIsolation: isolation.Report{"example-project/codex": {}}, Problems: []runnerauth.Problem{problem}}
			if cached {
				machine.BackendIsolation["example-project/codex"] = []string{isolation.NativeTrusted}
			}
			problems, _ := source.heartbeatProblems(t.Context(), machine)
			found := false
			for _, got := range problems {
				if got.Code == "backend_missing" || got.FixCommand != "" || strings.Contains(strings.ToLower(got.FixHint), "install") {
					t.Fatalf("incomplete observation became missing tooling: %+v", problems)
				}
				if got.Code == "backend_probe_failed" {
					found = true
					if !got.ReportedAt.Equal(observed) || got.Check != problem.Check || got.Subject != problem.Subject {
						t.Fatalf("probe observation identity changed: %+v", got)
					}
				}
			}
			if !found {
				t.Fatalf("incomplete probe diagnostic was dropped: %+v", problems)
			}
			if machine.BackendIsolation.Supports(isolation.NativeTrusted) != cached {
				t.Fatal("incomplete probe changed capability decision")
			}
		})
	}
}

func TestIsolationProbeCancellation(t *testing.T) {
	for _, test := range []struct {
		name             string
		cached, complete bool
		workflowFailure  bool
		workflowLoad     bool
		cancel, killGit  bool
	}{
		{name: "unobserved cancellation"},
		{name: "cached cancellation", cached: true},
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
				holds, releases := 0, 0
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				scheduler := &Scheduler{now: func() time.Time { return now }, leaseHold: func(context.Context) (func(), error) {
					holds++
					return func() { releases++ }, nil
				}, isolationReport: func(ctx context.Context) (isolation.Report, []runnerauth.Problem) {
					calls++
					if cached && calls == 1 {
						return isolation.Report{"example-project/codex": {isolation.NativeTrusted, isolation.Sandbox}}, nil
					}
					if test.complete {
						return isolation.Report{"example-project/codex": {isolation.NativeTrusted}}, nil
					}
					problem := runnerauth.Problem{Code: "backend_probe_failed", Subject: "example-project/codex", Check: "codex login status", Message: "Probe incomplete", ErrorOutput: "context deadline exceeded", FixHint: "Check the runner probe", ReportedAt: time.Now()}
					problems := []runnerauth.Problem{problem}
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
					<-ctx.Done()
					return isolation.Report{"example-project/codex": {}}, problems
				}}
				if cached {
					scheduler.probeIsolation(parent)
					now = now.Add(time.Minute)
				}
				report, problems := scheduler.probeIsolation(parent)
				if holds != calls || releases != holds {
					t.Fatalf("probe hold leaked: calls=%d holds=%d releases=%d", calls, holds, releases)
				}
				if test.complete {
					if !report.Supports(isolation.NativeTrusted) || report.Supports(isolation.Sandbox) || len(problems) != 0 {
						t.Fatalf("native result unavailable or manufactured sandbox: report=%+v problems=%+v", report, problems)
					}
					if !reflect.DeepEqual(scheduler.lastIsolation, report) || !scheduler.lastIsolationAt.Equal(now) {
						t.Fatalf("completed native result was not refreshed: %+v at %v", scheduler.lastIsolation, scheduler.lastIsolationAt)
					}
					return
				}
				if report.Supports(isolation.NativeTrusted) != cached {
					t.Fatalf("cancelled probe changed cached capabilities: %+v", report)
				}
				if cached && (!reflect.DeepEqual(report, isolation.Report{"example-project/codex": {isolation.NativeTrusted, isolation.Sandbox}}) || !scheduler.lastIsolationAt.Equal(completed)) {
					t.Fatalf("cancelled workflow load changed the completed probe: report=%+v at %v", report, scheduler.lastIsolationAt)
				}
				wantProblems := 2
				if cached && !test.workflowFailure {
					wantProblems = 1
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
				if !cached && scheduler.lastIsolation != nil {
					t.Fatal("incomplete capabilities were cached as completed")
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
