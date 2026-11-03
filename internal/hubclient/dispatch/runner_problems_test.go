package dispatch

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
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestIsolationProbeCancellation(t *testing.T) {
	for _, test := range []struct {
		name             string
		cached, complete bool
		workflowFailure  bool
		innerTimeout     bool
		completedPeer    string
		workflowLoad     bool
		cancel, killGit  bool
	}{
		{name: "unobserved cancellation"},
		{name: "cached cancellation", cached: true},
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
				parent, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
				if test.killGit {
					cancel()
					parent, cancel = context.WithTimeout(t.Context(), time.Second)
				}
				defer cancel()
				scheduler := &Scheduler{now: func() time.Time { return now }, leaseHold: func(context.Context) (func(), error) {
					holds++
					return func() { releases++ }, nil
				}, isolationReport: func(ctx context.Context) (isolation.Report, []runnerauth.Problem) {
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
				if cached && (!reflect.DeepEqual(report, wantReport) || !scheduler.lastIsolationAt.Equal(completed)) {
					t.Fatalf("cancelled workflow load changed the completed probe: report=%+v at %v", report, scheduler.lastIsolationAt)
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
				if !cached && scheduler.lastIsolation != nil {
					t.Fatal("incomplete capabilities were cached as completed")
				}
				if test.completedPeer != "" {
					followup = true
					now = now.Add(time.Minute)
					report, _ := scheduler.probeIsolation(t.Context())
					if !reflect.DeepEqual(report, wantReport) {
						t.Fatalf("later incomplete checks restored revoked capabilities: %+v, want %+v", report, wantReport)
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
		scheduler := &Scheduler{now: time.Now, isolationReport: func(ctx context.Context) (isolation.Report, []runnerauth.Problem) {
			calls++
			if calls <= 3 {
				<-ctx.Done()
				return isolation.Report{"example-project/codex": {}}, nil
			}
			timer := time.NewTimer(2 * time.Minute)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return isolation.Report{"example-project/codex": {}}, nil
			case <-timer.C:
				return isolation.Report{"example-project/codex": {isolation.NativeTrusted}}, nil
			}
		}}
		for attempt := 1; attempt <= 4; attempt++ {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
			report, problems := scheduler.probeIsolation(ctx)
			cancel()
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
