package validation_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
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
	calls    int
	onCall   func()
}

func (b *ciDecisionBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, update runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	b.calls++
	if !b.expected {
		b.t.Fatalf("replay or invalid evidence reached the AI backend on call %d", b.calls)
	}
	b.expected = false
	if onCall := b.onCall; onCall != nil {
		b.onCall = nil
		onCall()
	}
	if err := ctx.Err(); err != nil {
		return runner.AgentTurnResult{}, err
	}
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

type scheduledClassificationImporter struct{ calls int }

func (i *scheduledClassificationImporter) FetchImportPage(_ context.Context, request hubserver.GitHubImportRequest) (hubserver.GitHubImportPage, error) {
	i.calls++
	if request.Scheduled == nil || request.Scheduled.RunID != "1" || request.Scheduled.JobID == "999" {
		return hubserver.GitHubImportPage{}, errors.New("unknown run or job")
	}
	return hubserver.GitHubImportPage{Scheduled: request.Scheduled}, nil
}

func TestScheduledCIClassificationPersistsOneDecision(t *testing.T) {
	t.Parallel()
	backend := &ciDecisionBackend{}
	importer := &scheduledClassificationImporter{}
	cfg := hubserver.Config{
		Clock:         func() time.Time { return time.Unix(7200, 0).UTC() },
		ImportBackend: importer,
		Conversation:  &hubserver.ConversationConfig{Backend: backend, Model: "configured-model"},
	}
	local := hubfixture.Open(t, cfg)
	repository := "owner/repository"
	for _, test := range []struct {
		name, log string
		decision  progresscheck.CIDecision
		fail      bool
	}{
		{"Go test", "--- FAIL: TestExample (0s)", progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "TestExample", Reason: "assertion failed"}, false},
		{"timeout with location", "internal/client.go:42:3: network timeout", progresscheck.CIDecision{Outcome: progresscheck.CIInfrastructure, Target: "download timeout", Reason: "external network unavailable"}, false},
		{"unknown", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "evidence insufficient"}, false},
		{"project budget", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "evidence insufficient"}, false},
		{"interrupted request", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "evidence insufficient"}, false},
		{"storage denial", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CISource, Target: "TestExample", Reason: strings.Repeat("assertion failed ", 20)}, false},
		{"provider unavailable", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown}, true},
		{"organization budget", "Exit status 1", progresscheck.CIDecision{Outcome: progresscheck.CIUnknown, Reason: "evidence insufficient"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			*backend = ciDecisionBackend{t: t, decision: test.decision, fail: test.fail}
			hub := local
			if test.name == "organization budget" {
				hub = hubfixture.Open(t, cfg)
			}
			var hosted *hubfixture.Hosted
			if test.name == "storage denial" {
				hostedConfig := cfg
				hostedConfig.Hosted = hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider()).Hosted
				hosted = hubfixture.OpenHosted(t, hostedConfig)
				hub = hosted.Hub
			}
			storage := hub.Repository
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
			options := hubtest.CredentialOptions{KeyScope: "write", Now: hub.Now(), Projects: []tracker.ProjectID{firstProject, secondProject}}
			if hosted != nil {
				hosted.Project = firstProject
				user := hosted.User(t, "classifier", "owner", "classifier@example.test", "write", "")
				member := new(hubtest.SeededMember)
				if err := storage.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					member.Member, err = r.AccessMember(t.Context(), user.Identity.Subject)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				options.Member = member
			}
			actor := hubtest.Credential(t, storage, org, options)
			request := func(project tracker.ProjectID, input progresscheck.CIFailure) *httptest.ResponseRecorder {
				path := hubfixture.ProjectBase(org, project) + "/scheduled-ci-decisions"
				return hub.Request(t, http.MethodPost, path, actor.Token, input)
			}
			call := func(project tracker.ProjectID, input progresscheck.CIFailure, status int) *httptest.ResponseRecorder {
				t.Helper()
				response := request(project, input)
				if response.Code != status {
					t.Fatalf("status=%d want=%d body=%s", response.Code, status, response.Body.String())
				}
				return response
			}
			input := progresscheck.CIFailure{Repository: repository, HeadSHA: strings.Repeat("a", 40), RunID: "1", RunAttempt: "1", JobID: "2", JobName: test.name, Step: "Run check", ExitStatus: "failure", LogTail: test.log, ChangedFiles: []string{"internal/example.go"}}
			if test.name == "project budget" {
				const wantDecisions = 120
				beforeImports, beforeCalls := importer.calls, backend.calls
				for n := range 125 {
					job := input
					job.JobID = fmt.Sprint(n + 100)
					backend.expected = true
					want := http.StatusTooManyRequests
					if n < wantDecisions {
						want = http.StatusOK
					}
					if response := request(firstProject, job); response.Code != want {
						t.Errorf("project job request %d status=%d want=%d", n, response.Code, want)
					}
				}
				var rows int
				if err := hub.Admin.QueryRowContext(t.Context(), `SELECT count(*) FROM scheduled_ci_decisions WHERE organization_id=$1 AND project_id=$2`, org, firstProject).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if imports, calls := importer.calls-beforeImports, backend.calls-beforeCalls; imports != wantDecisions || calls != wantDecisions || rows != wantDecisions {
					t.Fatalf("project budget made %d importer calls and %d model calls and retained %d decisions, want %d of each", imports, calls, rows, wantDecisions)
				}
				backend.expected = false
				replay := input
				replay.JobID = "100"
				call(firstProject, replay, http.StatusOK)
				backend.expected = true
				call(secondProject, replay, http.StatusOK)
				return
			}
			if test.name == "timeout with location" {
				backend.onCall = func() {
					before := importer.calls
					call(firstProject, input, http.StatusConflict)
					if importer.calls != before {
						t.Error("in-flight duplicate reached the importer")
					}
				}
			}
			if test.name == "interrupted request" {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				path := hubfixture.ProjectBase(org, firstProject) + "/scheduled-ci-decisions"
				req := hubfixture.NewRequest(t, http.MethodPost, path, actor.Token, input).WithContext(ctx)
				backend.expected, backend.onCall = true, cancel
				response := httptest.NewRecorder()
				hub.Handler().ServeHTTP(response, req)
				if response.Code == http.StatusOK || backend.calls != 1 {
					t.Fatalf("interrupted classification status=%d calls=%d", response.Code, backend.calls)
				}
			}
			if test.name == "storage denial" {
				backend.expected = true
				backend.onCall = func() {
					if err := storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.AdjustCollaborationBytes(t.Context(), 1<<55, 1<<55+1<<20)
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
				call(firstProject, input, http.StatusTooManyRequests)
			}
			if test.name == "interrupted request" || test.name == "storage denial" {
				for range 2 {
					response := call(firstProject, input, http.StatusOK)
					var decision progresscheck.CIDecision
					if err := json.Unmarshal(response.Body.Bytes(), &decision); err != nil || decision.Outcome != progresscheck.CIUnknown {
						t.Fatalf("failed attempt replay = %+v, %v", decision, err)
					}
				}
				if backend.calls != 1 {
					t.Fatalf("model calls=%d, want 1", backend.calls)
				}
				return
			}
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
			input.LogTail = test.log
			if test.name == "unknown" {
				unknownRun, unknownJob := input, input
				unknownRun.RunID, unknownJob.JobID = "2", "999"
				call(firstProject, unknownRun, http.StatusUnprocessableEntity)
				for range 2 {
					call(firstProject, unknownJob, http.StatusUnprocessableEntity)
				}
			}
			if test.name == "organization budget" {
				const limit = 6000
				const metric = "scheduled-ci"
				if err := storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					used, err := w.BillingUsage(t.Context(), hub.Now(), metric)
					if err != nil && !errors.Is(err, corestore.ErrNotFound) {
						return err
					}
					usage, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: org, ID: "existing-classification-usage", WindowStart: hub.Now(), Metric: metric, Amount: limit - used - 1}, limit)
					if err == nil && !usage.Applied {
						t.Fatal("failed to seed existing classification usage")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				const countReceipts = `SELECT count(*) FROM hosted_usage_receipts
					WHERE organization_id=$1 AND starts_with(receipt_id,'scheduled_ci_')`
				var beforeReceipts, afterReceipts int
				if err := hub.Admin.QueryRowContext(t.Context(), countReceipts, org).Scan(&beforeReceipts); err != nil {
					t.Fatal(err)
				}
				beforeImports, beforeCalls := importer.calls, backend.calls
				unknown := input
				unknown.RunID = "2"
				call(firstProject, unknown, http.StatusUnprocessableEntity)
				projects := [...]tracker.ProjectID{firstProject, secondProject}
				for n := range 30 {
					unknown.JobID = fmt.Sprint(n + 100)
					call(projects[n%len(projects)], unknown, http.StatusTooManyRequests)
				}
				if err := hub.Admin.QueryRowContext(t.Context(), countReceipts, org).Scan(&afterReceipts); err != nil {
					t.Fatal(err)
				}
				if imports, calls, receipts := importer.calls-beforeImports, backend.calls-beforeCalls, afterReceipts-beforeReceipts; imports != 1 || calls != 0 || receipts != 1 {
					t.Fatalf("organization budget made %d GitHub verifications and %d model calls and added %d receipts, want 1, 0 and 1", imports, calls, receipts)
				}
			}
			if !test.fail {
				return
			}
			plans := hostedplans.PilotPlans()
			limit := plans.Plans[0].Allowances["api_mutations"]
			window := hub.Now()
			err := storage.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				used, err := w.BillingUsage(t.Context(), window, "api_mutations")
				if err != nil && !errors.Is(err, corestore.ErrNotFound) {
					return err
				}
				usage, err := w.ApplyBillingUsage(t.Context(), billingstore.UsageReceipt{OrganizationID: org, ID: "existing-api-usage", WindowStart: window, Metric: "api_mutations", Amount: limit - used - 1}, limit)
				if err == nil && !usage.Applied {
					t.Fatal("failed to seed existing API usage")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			beforeImports := importer.calls
			beforeCalls := backend.calls
			for n := range 30 {
				distinct := input
				distinct.JobID = fmt.Sprint(n + 100)
				backend.expected = true
				response := request(firstProject, distinct)
				want := http.StatusTooManyRequests
				if n == 0 {
					want = http.StatusOK
				}
				if response.Code != want {
					t.Errorf("verified job %d: status=%d want=%d", n, response.Code, want)
				}
			}
			backend.expected = false
			var rows int
			if err := hub.Admin.QueryRowContext(t.Context(), `SELECT count(*) FROM scheduled_ci_decisions WHERE organization_id=$1 AND project_id=$2`, org, firstProject).Scan(&rows); err != nil {
				t.Fatal(err)
			}
			if calls := backend.calls - beforeCalls; calls != 1 || rows != 2 {
				t.Fatalf("distinct jobs made %d model calls and left %d decisions, want 1 call and 2 decisions (including the existing decision)", calls, rows)
			}
			if imports := importer.calls - beforeImports; imports != 1 {
				t.Fatalf("organization budget allowed %d GitHub verifications, want 1", imports)
			}
		})
	}
}
