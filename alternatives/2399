package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestValidatorAcceptanceContextIdentity(t *testing.T) {
	issue := connector.Issue{ID: "mobile200", Identifier: "digitaldrywood/pyroapex-mobile#200", Description: "Require an archived failure or reproduction", PullRequest: &connector.PullRequest{Number: 207, BaseSHA: "base", HeadSHA: "5d9dffe"}}
	before := validatorStageIdentityForIssue(issue)
	issue.Description = "An explicitly unproven intermittent cause is acceptable"
	after := validatorStageIdentityForIssue(issue)
	if before.Key == after.Key {
		t.Fatal("unchanged head reused validator identity after authorized acceptance changed")
	}
}

func TestValidatorContextMemoReuse(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	for _, verdict := range []string{gate.ValidatorVerdictRework, gate.ValidatorVerdictPass, gate.ValidatorVerdictWait} {
		t.Run(verdict, func(t *testing.T) {
			issue := connector.Issue{ID: "mobile200", Identifier: "digitaldrywood/pyroapex-mobile#200", Description: "Require reproduction", PullRequest: &connector.PullRequest{Number: 207, BaseSHA: "base", HeadSHA: "same-head"}}
			memo := openValidatorMemoStore(t)
			o := &Orchestrator{cfg: Config{Project: scheduler.ProjectCandidate{ID: "detent"}}, validatorMemo: memo}
			identity := validatorStageIdentityForIssue(issue)
			result := gate.ValidatorResult{Submitted: true, Verdict: verdict, Score: 1, DiffDigest: "diff"}
			o.recordValidatorVerdict(t.Context(), issue, identity, result, time.Now())
			// A new orchestrator must load the exact context after a storage reload.
			if _, _, ok := o.validatorStageResult(t.Context(), issue); !ok {
				t.Fatal("same context did not reload")
			}
			if _, _, ok := o.validatorStageResult(t.Context(), issue); !ok {
				t.Fatal("same context did not reuse in memory")
			}
			issue.Description = "Unproven intermittent cause acceptable"
			if _, _, ok := o.validatorStageResult(t.Context(), issue); ok {
				t.Fatal("old verdict reused in memory for revised acceptance")
			}
			restarted := &Orchestrator{cfg: o.cfg, validatorMemo: memo}
			if _, _, ok := restarted.validatorStageResult(t.Context(), issue); ok {
				t.Fatal("old verdict reused from storage for revised acceptance")
			}
			b := validatorStageIdentityForIssue(issue)
			o.recordValidatorVerdict(t.Context(), issue, b, gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, Score: 1, DiffDigest: "diff"}, time.Now())
			// Even a late write for A cannot replace a stored B or a different PR.
			o.recordValidatorVerdict(t.Context(), issue, identity, result, time.Now())
			current, _, ok := restarted.validatorStageResult(t.Context(), issue)
			if !ok || current.Verdict != gate.ValidatorVerdictPass {
				t.Fatal("late A overrode B")
			}
			issue.PullRequest = &connector.PullRequest{Number: 208, BaseSHA: "base", HeadSHA: "same-head"}
			if _, _, ok := restarted.validatorStageResult(t.Context(), issue); ok {
				t.Fatal("different PR reused verdict")
			}
		})
	}
}

func TestValidatorLegacyContextNotReusable(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	issue := connector.Issue{ID: "legacy", Identifier: "o/r#1", PullRequest: &connector.PullRequest{Number: 2, BaseSHA: "base", HeadSHA: "head"}}
	memo := openValidatorMemoStore(t)
	for _, v := range []string{gate.ValidatorVerdictPass, gate.ValidatorVerdictRework, gate.ValidatorVerdictWait} {
		t.Run(v, func(t *testing.T) {
			err := memo.RecordValidatorVerdict(t.Context(), store.ValidatorVerdict{ProjectID: "detent", IssueID: issue.ID, HeadSHA: "head", Repository: "o/r", PRNumber: new(int64(2)), BaseSHA: "base", DiffDigest: "diff", Submitted: true, Verdict: v, RecordedAt: time.Now()})
			if err != nil {
				t.Fatal(err)
			}
			o := &Orchestrator{cfg: Config{Project: scheduler.ProjectCandidate{ID: "detent"}}, validatorMemo: memo}
			if _, _, ok := o.validatorStageResult(t.Context(), issue); ok {
				t.Fatal("legacy context treated as current verdict")
			}
		})
	}
}

func TestValidatorChangedContextReviewsOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	for _, verdict := range []string{gate.ValidatorVerdictRework, gate.ValidatorVerdictPass, gate.ValidatorVerdictWait} {
		t.Run(verdict, func(t *testing.T) {
			cfg := autoPromoteValidatorTestConfig()
			issue := connector.Issue{ID: "changed", Identifier: "o/r#200", Description: "A", PullRequest: &connector.PullRequest{Number: 207, BaseSHA: "base", HeadSHA: "head"}}
			tracker := &currentValidatorTaskConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, body: "A"}
			validator := &autoPromoteTickValidator{result: gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, Score: 1, DiffDigest: "diff"}}
			o := &Orchestrator{cfg: cfg, connector: tracker, validator: validator, validatorMemo: openValidatorMemoStore(t)}
			state := newState(cfg)
			o.recordValidatorVerdict(t.Context(), issue, validatorStageIdentityForIssue(issue, cfg.AutoPromote.Gate), gate.ValidatorResult{Submitted: true, Verdict: verdict, Score: 1, DiffDigest: "diff"}, time.Now())
			o.startValidatorStage(t.Context(), &state, issue, time.Now())
			if len(validator.Requests()) != 0 {
				t.Fatal("unchanged context started review")
			}
			body := "B: unproven cause acceptable"
			if verdict == gate.ValidatorVerdictPass {
				body = "B: require reproduction and physical hardware"
			}
			tracker.setBody(body)
			o.startValidatorStage(t.Context(), &state, issue, time.Now())
			o.validatorWG.Wait()
			// Keep using the stale board snapshot: each lookup must read B.
			o.startValidatorStage(t.Context(), &state, issue, time.Now())
			o.validatorWG.Wait()
			requests := validator.Requests()
			if len(requests) != 1 || requests[0].Issue.Description != body {
				t.Fatalf("fresh review requests=%+v", requests)
			}
		})
	}
}

type currentValidatorTaskConnector struct {
	*autoPromoteTickConnector
	mu   sync.Mutex
	body string
	err  error
}

func (c *currentValidatorTaskConnector) FetchValidationIssue(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	issue.Description = c.body
	return issue, c.err
}
func (c *currentValidatorTaskConnector) setBody(body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.body = body
}

func TestValidatorContextSchedulesOnceAndRejectsHeldResult(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	cfg := autoPromoteValidatorTestConfig()
	issue := connector.Issue{ID: "held", Identifier: "o/r#200", Description: "A", PullRequest: &connector.PullRequest{Number: 207, BaseSHA: "base", HeadSHA: "head", State: "OPEN"}}
	tracker := &currentValidatorTaskConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, body: "A"}
	validator := newBlockingAutoPromoteValidatorRunner()
	o := &Orchestrator{cfg: cfg, connector: tracker, validator: validator, validatorMemo: openValidatorMemoStore(t), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	state := newState(cfg)
	o.startValidatorStage(t.Context(), &state, issue, time.Now())
	select {
	case req := <-validator.started:
		if req.Issue.Description != "A" {
			t.Fatal("wrong input")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("review did not start")
	}
	o.startValidatorStage(t.Context(), &state, issue, time.Now())
	// Current task B is published while A is held. Preserve a B result even if
	// A completes afterwards, and never describe A as current B evidence.
	tracker.setBody("B")
	revised := issue
	revised.Description = "B"
	b := validatorStageIdentityForIssue(revised, cfg.AutoPromote.Gate)
	o.recordValidatorVerdict(t.Context(), revised, b, gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictWait, DiffDigest: "diff"}, time.Now())
	validator.Release()
	select {
	case <-validator.done:
	case <-time.After(5 * time.Second):
		t.Fatal("review did not finish")
	}
	a := validatorStageIdentityForIssue(issue, cfg.AutoPromote.Gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		o.validatorMu.Lock()
		_, running := o.validatorRuns[a.Key]
		_, published := o.validatorResults[a.Key]
		o.validatorMu.Unlock()
		if !running {
			if published {
				t.Fatal("held A published after B")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("held stage did not retire")
		}
		runtime.Gosched()
	}
	if _, err := o.validatorMemo.ValidatorVerdict(t.Context(), store.ValidatorVerdictKey{ProjectID: "detent", IssueID: issue.ID, HeadSHA: "head", ContextDigest: a.ContextDigest}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale A persisted: %v", err)
	}
	result, _, ok := o.validatorStageResult(t.Context(), revised)
	if !ok || result.Verdict != gate.ValidatorVerdictWait {
		t.Fatal("B verdict lost")
	}
}

func TestApplyValidatorRefreshesBeforeReusing(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		err        error
		reuse      bool
	}{
		{name: "unchanged", body: "A", reuse: true},
		{name: "changed", body: "B"},
		{name: "unavailable", body: "A", err: errors.New("read unavailable")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := autoPromoteValidatorTestConfig()
			issue := connector.Issue{ID: "fresh", Identifier: "o/r#200", Description: "A", PullRequest: &connector.PullRequest{Number: 207, BaseSHA: "base", HeadSHA: "head"}}
			identity := validatorStageIdentityForIssue(issue, cfg.AutoPromote.Gate)
			tracker := &currentValidatorTaskConnector{autoPromoteTickConnector: &autoPromoteTickConnector{}, body: tt.body, err: tt.err}
			o := &Orchestrator{cfg: cfg, connector: tracker, validatorResults: map[string]validatorStageResult{identity.Key: {Result: gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictRework}}}}
			state := newState(cfg)
			summary := AutoPromoteSummary{}
			_, ready := o.applyValidatorStage(t.Context(), &state, issue, &summary, AutoPromoteDecision{Reason: AutoPromoteReasonValidatorMissing}, cfg.AutoPromote, time.Now())
			if ready != tt.reuse {
				t.Fatalf("used old rejection=%v, want %v", ready, tt.reuse)
			}
		})
	}
}

func (c *autoPromoteTickConnector) FetchValidationIssue(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	for _, current := range c.stateIssues {
		if current.ID == issue.ID {
			issue.Title = current.Title
			issue.Description = current.Description
			break
		}
	}
	return issue, nil
}
