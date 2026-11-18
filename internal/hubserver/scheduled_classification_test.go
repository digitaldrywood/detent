package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
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
	onCall   func()
}

func (b *ciDecisionBackend) RunTurn(_ context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.calls++
	if b.onCall != nil {
		callback := b.onCall
		b.onCall = nil
		callback()
	}
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
	t.Parallel()
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
			ref := billingstore.PlanReference{ID: "example", Version: 1}
			allowances := map[string]int64{"api_mutations": 3, "collaboration_bytes": 100000}
			seedConversationAllowancePostgres(t, storage, "org_example", ref, allowances)
			backend := &ciDecisionBackend{t: t, decision: test.decision, fail: test.fail}
			service := &Service{database: conversationAllowanceDatabase("org_example", ref, allowances, storage), echo: echo.New(), config: Config{Logger: discardLogger(), TenantRepository: storage, Clock: func() time.Time { return time.Unix(120, 0).UTC() }, Conversation: &ConversationConfig{Backend: backend, Model: "configured-model"}}}
			repository := "owner/repository"
			err := storage.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
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
				c.Set("native_scope", hubscope.NativeScope{Organization: "org_example", Project: project})
				if err := service.classifyScheduledFailure(c); err != nil {
					service.echo.HTTPErrorHandler(err, c)
				}
				return response
			}
			input := progresscheck.CIFailure{Repository: repository, HeadSHA: strings.Repeat("a", 40), RunID: "1", RunAttempt: "1", JobID: "2", JobName: test.name, Step: "Run check", ExitStatus: "failure", LogTail: test.log, ChangedFiles: []string{"internal/example.go"}}
			service.config.ImportBackend = scheduledContractImporter(func(_ context.Context, r GitHubImportRequest) (GitHubImportPage, error) {
				if r.Scheduled.RunID != "1" || r.Scheduled.JobID == "999" {
					return GitHubImportPage{}, errors.New("unknown run")
				}
				return GitHubImportPage{Scheduled: r.Scheduled}, nil
			})
			backend.onCall = func() { requireNativeStatus(t, call("prj_first", input), http.StatusConflict) }
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

			for n := range 30 {
				unknown := input
				unknown.RunID = fmt.Sprint(n + 10)
				unknown.JobID = fmt.Sprint(n + 100)
				requireNativeStatus(t, call("prj_first", unknown), http.StatusUnprocessableEntity)
			}
			unknownJob := input
			unknownJob.JobID = "999"
			requireNativeStatus(t, call("prj_first", unknownJob), http.StatusUnprocessableEntity)
			for n := range 30 {
				distinct := input
				distinct.JobID = fmt.Sprint(n + 100)
				response := call("prj_first", distinct)
				if n == 0 {
					requireNativeStatus(t, response, http.StatusOK)
				} else {
					requireNativeStatus(t, response, http.StatusTooManyRequests)
				}
			}
			if backend.calls != 3 {
				t.Fatalf("unbounded model calls: %d", backend.calls)
			}
			err = storage.ReadTenant(t.Context(), "org_example", corestore.Serving, func(r tenantstore.Reader) error {
				used, err := r.CollaborationBytes(t.Context())
				if err != nil {
					return err
				}
				if used <= 0 {
					t.Fatal("decisions are unaccounted")
				}
				for n := range 30 {
					distinct := input
					distinct.JobID = fmt.Sprint(n + 100)
					_, err := r.(policystore.ScheduledReader).ReadScheduledDecision(t.Context(), "prj_first", distinct)
					if n == 0 && err != nil || n > 0 && !errors.Is(err, corestore.ErrNotFound) {
						t.Fatalf("decision %d: %v", n, err)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "Go test" {
				service.database.hostedRuntime.Store(nil)
				defaults := hostedplans.PilotPlans()
				limit := defaults.Plans[0].Allowances["api_mutations"]
				err := storage.WriteTenant(t.Context(), "org_example", corestore.Serving, func(w tenantstore.Writer) error {
					usage, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: "org_example", ID: "existing-usage", WindowStart: time.Unix(0, 0).UTC(), Metric: "api_mutations", Amount: limit}, limit)
					if err == nil && !usage.Applied {
						t.Fatal("default budget fixture did not consume its allowance")
					}
					return err
				})
				if err != nil {
					t.Fatal(err)
				}
				distinct := input
				distinct.JobID = "500"
				requireNativeStatus(t, call("prj_first", distinct), http.StatusTooManyRequests)
			}

			input.HeadSHA = strings.Repeat("b", 40)
			response = call("prj_first", input)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			input.HeadSHA = strings.Repeat("a", 40)
			input.LogTail = strings.Repeat("x", 8001)
			response = call("prj_first", input)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if backend.calls != 3 {
				t.Fatal("invalid evidence reached the AI backend")
			}
		})
	}
}
