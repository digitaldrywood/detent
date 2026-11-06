package hubserver

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalBoardMigrationToLanding(t *testing.T) {
	t.Parallel()
	f := localBoardTarget(t, nil, "Example migration")
	if f.service.config.Hosted != nil || f.service.database.hostedBilling || f.service.database.plans() != nil {
		t.Fatal("local migration enabled hosted identity or Cloud plans")
	}
	server := httptest.NewServer(f.service.Handler())
	t.Cleanup(server.Close)
	request := httptest.NewRequest(http.MethodGet, server.URL+"/work", nil)
	request.SetBasicAuth("operator", testHubAdminToken)
	request.RequestURI = ""
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("local web app status = %d", response.StatusCode)
	}
	runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	runner.enroll(t)
	_, records := localBoardFixture(t, "local_sqlite")
	var imported []tracker.NativeIssue
	for _, record := range records {
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/local-board-import", f.token, localboard.Request{Record: record})
		requireNativeStatus(t, response, http.StatusOK)
		var result localboard.Result
		decodeHubResponse(t, response, &result)
		imported = append(imported, readWorkItem(t, f, result.WorkItemID, ""))
	}
	if len(imported) != 2 || imported[0].State != "Todo" || imported[1].State != "Backlog" || !reflect.DeepEqual(imported[1].Dependencies, []tracker.NativeWorkItemID{imported[0].WorkItemID}) {
		t.Fatalf("imported lanes and dependencies = %+v", imported)
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	rules := tracker.ChangeReviewPolicy{PolicyID: hubTestPolicy().ID, RequireReview: true}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "review-policy"}, Policy: rules}), http.StatusOK)
	worker := runner.redemption.Credential
	claimRequest := tracker.NativeClaim{PolicyID: hubTestPolicy().ID, WorkItemID: imported[0].WorkItemID, MachineID: runner.binding.MachineID, SessionID: "migration", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	requireNativeCode(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claimRequest), http.StatusConflict, "issue_contract_unsatisfied")
	itemPath := f.base + "/work-items/" + string(imported[0].WorkItemID)
	body := imported[0].Body + issueContractTestSections
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, itemPath, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "complete-imported-contract"}, ExpectedRevision: imported[0].Revision, Body: &body}), http.StatusOK)
	claim := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claimRequest)
	requireNativeStatus(t, claim, http.StatusOK)
	var lease tracker.NativeLease
	decodeHubResponse(t, claim, &lease)
	started := nativeStartedEvent(lease)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/events", worker, started), http.StatusOK)
	mutation := tracker.Mutation{IdempotencyKey: "change", LeaseID: lease.ID, FencingToken: lease.FencingToken}
	created := performHubAPIRequest(t, f.service, http.MethodPost, itemPath+"/changes", worker, tracker.CreateChange{Mutation: mutation, Title: "Example migration change"})
	requireNativeStatus(t, created, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, created, &change)
	changePath := itemPath + "/changes/" + change.ID
	mutation.IdempotencyKey = "publish"
	input := changeTestInput()
	input.Repository = "https://github.com/example-org/example-project"
	input.RunID, input.AttemptID = started.Data.RunID, started.Data.AttemptID
	bundle := changeTestSource(t, &input)
	published := performHubAPIRequest(t, f.service, http.MethodPost, changePath+"/versions", worker, tracker.PublishChangeVersion{Mutation: mutation, ChangeVersionInput: input, SourceBundle: bundle})
	requireNativeStatus(t, published, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, published, &version)
	versionPath := changePath + "/versions/" + version.ID
	mutation.IdempotencyKey = "land"
	landing := tracker.LandChangeVersion{Mutation: mutation, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/landing", worker, landing), http.StatusConflict)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
	landed := performHubAPIRequest(t, f.service, http.MethodPost, versionPath+"/landing", worker, landing)
	requireNativeStatus(t, landed, http.StatusOK)
	decodeHubResponse(t, landed, &change)
	if change.Landed == nil || change.Landed.VersionID != version.ID || change.Landed.HeadSHA != version.HeadSHA || change.Landed.MergeSHA != landing.MergeSHA {
		t.Fatalf("landing lost imported change identity: %+v", change)
	}
	for i, want := range []string{"Done", "Backlog"} {
		item := readWorkItem(t, f, imported[i].WorkItemID, "")
		if item.State != want || item.Terminal != (i == 0) || !reflect.DeepEqual(item.Dependencies, imported[i].Dependencies) {
			t.Fatalf("after landing item = %+v, want lane %s and original dependencies", item, want)
		}
	}
}
