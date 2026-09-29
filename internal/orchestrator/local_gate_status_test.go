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
		prHead        string
		postErr       error
		wantPosted    []string
		wantRefreshed bool
		wantErr       bool
	}{
		{name: "validated current head", localStatus: "local-gate", validatedHead: head, prHead: head, wantPosted: []string{"example/repo@" + head + "=local-gate"}, wantRefreshed: true},
		{name: "head changed after validation", localStatus: "local-gate", validatedHead: head, prHead: other},
		{name: "gate not run by Detent", localStatus: "local-gate", validatedHead: "", prHead: head},
		{name: "no local status configured", localStatus: "", validatedHead: head, prHead: head},
		{name: "post fails", localStatus: "local-gate", validatedHead: head, prHead: head, postErr: errors.New("forbidden"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fake := &localGateStatusConnector{postErr: tt.postErr}
			cfg := Config{}
			cfg.AutoPromote.Gate = gate.Config{Kind: gate.KindCommand, Run: "make check-fast", LocalStatus: tt.localStatus}
			orch := &Orchestrator{cfg: cfg, connector: fake, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
			issue := connector.Issue{ID: "1", Identifier: "example/repo#7", PRRepository: "example/repo", PullRequest: &connector.PullRequest{Number: 7, HeadSHA: tt.prHead}}
			event := runpkg.Completion{Result: runpkg.RunResult{GateValidatedHead: tt.validatedHead}}

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
