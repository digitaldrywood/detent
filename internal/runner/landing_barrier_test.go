package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
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
	result   gate.CommandResult
	err      error
	runs     int
	released bool
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
func (b *barrierWorkspace) RunLandingBarrier(context.Context, string, string, string) (gate.CommandResult, error) {
	b.runs++
	return b.result, b.err
}

type barrierOwner struct {
	cancel   context.CancelFunc
	fail     bool
	claims   int
	heads    []string
	finishes int
	results  []*gate.CommandResult
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

func TestLandingBarrierPublication(t *testing.T) {
	for _, test := range []struct {
		name               string
		code               int
		instanceFailure    bool
		publicationFailure bool
	}{
		{"green", 0, false, false},
		{"red", 7, false, false},
		{"publication retry retains red result", 7, false, true},
		{"instance failure releases claim", -1, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				backend := &barrierWorkspace{result: gate.CommandResult{Command: "make verify", ExitCode: test.code}}
				if test.instanceFailure {
					backend.err = errors.New("workspace unavailable")
				}
				owner := &barrierOwner{cancel: cancel, fail: test.publicationFailure}
				cfg := config.Default()
				cfg.Gate.LandingMode, cfg.Gate.Run = gate.LandingRollingBarrier, "make verify"
				cfg = cfg.ForNativeTracker()
				workflow := config.Workflow{Config: cfg, SourceHash: policy.Digest([]byte("source")), Definition: config.ProjectDefinition{Revision: strings.Repeat("a", 40)}}
				if _, err := config.ResolvePolicy(workflow); err != nil {
					t.Fatal(err)
				}
				r := &Runner{workspace: backend, projectID: "project", workflow: workflow, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				r.RunLandingBarriers(ctx, owner)
				wantFinishes := 1
				if test.publicationFailure {
					wantFinishes = 2
				}
				if backend.runs != 1 || owner.claims != 1 || owner.finishes != wantFinishes || !backend.released || len(owner.heads) != 1 || owner.heads[0] != strings.Repeat("d", 40) {
					t.Fatalf("runs=%d claims=%d finishes=%d released=%v heads=%v", backend.runs, owner.claims, owner.finishes, backend.released, owner.heads)
				}
				for _, result := range owner.results {
					if test.instanceFailure {
						if result != nil {
							t.Fatalf("instance failure recorded gate result: %+v", result)
						}
						continue
					}
					if result == nil || result.ExitCode != test.code || result.Command != "make verify" {
						t.Fatalf("result=%+v", result)
					}
				}
			})
		})
	}
}

type repairingBarrierWorkspace struct {
	barrierWorkspace
	head      string
	prepared  int
	published int
	released  int
}

func (b *repairingBarrierWorkspace) PrepareLandingBarrierRepair(context.Context, string, string) (string, string, func() error, error) {
	b.prepared++
	return "/repair", b.head, func() error { b.released++; return nil }, nil
}

func (b *repairingBarrierWorkspace) PublishLandingBarrierRepair(_ context.Context, path, _, head string) (string, error) {
	b.published++
	if path != "/repair" || head != b.head {
		return "", errors.New("unexpected repair workspace")
	}
	return strings.Repeat("f", 40), nil
}

type repeatingBarrierOwner struct {
	cancel context.CancelFunc
	reds   int
	claims int
}

func (o *repeatingBarrierOwner) NextLandingBarrier(context.Context, string, string, string, bool, func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error) {
	o.claims++
	if o.claims > o.reds {
		o.cancel()
		return tracker.LandingBarrier{}, false, nil
	}
	return tracker.LandingBarrier{ID: "barrier", Repository: "https://github.com/example/repo", BaseRef: "main"}, true, nil
}

func (o *repeatingBarrierOwner) FinishLandingBarrier(context.Context, string, tracker.LandingBarrier, *gate.CommandResult) error {
	return nil
}

func TestRedLandingBarrierRepairsItself(t *testing.T) {
	head := strings.Repeat("e", 40)
	for _, test := range []struct {
		name          string
		preparedHead  string
		reds          int
		wantTurns     int
		wantPublished int
	}{
		{name: "red head is repaired and published", preparedHead: head, reds: 1, wantTurns: 1, wantPublished: 1},
		{name: "same red head is repaired once", preparedHead: head, reds: 2, wantTurns: 1, wantPublished: 1},
		{name: "moved base skips the stale repair", preparedHead: strings.Repeat("a", 40), reds: 1, wantTurns: 0, wantPublished: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				backend := &repairingBarrierWorkspace{barrierWorkspace: barrierWorkspace{result: gate.CommandResult{Command: "make verify", HeadSHA: head, ExitCode: 7, Output: "barrier failure sentinel"}}, head: test.preparedHead}
				owner := &repeatingBarrierOwner{cancel: cancel, reds: test.reds}
				cfg := config.Default()
				cfg.Gate.LandingMode, cfg.Gate.Run = gate.LandingRollingBarrier, "make verify"
				cfg = cfg.ForNativeTracker()
				workflow := config.Workflow{Config: cfg, SourceHash: policy.Digest([]byte("source")), Definition: config.ProjectDefinition{Revision: strings.Repeat("a", 40)}}
				router, err := NewRouter([]Route{{Name: "code", Role: RoleCode, BackendID: "codex", Default: true}})
				if err != nil {
					t.Fatal(err)
				}
				agent := &fakeCodexClient{}
				r := &Runner{workspace: backend, projectID: "project", workflow: workflow, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), agentRuntime: agentRuntime{backends: map[string]AgentBackend{"codex": agent}, router: router}}
				r.RunLandingBarriers(ctx, owner)
				if agent.calls != test.wantTurns || backend.published != test.wantPublished || backend.prepared != backend.released {
					t.Fatalf("turns=%d published=%d prepared=%d released=%d", agent.calls, backend.published, backend.prepared, backend.released)
				}
				if test.wantTurns > 0 && (agent.request.Workspace != "/repair" || !strings.Contains(agent.request.Prompt, "barrier failure sentinel") || !strings.Contains(agent.request.Prompt, head)) {
					t.Fatalf("repair request=%+v", agent.request)
				}
			})
		})
	}
}
