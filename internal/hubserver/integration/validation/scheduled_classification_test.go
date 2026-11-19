package validation_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type ciDecisionBackend struct {
	t        *testing.T
	expected bool
	decision progresscheck.CIDecision
	fail     bool
}

func (b *ciDecisionBackend) RunTurn(_ context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	if !b.expected {
		b.t.Fatal("replay or invalid evidence reached the AI backend")
	}
	b.expected = false
	if !request.TextOnly || !request.ReadOnly || request.MaxTurns != 1 || request.ToolInstructions != progresscheck.CIInstructions || request.SupplementalTools {
		b.t.Fatal("CI decision backend may only generate text from evidence")
	}
	var input progresscheck.CIFailure
	if err := json.Unmarshal([]byte(request.Prompt), &input); err != nil || input.LogTail == "" || !slices.Equal(input.ChangedFiles, []string{"internal/example.go"}) || input.Step != "Run check" || input.ExitStatus != "failure" {
		b.t.Fatalf("missing CI decision evidence: %+v, %v", input, err)
	}
	if b.fail {
		return runner.AgentTurnResult{}, errors.New("provider unavailable")
	}
	raw, err := json.Marshal(b.decision)
	if err != nil {
		return runner.AgentTurnResult{}, err
	}
	return runner.AgentTurnResult{}, update(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: string(raw)})
}

func TestScheduledCIClassificationPersistsOneDecision(t *testing.T) {
	t.Parallel()
	backend := &ciDecisionBackend{}
	hub := hubfixture.Open(t, hubserver.Config{
		Conversation: &hubserver.ConversationConfig{Backend: backend, Model: "configured-model"},
	})
	storage := hub.Repository
	repository := "owner/repository"
	for _, test := range []struct {
		name, log string
		decision  progresscheck.CIDecision
		fail      bool
	}{
		{"Go test", "--- FAIL: TestExample (0s)", progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "TestExample", Reason: "assertion failed"}, false},
		{"timeout with location", "internal/client.go:42:3: network timeout", progresscheck.CIDecision{Outcome: progresscheck.CIInfrastructure, Target: "download timeout", Reason: "external network unavailable"}, false},
		{"unknown", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "evidence insufficient"}, false},
		{"provider unavailable", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			*backend = ciDecisionBackend{t: t, decision: test.decision, fail: test.fail}
			org := hub.Primary
			firstProject := hubtest.Project(t, storage, org, hubtest.ProjectOptions{}).ID
			secondProject := hubtest.Project(t, storage, org, hubtest.ProjectOptions{}).ID
			if err := storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				for _, project := range []tracker.ProjectID{firstProject, secondProject} {
					record, err := w.ProjectRecord(t.Context(), project)
					if err != nil {
						return err
					}
					record.Settings.CheckoutRepository = repository
					if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			actor := hubtest.Credential(t, storage, org, hubtest.CredentialOptions{KeyScope: "write", Projects: []tracker.ProjectID{firstProject, secondProject}})
			call := func(project tracker.ProjectID, input progresscheck.CIFailure, status int) *httptest.ResponseRecorder {
				t.Helper()
				path := hubfixture.ProjectBase(org, project) + "/scheduled-ci-decisions"
				response := hub.Request(t, http.MethodPost, path, actor.Token, input)
				if response.Code != status {
					t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
				}
				return response
			}
			input := progresscheck.CIFailure{Repository: repository, HeadSHA: strings.Repeat("a", 40), RunID: "1", RunAttempt: "1", JobID: "2", JobName: test.name, Step: "Run check", ExitStatus: "failure", LogTail: test.log, ChangedFiles: []string{"internal/example.go"}}
			backend.expected = true
			var first progresscheck.CIDecision
			for n := range 2 {
				response := call(firstProject, input, http.StatusOK)
				var decision progresscheck.CIDecision
				if err := json.Unmarshal(response.Body.Bytes(), &decision); err != nil {
					t.Fatal(err)
				}
				if decision.Outcome != test.decision.Outcome || !test.fail && decision != test.decision || decision.Validate() != nil {
					t.Fatalf("recorded decision = %+v", decision)
				}
				if n == 0 {
					first = decision
				} else if first != decision {
					t.Fatal("replay changed the recorded decision")
				}
			}
			if backend.expected {
				t.Fatal("new evidence did not reach the AI backend")
			}
			backend.expected = true
			call(secondProject, input, http.StatusOK)
			if backend.expected {
				t.Fatal("another project reused the first project's decision")
			}

			input.HeadSHA = strings.Repeat("b", 40)
			call(firstProject, input, http.StatusUnprocessableEntity)
			input.HeadSHA = strings.Repeat("a", 40)
			input.LogTail = strings.Repeat("x", 8001)
			call(firstProject, input, http.StatusUnprocessableEntity)
		})
	}
}
