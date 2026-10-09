package runner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type barrierWorkspace struct {
	workspace.Backend
	result      gate.CommandResult
	err         error
	acquireErrs []error
	acquires    int
	runs        int
	released    bool
}

func (b *barrierWorkspace) AcquireLandingBarrierRunner(context.Context) (func() error, error) {
	b.acquires++
	if len(b.acquireErrs) > 0 {
		err := b.acquireErrs[0]
		b.acquireErrs = b.acquireErrs[1:]
		if err != nil {
			return nil, err
		}
	}
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

func TestLandingBarrierRunnerLockRetry(t *testing.T) {
	held := &instancelock.HeldError{Path: "detent-landing-barrier.lock"}
	for _, test := range []struct {
		name         string
		acquireErrs  []error
		wantAcquires int
		wantRuns     int
		wantClaims   int
	}{
		{name: "free lock", wantAcquires: 1, wantRuns: 1, wantClaims: 1},
		{name: "held lock retries then proceeds", acquireErrs: []error{held, held}, wantAcquires: 3, wantRuns: 1, wantClaims: 1},
		{name: "unavailable lock stops", acquireErrs: []error{errors.New("git common dir unavailable")}, wantAcquires: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				backend := &barrierWorkspace{result: gate.CommandResult{Command: "make verify"}, acquireErrs: test.acquireErrs}
				owner := &barrierOwner{cancel: cancel}
				cfg := config.Default()
				cfg.Gate.LandingMode, cfg.Gate.Run = gate.LandingRollingBarrier, "make verify"
				cfg = cfg.ForNativeTracker()
				workflow := config.Workflow{Config: cfg, SourceHash: policy.Digest([]byte("source")), Definition: config.ProjectDefinition{Revision: strings.Repeat("a", 40)}}
				r := &Runner{workspace: backend, projectID: "project", workflow: workflow, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
				r.RunLandingBarriers(ctx, owner)
				if backend.acquires != test.wantAcquires || backend.runs != test.wantRuns || owner.claims != test.wantClaims {
					t.Fatalf("acquires=%d runs=%d claims=%d, want %d/%d/%d", backend.acquires, backend.runs, owner.claims, test.wantAcquires, test.wantRuns, test.wantClaims)
				}
				if backend.released != (test.wantRuns > 0) {
					t.Fatalf("released=%v after %d runs", backend.released, backend.runs)
				}
			})
		})
	}
}

type repairingBarrierWorkspace struct {
	barrierWorkspace
	head       string
	prepared   int
	published  int
	released   int
	verifyExit int
	verified   []string
	commits    []string
	firstBad   int
	current    string
	reverted   string
}

func (b *repairingBarrierWorkspace) VerifyLandingBarrierRepair(_ context.Context, path, head, command string, failed []string) (gate.CommandResult, bool, error) {
	b.verified = failed
	if path != "/repair" || head != "" && head != b.head || command != "make verify" {
		return gate.CommandResult{}, false, errors.New("unexpected repair verification")
	}
	if head == "" && b.commits != nil {
		if slices.Index(b.commits, b.current) >= b.firstBad {
			return gate.CommandResult{ExitCode: 1}, true, nil
		}
		return gate.CommandResult{}, true, nil
	}
	if b.reverted != "" {
		return gate.CommandResult{}, true, nil
	}
	return gate.CommandResult{ExitCode: b.verifyExit}, true, nil
}

func (b *repairingBarrierWorkspace) LandingBarrierCommits(context.Context, string, string, string) ([]string, error) {
	return b.commits, nil
}

func (b *repairingBarrierWorkspace) CheckoutLandingBarrierCommit(_ context.Context, _ string, commit string) error {
	b.current = commit
	return nil
}

func (b *repairingBarrierWorkspace) RevertLandingBarrierCommit(_ context.Context, _ string, head, commit string) error {
	if head != b.head {
		return errors.New("revert from unexpected head")
	}
	b.reverted = commit
	return nil
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
	green  string
}

func (o *repeatingBarrierOwner) NextLandingBarrier(context.Context, string, string, string, bool, func(context.Context, string) (string, error)) (tracker.LandingBarrier, bool, error) {
	o.claims++
	if o.claims > o.reds {
		o.cancel()
		return tracker.LandingBarrier{}, false, nil
	}
	return tracker.LandingBarrier{ID: "barrier", Repository: "https://github.com/example/repo", BaseRef: "main", GreenHead: o.green}, true, nil
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
		verifyExit    int
		green         string
		commits       []string
		firstBad      int
		wantTurns     int
		wantPublished int
		wantReverted  string
	}{
		{name: "unverified repair falls back to reverting the culprit", preparedHead: head, reds: 1, verifyExit: 1, green: "g0", commits: []string{"c1", "c2", "c3", "c4", head}, firstBad: 2, wantTurns: 1, wantPublished: 1, wantReverted: "c3"},
		{name: "first commit after green is the culprit", preparedHead: head, reds: 1, verifyExit: 1, green: "g0", commits: []string{"c1", head}, firstBad: 0, wantTurns: 1, wantPublished: 1, wantReverted: "c1"},
		{name: "red head is repaired and published", preparedHead: head, reds: 1, wantTurns: 1, wantPublished: 1},
		{name: "same red head is repaired once", preparedHead: head, reds: 2, wantTurns: 1, wantPublished: 1},
		{name: "moved base repairs failures that still fail", preparedHead: strings.Repeat("a", 40), reds: 1, verifyExit: 1, wantTurns: 1, wantPublished: 0},
		{name: "moved base skips failures fixed upstream", preparedHead: strings.Repeat("a", 40), reds: 1, wantTurns: 0, wantPublished: 0},
		{name: "unverified repair is not published", preparedHead: head, reds: 1, verifyExit: 1, wantTurns: 1, wantPublished: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				failedLine := "detent-barrier-failed: go example.com/pkg:TestBroken"
				backend := &repairingBarrierWorkspace{barrierWorkspace: barrierWorkspace{result: gate.CommandResult{Command: "make verify", HeadSHA: head, ExitCode: 7, Output: "barrier failure sentinel\n" + failedLine + "\ndetent-barrier-failed: browser \n"}}, head: test.preparedHead, verifyExit: test.verifyExit, commits: test.commits, firstBad: test.firstBad}
				owner := &repeatingBarrierOwner{cancel: cancel, reds: test.reds, green: test.green}
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
				if backend.reverted != test.wantReverted {
					t.Fatalf("reverted=%q want %q", backend.reverted, test.wantReverted)
				}
				if test.wantReverted != "" && !strings.Contains(agent.request.Prompt, test.wantReverted) {
					t.Fatalf("repair prompt omits culprit %q", test.wantReverted)
				}
				if agent.calls != test.wantTurns || backend.published != test.wantPublished || backend.prepared != backend.released {
					t.Fatalf("turns=%d published=%d prepared=%d released=%d", agent.calls, backend.published, backend.prepared, backend.released)
				}
				if test.wantTurns > 0 && (agent.request.Workspace != "/repair" || !strings.Contains(agent.request.Prompt, "barrier failure sentinel") || !strings.Contains(agent.request.Prompt, head) || !strings.Contains(agent.request.Prompt, "re-run only them")) {
					t.Fatalf("repair request=%+v", agent.request)
				}
				if test.wantTurns > 0 && (len(backend.verified) != 1 || backend.verified[0] != failedLine) {
					t.Fatalf("verified failures=%q, want only %q", backend.verified, failedLine)
				}
			})
		})
	}
}
