package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
)

type ciDecisionBackend struct {
	t        *testing.T
	calls    int
	decision progresscheck.CIDecision
	fail     bool
}

func (b *ciDecisionBackend) RunTurn(_ context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.calls++
	if !request.TextOnly || !request.ReadOnly || request.MaxTurns != 1 || request.ToolInstructions != progresscheck.CIInstructions || request.SupplementalTools {
		b.t.Fatal("CI decision backend may only generate text from evidence")
	}
	var input progresscheck.CIFailure
	if err := json.Unmarshal([]byte(request.Prompt), &input); err != nil || input.LogTail == "" || len(input.ChangedFiles) != 1 || input.Step != "Run check" || input.ExitStatus != "failure" {
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
	for _, test := range []struct {
		name, log string
		decision  progresscheck.CIDecision
		fail      bool
	}{
		{"Go test", "--- FAIL: TestExample (0s)", progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "TestExample", Reason: "assertion failed"}, false},
		{"golangci-lint", "level=error msg=unused variable", progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "golangci-lint", Reason: "unused variable"}, false},
		{"Vitest", "FAIL src/example.test.ts > saves", progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "src/example.test.ts", Reason: "assertion failed"}, false},
		{"timeout with location", "internal/client.go:42:3: network timeout", progresscheck.CIDecision{Outcome: progresscheck.CIInfrastructure, Target: "download timeout", Reason: "external network unavailable"}, false},
		{"runner lost", "Runner lost communication", progresscheck.CIDecision{Outcome: progresscheck.CIInfrastructure, Target: "runner lost", Reason: "runner connection lost"}, false},
		{"unknown", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "evidence insufficient"}, false},
		{"provider unavailable", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			backend := &ciDecisionBackend{t: t, decision: test.decision, fail: test.fail}
			f.service.config.Conversation = &ConversationConfig{Backend: backend, Model: "configured-model"}
			repository := "owner/repository"
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository=? WHERE id=?", repository, f.project.ID); err != nil {
				t.Fatal(err)
			}
			input := progresscheck.CIFailure{Repository: repository, HeadSHA: strings.Repeat("a", 40), RunID: "1", RunAttempt: "1", JobID: "2", JobName: test.name, Step: "Run check", ExitStatus: "failure", LogTail: test.log, ChangedFiles: []string{"internal/example.go"}}
			var first progresscheck.CIDecision
			for n := range 2 {
				response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/scheduled-ci-decisions", f.token, input)
				requireNativeStatus(t, response, http.StatusOK)
				var decision progresscheck.CIDecision
				decodeHubResponse(t, response, &decision)
				if decision.Outcome != test.decision.Outcome || !test.fail && decision != test.decision || decision.Validate() != nil {
					t.Fatalf("recorded decision = %+v", decision)
				}
				if n == 0 {
					first = decision
				} else if !reflect.DeepEqual(first, decision) {
					t.Fatal("replay changed the recorded decision")
				}
			}
			var count int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM scheduled_ci_decisions WHERE project_id=?", f.project.ID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if backend.calls != 1 || count != 1 {
				t.Fatalf("AI calls=%d stored decisions=%d", backend.calls, count)
			}
			other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-project")
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET checkout_repository=? WHERE id=?", repository, other.project.ID); err != nil {
				t.Fatal(err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, other.base+"/scheduled-ci-decisions", other.token, input)
			requireNativeStatus(t, response, http.StatusOK)
			if backend.calls != 2 {
				t.Fatal("another project reused the first project's decision")
			}
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/scheduled-ci-decisions", other.token, input)
			requireNativeStatus(t, response, http.StatusNotFound)
			input.HeadSHA = strings.Repeat("b", 40)
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/scheduled-ci-decisions", f.token, input)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			input.HeadSHA = strings.Repeat("a", 40)
			input.LogTail = strings.Repeat("x", 8001)
			response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/scheduled-ci-decisions", f.token, input)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if backend.calls != 2 {
				t.Fatal("invalid evidence reached the AI backend")
			}
		})
	}
}
