package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/progresscheck"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
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
			storage := projectRecordPostgres(t)
			backend := &ciDecisionBackend{t: t, decision: test.decision, fail: test.fail}
			service := &Service{echo: echo.New(), config: Config{Logger: discardLogger(), TenantRepository: storage, now: time.Now, Conversation: &ConversationConfig{Backend: backend, Model: "configured-model"}}}
			repository := "owner/repository"
			err := storage.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_example", Name: "Example", CreatedAt: time.Now()}); err != nil {
					return err
				}
				for _, id := range []tracker.ProjectID{"prj_first", "prj_second"} {
					record, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: "org_example", ID: id, Name: string(id), Slug: strings.ReplaceAll(string(id), "_", "-"), Profile: "native"}, States: nativeFixtureStates()})
					if err != nil {
						return err
					}
					record.Settings.CheckoutRepository = repository
					if _, err := w.UpdateProjectRecord(t.Context(), record); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			call := func(project tracker.ProjectID, input progresscheck.CIFailure) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(input)
				request := httptest.NewRequest(http.MethodPost, "/scheduled-ci-decisions", strings.NewReader(string(raw)))
				request.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
				response := httptest.NewRecorder()
				c := service.echo.NewContext(request, response)
				c.Set("native_scope", nativeScope{organization: "org_example", project: project})
				if err := service.classifyScheduledFailure(c); err != nil {
					service.echo.HTTPErrorHandler(err, c)
				}
				return response
			}
			input := progresscheck.CIFailure{Repository: repository, HeadSHA: strings.Repeat("a", 40), RunID: "1", RunAttempt: "1", JobID: "2", JobName: test.name, Step: "Run check", ExitStatus: "failure", LogTail: test.log, ChangedFiles: []string{"internal/example.go"}}
			var first progresscheck.CIDecision
			for n := range 2 {
				response := call("prj_first", input)
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
			if backend.calls != 1 {
				t.Fatalf("AI calls=%d", backend.calls)
			}
			response := call("prj_second", input)
			requireNativeStatus(t, response, http.StatusOK)
			if backend.calls != 2 {
				t.Fatal("another project reused the first project's decision")
			}

			input.HeadSHA = strings.Repeat("b", 40)
			response = call("prj_first", input)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			input.HeadSHA = strings.Repeat("a", 40)
			input.LogTail = strings.Repeat("x", 8001)
			response = call("prj_first", input)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if backend.calls != 2 {
				t.Fatal("invalid evidence reached the AI backend")
			}
		})
	}
}
