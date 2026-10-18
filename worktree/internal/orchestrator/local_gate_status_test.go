package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	runpkg "github.com/digitaldrywood/detent/internal/runner"
)

type localGateStatusConnector struct {
	connector.Connector
	posted   []string
	postErr  error
	hydrated int
}

func (c *localGateStatusConnector) PostCommitStatus(_ context.Context, repository, sha, statusContext, _ string) error {
	if c.postErr != nil {
		return c.postErr
	}
	c.posted = append(c.posted, repository+"@"+sha+"="+statusContext)
	return nil
}

func (c *localGateStatusConnector) HydratePullRequest(_ context.Context, issue connector.Issue) (connector.Issue, error) {
	c.hydrated++
	issue.Title = "refreshed"
	return issue, nil
}

func TestPostValidatedGateStatusOnlyForTheValidatedHead(t *testing.T) {
	t.Parallel()

	head := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	tests := []struct {
		name          string
		localStatus   string
		validatedHead string
		validatedRun  string
		prHead        string
		prChecks      []connector.PullRequestCheck
		prRequired    []connector.PullRequestCheck
		postErr       error
		wantPosted    []string
		wantRefreshed bool
		wantErr       bool
	}{
		{name: "missing owned required status", localStatus: "local-gate", validatedHead: head, prHead: head, prRequired: []connector.PullRequestCheck{{Name: "local-gate", Status: "missing", Conclusion: "missing"}}, wantPosted: []string{"example/repo@" + head + "=local-gate"}, wantRefreshed: true},
		{name: "validated current head", localStatus: "local-gate", validatedHead: head, prHead: head, wantPosted: []string{"example/repo@" + head + "=local-gate"}, wantRefreshed: true},
		{name: "head changed after validation", localStatus: "local-gate", validatedHead: head, prHead: other},
		{name: "gate not run by Detent", localStatus: "local-gate", validatedHead: "", prHead: head},
		{name: "no local status configured", localStatus: "", validatedHead: head, prHead: head},
		{name: "gate command changed after validation", localStatus: "local-gate", validatedHead: head, validatedRun: "make lint", prHead: head},
		{name: "success already on the head", localStatus: "local-gate", validatedHead: head, prHead: head, prChecks: []connector.PullRequestCheck{{Name: "local-gate", Status: "success", Conclusion: "success"}}},
		{name: "pending status on the head", localStatus: "local-gate", validatedHead: head, prHead: head, prChecks: []connector.PullRequestCheck{{Name: "local-gate", Status: "pending", Conclusion: "pending"}}, wantPosted: []string{"example/repo@" + head + "=local-gate"}, wantRefreshed: true},
		{name: "post fails", localStatus: "local-gate", validatedHead: head, prHead: head, postErr: errors.New("forbidden"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &localGateStatusConnector{postErr: tt.postErr}
			cfg := Config{}
			cfg.AutoPromote.Gate = gate.Config{Kind: gate.KindCommand, Run: "make check-fast", LocalStatus: tt.localStatus}
			orch := &Orchestrator{cfg: cfg, connector: fake, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			issue := connector.Issue{ID: "1", Identifier: "example/repo#7", PRRepository: "example/repo", PullRequest: &connector.PullRequest{Number: 7, HeadSHA: tt.prHead, Checks: tt.prChecks, RequiredCheckFailures: tt.prRequired}}

			if tt.prRequired != nil {
				if missing := mergeWorkerMissingRequiredChecks(issue, tt.localStatus); len(missing) != 0 {
					t.Fatalf("owned context entered missing-check accounting: %v", missing)
				}
				if mergeWorkerMissingRequiredChecksPropagating(issue, 1, tt.localStatus) {
					t.Fatal("owned context waited for external propagation")
				}
				if pending := mergeWorkerCurrentHeadCIPendingChecks(issue, tt.localStatus); len(pending) != 0 {
					t.Fatalf("owned context entered current-head CI wait: %v", pending)
				}
			}
			event := runpkg.Completion{Result: runpkg.RunResult{GateValidatedHead: tt.validatedHead, GateValidatedRun: tt.validatedRun}}
			if tt.validatedRun == "" && tt.validatedHead != "" {
				event.Result.GateValidatedRun = "make check-fast"
			}

			got, err := orch.postValidatedGateStatus(context.Background(), fake, event, issue)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, want error %v", err, tt.wantErr)
			}
			if strings.Join(fake.posted, ",") != strings.Join(tt.wantPosted, ",") {
				t.Fatalf("posted = %v, want %v", fake.posted, tt.wantPosted)
			}
			if refreshed := got.Title == "refreshed"; refreshed != tt.wantRefreshed || (fake.hydrated == 1) != tt.wantRefreshed {
				t.Fatalf("refreshed = %v (hydrations %d), want %v", refreshed, fake.hydrated, tt.wantRefreshed)
			}
		})
	}
}
