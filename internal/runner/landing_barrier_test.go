package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type barrierWorkspace struct {
	workspace.Backend
	result    gate.CommandResult
	err       error
	runs      int
	released  bool
	path      string
	repairs   int
	repairErr error
}

func (b *barrierWorkspace) AcquireLandingBarrierRunner(context.Context) (func() error, error) {
	return func() error { b.released = true; return nil }, nil
}
func (b *barrierWorkspace) LandingRepository(context.Context) string {
	return "https://github.com/example/repo"
}
func (b *barrierWorkspace) LandingBarrierHead(context.Context, string) (string, error) {
	return strings.Repeat("d", 40), nil
}
func (b *barrierWorkspace) RunLandingBarrier(ctx context.Context, _, _, _ string, callbacks workspace.LandingBarrierCallbacks) (gate.CommandResult, error) {
	b.runs++
	if b.err != nil {
		return b.result, b.err
	}
	if err := callbacks.Record(ctx, b.result); err != nil {
		return b.result, err
	}
	if b.result.ExitCode != 0 {
		b.repairs++
		if err := callbacks.Repair(ctx, workspace.Info{Path: b.path, Key: "barrier"}, b.result); err != nil {
			return b.result, err
		}
		if b.repairErr != nil {
			return b.result, b.repairErr
		}
		b.result.ExitCode = 0
		b.result.HeadSHA = strings.Repeat("e", 40)
		if err := callbacks.Record(ctx, b.result); err != nil {
			return b.result, err
		}
	}
	return b.result, nil
}

type barrierRepairAgent struct {
	catalogAgentBackend
	requests []AgentTurnRequest
	err      error
}

func (a *barrierRepairAgent) RunTurn(_ context.Context, request AgentTurnRequest, update AgentUpdateHandler) (AgentTurnResult, error) {
	a.requests = append(a.requests, request)
	if _, err := os.Stat(request.TempDir); err != nil {
		return AgentTurnResult{}, err
	}
	if err := update(AgentUpdate{Delta: "staged barrier repair"}); err != nil {
		return AgentTurnResult{}, err
	}
	return AgentTurnResult{ThreadID: "repair-thread", TurnID: "repair-turn"}, a.err
}

type barrierOwner struct {
	cancel   context.CancelFunc
	fail     bool
	claims   int
	heads    []string
	finishes int
	results  []*gate.CommandResult
	evidence []*gate.CommandResult
	repairs  []tracker.LandingBarrierRepair
}

func (o *barrierOwner) NextLandingBarrier(ctx context.Context, _, _, _ string, _ bool, observeHead func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error) {
	o.claims++
	head, err := observeHead(ctx, "")
	if err != nil {
		return tracker.LandingBarrier{}, false, err
	}
	o.heads = append(o.heads, head)
	return tracker.LandingBarrier{ID: "barrier", Repository: "https://github.com/example/repo"}, true, nil
}
func (o *barrierOwner) FinishLandingBarrier(_ context.Context, _ string, _ tracker.LandingBarrier, result *gate.CommandResult) error {
	o.finishes++
	o.results = append(o.results, result)
	if o.fail && o.finishes == 1 {
		return errors.New("publication unavailable")
	}
	o.cancel()
	return nil
}

func (o *barrierOwner) RecordLandingBarrier(_ context.Context, _ string, _ tracker.LandingBarrier, result *gate.CommandResult, repair *tracker.LandingBarrierRepair) error {
	if result != nil {
		copied := *result
		o.evidence = append(o.evidence, &copied)
	}
	if repair != nil {
		o.repairs = append(o.repairs, *repair)
	}
	return nil
}

func TestLandingBarrierPublication(t *testing.T) {
	for _, test := range []struct {
		name               string
		code               int
		instanceFailure    bool
		publicationFailure bool
		repairFailure      bool
	}{
		{"green", 0, false, false, false},
		{"red barrier repairs without work item", 7, false, false, false},
		{"publication retry retains repaired result", 7, false, true, false},
		{"instance failure releases claim", -1, true, false, false},
		{"repair agent failure keeps red evidence", 7, false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				backend := &barrierWorkspace{result: gate.CommandResult{Command: "make verify", ExitCode: test.code, HeadSHA: strings.Repeat("d", 40), Output: "failing check sentinel"}}
				if test.instanceFailure {
					backend.err = errors.New("workspace unavailable")
				}
				backend.path = t.TempDir()
				agent := &barrierRepairAgent{catalogAgentBackend: catalogAgentBackend{models: []AgentModel{{ID: "gpt-6.1-sol", Model: "gpt-6.1-sol", Default: true, SupportedReasoningEfforts: []string{"high", "medium", "low"}}}}}
				if test.repairFailure {
					agent.err = errors.New("backend protocol unavailable")
				}
				owner := &barrierOwner{cancel: cancel, fail: test.publicationFailure}
				cfg := config.Default()
				cfg.Gate.LandingMode, cfg.Gate.Run = gate.LandingRollingBarrier, "make verify"
				cfg = cfg.ForNativeTracker()
				workflow := config.Workflow{Config: cfg, SourceHash: policy.Digest([]byte("source")), Definition: config.ProjectDefinition{Revision: strings.Repeat("a", 40)}}
				if _, err := config.ResolvePolicy(workflow); err != nil {
					t.Fatal(err)
				}
				runtime, err := newAgentRuntime(workflow, nil, AgentBackendFactoryFunc(func(config.AgentBackend) (AgentBackend, error) { return agent, nil }))
				if err != nil {
					t.Fatal(err)
				}
				r := &Runner{agentRuntime: runtime, workspace: backend, projectID: "project", workflow: workflow, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				r.RunLandingBarriers(ctx, owner)
				wantFinishes := 1
				if test.publicationFailure {
					wantFinishes = 2
				}
				if backend.runs != 1 || owner.claims != 1 || owner.finishes != wantFinishes || !backend.released || len(owner.heads) != 1 || owner.heads[0] != strings.Repeat("d", 40) {
					t.Fatalf("runs=%d claims=%d finishes=%d released=%v heads=%v", backend.runs, owner.claims, owner.finishes, backend.released, owner.heads)
				}
				for _, result := range owner.results {
					if test.instanceFailure || test.repairFailure {
						if result != nil {
							t.Fatalf("instance failure recorded gate result: %+v", result)
						}
						continue
					}
					if result == nil || result.ExitCode != 0 || result.Command != "make verify" {
						t.Fatalf("result=%+v", result)
					}
				}
				wantsRepair := test.code != 0 && !test.instanceFailure
				if wantsRepair {
					if backend.repairs != 1 || len(agent.requests) != 1 || len(owner.repairs) != 1 || len(owner.evidence) < 1 || owner.evidence[0].ExitCode != 7 || owner.repairs[0].ThreadID != "repair-thread" || owner.repairs[0].Output != "staged barrier repair" {
						t.Fatalf("repair evidence=%+v command evidence=%+v requests=%+v", owner.repairs, owner.evidence, agent.requests)
					}
					request := agent.requests[0]
					if request.Workspace != backend.path || !strings.Contains(request.Prompt, "failing check sentinel") || !strings.Contains(request.Prompt, strings.Repeat("d", 40)) {
						t.Fatalf("repair request=%+v", request)
					}
					if test.repairFailure && !strings.Contains(owner.repairs[0].Error, "backend protocol unavailable") {
						t.Fatalf("repair error=%+v", owner.repairs[0])
					}
				} else if len(agent.requests) != 0 {
					t.Fatal("green or infrastructure failure started source repair")
				}

			})
		})
	}
}
