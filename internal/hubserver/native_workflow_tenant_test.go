package hubserver

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workpad"
)

func TestWorkerBlockedMoveRecordsCurrentContractHumanAction(t *testing.T) {
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
		{name: "untyped with current attempt question", evidence: "attempt", status: http.StatusOK},
		{name: "typed current attempt", evidence: "attempt", typed: true, status: http.StatusOK},
		{name: "untyped records current contract action", evidence: "contract", status: http.StatusOK, source: "issue_contract"},
		{name: "typed current contract", evidence: "contract", typed: true, status: http.StatusOK},
		{name: "untyped with attempt question superseded during transition", evidence: "attempt", resolve: true, status: http.StatusOK},
		{name: "untyped contract resolved during transition", evidence: "contract", resolve: true, status: http.StatusConflict, code: "revision_conflict"},
		{name: "untyped with historical action only", evidence: "historical", status: http.StatusOK},
		{name: "typed historical action only", evidence: "historical", typed: true, status: http.StatusOK},
		{name: "untyped after machine failure", evidence: "failure", status: http.StatusOK},
		{name: "old runner order: lane move, question comment, run finished, no rerun", evidence: "old_runner_order", status: http.StatusOK},
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
					if test.evidence != "old_runner_order" {
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
					if test.evidence == "failure" {
						if after.State != current.State || after.Revision != current.Revision {
							t.Fatalf("instance failure moved the item: state=%q revision=%d", after.State, after.Revision)
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
					if !test.typed && test.source == "" {
						if recorded.Data.ReasonDetail != detail || reported && signal != nil && signal.HumanAction != "" {
							t.Fatalf("untyped hold without a current contract action was not stored as sent: %q", recorded.Data.ReasonDetail)
						}
					} else {
						if !reported || signal == nil || signal.Invalid != nil || signal.Status != workpad.StatusBlocked || signal.HumanAction != action {
							t.Fatalf("stored transition lacks the typed human action: %q", recorded.Data.ReasonDetail)
						}
						if !strings.HasPrefix(recorded.Data.ReasonDetail, "The provider turn completed.") {
							t.Fatalf("stored transition lost the worker summary: %q", recorded.Data.ReasonDetail)
						}
						wantSourceID := ""
						if test.source == "issue_contract" {
							wantSourceID = strconv.FormatInt(int64(current.Revision), 10)
						}
						if signal.Fields["human_action_source"] != test.source || signal.Fields["human_action_source_id"] != wantSourceID {
							t.Fatalf("stored provenance = %+v, want %q %q", signal.Fields, test.source, wantSourceID)
						}
					}
					if test.evidence != "old_runner_order" {
						return
					}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/comments", worker, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "question", LeaseID: lease.ID, FencingToken: lease.FencingToken}, Body: "Question for a human.\n\n" + typedBlock}), http.StatusOK)
					finish.Data.Disposition = &tracker.NativeDisposition{Status: "blocked", HumanAction: true, FinalSummary: action}
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, path+"/events", worker, finish), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+string(lease.ID)+"/release", worker, tracker.NativeLeaseMutation{FencingToken: lease.FencingToken, Reason: "completed"}), http.StatusNoContent)
					credential, status, err := service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(worker), "", "")
					if err != nil || status != http.StatusOK {
						t.Fatalf("worker authentication status=%d error=%v", status, err)
					}
					rerun := performHubAPIRequest(t, service, http.MethodPost, f.base+"/claims", worker, tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: issue.WorkItemID, MachineID: credential.Runner.MachineID, SessionID: "rerun-" + test.name, TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}})
					if rerun.Code == http.StatusOK {
						t.Fatalf("held question was claimed for a rerun: %s", rerun.Body.String())
					}
					if held := readWorkItem(t, f, issue.WorkItemID, ""); held.State != "Blocked" {
						t.Fatalf("old runner completion left state %q", held.State)
					}
				})
			}
		})
	}
}
