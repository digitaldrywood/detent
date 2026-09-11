package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

func TestValidatorRuntimeObservable(t *testing.T) {
	for _, tt := range []struct {
		name    string
		failure error
	}{
		{name: "success"}, {name: "failure", failure: errors.New("validator failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			issue := connector.Issue{ID: "issue-2486", Identifier: "digitaldrywood/detent#2486", State: "Rework", Labels: []string{"original"}, PullRequest: &connector.PullRequest{Number: 2487, URL: "https://github.com/digitaldrywood/detent/pull/2487", HeadSHA: "head", State: "OPEN"}}
			validator := &observableValidator{started: make(chan ValidatorRequest, 1), release: make(chan struct{}), failure: tt.failure}
			orch, err := New(Config{}, Dependencies{Connector: hydratingDispatchConnector{issue: issue}})
			if err != nil {
				t.Fatal(err)
			}
			orch.validator = validator
			state := newState(orch.cfg)
			// An implementation for the same issue must not be overwritten by its validator.
			state.Running[issue.ID] = Running{Issue: issue, DetentSessionID: 6058}
			orch.startTick(&state, now)
			orch.publishState(&state)
			orch.startValidatorStage(t.Context(), &state, issue, now)
			release := sync.OnceFunc(func() { close(validator.release) })
			defer func() { release(); orch.validatorWG.Wait() }()
			request := <-validator.started
			if request.OnUsageUpdate == nil {
				t.Fatal("validator launched without runtime usage registration")
			}
			if err := request.OnUsageUpdate(runpkg.UsageUpdate{DetentSessionID: 6059, SessionID: "validator-session", RSSBytes: 256, RSSObservedAt: now}); err != nil {
				t.Fatal(err)
			}
			for _, completion := range []bool{false, true} {
				if completion {
					orch.startCompletion(&state)
				}
				observed, err := orch.State(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				for _, running := range observed.Running {
					if running.DetentSessionID == 6059 {
						if running.Issue.Labels[0] != "original" {
							t.Fatal("snapshot reader mutated validator registry")
						}
						running.Issue.Labels[0] = "changed by reader"
					}
				}
				snapshot := observed.Snapshot(now)
				if len(snapshot.Running) != 2 {
					t.Fatalf("Running = %#v, want implementation and validator", snapshot.Running)
				}
				found := false
				for _, running := range snapshot.Running {
					if running.DetentSessionID == 6059 {
						found = true
						if running.RSSBytes != 256 {
							t.Fatalf("validator memory = %d", running.RSSBytes)
						}
					}
				}
				if !found {
					t.Fatal("validator session missing from Running")
				}
			}
			release()
			orch.validatorWG.Wait()
			if got := orch.publishedState().Snapshot(now).Running; len(got) != 1 || got[0].DetentSessionID != 6058 {
				t.Fatalf("after validator exit Running = %#v", got)
			}
		})
	}
}

type observableValidator struct {
	started chan ValidatorRequest
	release chan struct{}
	failure error
}

func (v *observableValidator) Validate(_ context.Context, req ValidatorRequest) (gate.ValidatorResult, error) {
	v.started <- req
	<-v.release
	return gate.ValidatorResult{Submitted: true, Verdict: gate.ValidatorVerdictPass, Score: 1}, v.failure
}
