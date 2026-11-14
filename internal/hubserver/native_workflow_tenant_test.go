package hubserver

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestWorkerBlockedWithoutTypedActionNeedsCurrentRecordedHumanAction(t *testing.T) {
	t.Parallel()
	repository := projectRecordPostgres(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	service := openLocalPostgresHubTestService(t, repository, Config{Clock: func() time.Time { return now }, RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1"), TenantRepository: repository})
	const action = "Choose whether the example endpoint keeps its legacy response shape."
	typedBlock := "```detent-status\nschema: 1\nstatus: blocked\nblockers: []\nhuman_action: \"" + action + "\"\n```"
	tests := []struct {
		name     string
		evidence string
		typed    bool
		resolve  bool
		status   int
		code     string
		source   string
	}{
		{name: "untyped current attempt", evidence: "attempt", status: http.StatusOK, source: "attempt"},
		{name: "typed current attempt", evidence: "attempt", typed: true, status: http.StatusOK},
		{name: "untyped current contract", evidence: "contract", status: http.StatusOK, source: "issue_contract"},
		{name: "typed current contract", evidence: "contract", typed: true, status: http.StatusOK},
		{name: "untyped attempt resolved during transition", evidence: "attempt", resolve: true, status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "untyped contract resolved during transition", evidence: "contract", resolve: true, status: http.StatusConflict, code: "revision_conflict"},
		{name: "untyped historical action only", evidence: "historical", status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "typed historical action only", evidence: "historical", typed: true, status: http.StatusOK},
		{name: "untyped machine failure", evidence: "failure", status: http.StatusUnprocessableEntity, code: "invalid_request"},
		{name: "untyped question before its run finishes", evidence: "unfinished", status: http.StatusUnprocessableEntity, code: "invalid_request"},
	}
	for _, organization := range []nativeOrganization{{ID: "org_skew_first", Name: "First", Slug: "skew-first"}, {ID: "org_skew_second", Name: "Second", Slug: "skew-second"}} {
		t.Run(string(organization.ID), func(t *testing.T) {
			seedPostgresHubAdmin(t, repository, service, organization)
			service.config.TenantRepository = repository
			service.localOrganization, service.database.hostedOrganization, service.requestMetrics.Organization = string(organization.ID), organization.ID, organization.ID
			f := newNativeFixture(t, service, organization.ID, "human action skew")
			setChangeFixtureStates(t, f, []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Blocked", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"Todo", "Blocked", "Done"}},
				{Name: "Blocked", Transitions: []string{"Todo"}},
				{Name: "Done", Terminal: true, Transitions: []string{"Todo"}},
			})
			approveHubTestPolicy(t, service, f.base+"/policy", hubTestPolicy())
			for index, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					service.config.TenantRepository = repository
					issue := f.create(t, test.name)
					worker := f.worker(t, test.name)
					lease := claimNativeAttempt(t, f, worker, "machine", "session-"+test.name, issue.WorkItemID)
					path := f.base + "/work-items/" + string(issue.WorkItemID)
					start := nativeStartedEvent(lease)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", worker, start), http.StatusOK)
					finish := start
					finish.Type, finish.IdempotencyKey, finish.Data.Sequence, finish.Data.Outcome = "run.finished", "finish", 2, "succeeded"
					switch test.evidence {
					case "attempt":
						finish.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true, FinalSummary: action}
					case "failure":
						finish.Data.Outcome = "failed"
						finish.Data.TerminalFailure = &tracker.NativeTerminalFailure{Stage: "implement", Attribution: "instance", ErrorClass: "protocol", Error: "backend protocol unavailable", ObservedAt: now, Source: "backend", Coverage: "complete", Summary: "Example protocol refusal"}
					default:
						finish.Data.Disposition = &tracker.NativeDisposition{Status: "complete"}
					}
					if test.evidence != "unfinished" {
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
					}
					current := readWorkItem(t, f, issue.WorkItemID, "")
					switch test.evidence {
					case "contract":
						current = writeAdmissionIssue(t, f, current, func(value *tracker.NativeIssue) {
							value.IssueContract = &issuecontract.State{HumanAction: action, ReturnState: "Todo", RecordedAt: now}
						})
					case "historical":
						historical := "Earlier question.\n\n" + typedBlock
						requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/comments", worker, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "historical", LeaseID: lease.ID, FencingToken: lease.FencingToken}, Body: historical}), http.StatusOK)
						current = readWorkItem(t, f, issue.WorkItemID, "")
					}
					if test.resolve {
						resolved := current
						service.config.TenantRepository = &projectUpdateBeforeWriteRepository{Repository: repository, before: func() error {
							if test.evidence == "contract" {
								writeAdmissionIssue(t, f, resolved, func(value *tracker.NativeIssue) { value.IssueContract.HumanAction = "" })
								return nil
							}
							return repository.WriteTenant(t.Context(), organization.ID, corestore.Serving, func(w tenantstore.Writer) error {
								row, err := w.WorkItem(t.Context(), f.project.ID, issue.WorkItemID)
								if err != nil {
									return err
								}
								attempt, err := w.LatestAttempt(t.Context(), row.ID)
								if err != nil {
									return err
								}
								return w.SetAttemptClaimFacts(t.Context(), attemptClaimFacts(attempt, "progress", false))
							})
						}}
					}
					detail := "The provider turn completed. Moved from In Progress to Blocked."
					if test.typed {
						detail += "\n\n" + typedBlock
					}
					response := performHubAPIRequest(t, service, http.MethodPost, path+"/workflow", worker, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "hold", LeaseID: lease.ID, FencingToken: lease.FencingToken}, ExpectedRevision: current.Revision, State: "Blocked", Reason: "worker_progress", ReasonDetail: detail})
					service.config.TenantRepository = repository
					if status, code := nativeResponseCode(t, response); status != test.status || code != test.code {
						t.Fatalf("case %d status=%d code=%q, want %d %q: %s", index, status, code, test.status, test.code, response.Body.String())
					}
					after := readWorkItem(t, f, issue.WorkItemID, "")
					if test.status != http.StatusOK {
						if after.State == "Blocked" {
							t.Fatalf("refused hold moved the item: %+v", after)
						}
						return
					}
					if after.State != "Blocked" {
						t.Fatalf("accepted hold left state %q", after.State)
					}
					history := performHubAPIRequest(t, service, http.MethodGet, path+"/history?limit=100", f.token, nil)
					requireNativeStatus(t, history, http.StatusOK)
					var page tracker.Page[tracker.CollaborationEvent]
					decodeHubResponse(t, history, &page)
					var recorded *tracker.CollaborationEvent
					for index := range page.Items {
						if page.Items[index].Type == "workflow.transitioned" && page.Items[index].Data.ToState == "Blocked" {
							recorded = &page.Items[index]
						}
					}
					if recorded == nil {
						t.Fatalf("missing Blocked transition in %+v", page.Items)
					}
					signal, reported := workpad.SignalFromComment(recorded.Data.ReasonDetail, "", "")
					if !reported || signal == nil || signal.Invalid != nil || signal.Status != workpad.StatusBlocked || signal.HumanAction != action {
						t.Fatalf("stored transition lacks the typed human action: %q", recorded.Data.ReasonDetail)
					}
					if !strings.HasPrefix(recorded.Data.ReasonDetail, "The provider turn completed.") {
						t.Fatalf("stored transition lost the worker summary: %q", recorded.Data.ReasonDetail)
					}
					wantSourceID := ""
					switch test.source {
					case "attempt":
						wantSourceID = start.Data.AttemptID
					case "issue_contract":
						wantSourceID = strconv.FormatInt(int64(current.Revision), 10)
					}
					if signal.Fields["human_action_source"] != test.source || signal.Fields["human_action_source_id"] != wantSourceID {
						t.Fatalf("stored provenance = %+v, want %q %q", signal.Fields, test.source, wantSourceID)
					}
				})
			}
		})
	}
}
