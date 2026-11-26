package coreimport_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/localboard"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalBoardMigrationToLanding(t *testing.T) {
	t.Parallel()
	hub := hubfixture.Open(t, hubfixture.Config{StaticFS: fstest.MapFS{
		"app/conversation/index.html": {Data: []byte("<!doctype html><html><head></head><body><div id=\"root\"></div></body></html>")},
		"app/conversation/app.js":     {Data: []byte("console.log(1)")},
	}})
	f := importRouteProject(t, hub, "Example migration")
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(server.Close)
	request := httptest.NewRequest(http.MethodGet, server.URL+"/work", nil)
	request.SetBasicAuth("operator", hub.AdminToken())
	request.RequestURI = ""
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Errorf("local web app status = %d", response.StatusCode)
	}
	bootstrapRequest := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/app/bootstrap", nil)
	bootstrapRequest.SetBasicAuth("operator", hub.AdminToken())
	bootstrap := httptest.NewRecorder()
	hub.Handler().ServeHTTP(bootstrap, bootstrapRequest)
	hubfixture.RequireStatus(t, bootstrap, http.StatusOK)
	var deployment struct {
		SelfHosted bool             `json:"self_hosted"`
		Plan       *json.RawMessage `json:"plan"`
	}
	hubfixture.Decode(t, bootstrap, &deployment)
	if !deployment.SelfHosted || deployment.Plan != nil {
		t.Fatalf("local migration bootstrap has hosted identity or Cloud plans: %s", bootstrap.Body.String())
	}
	runner := hubfixture.PrepareRunner(t, f, runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate)
	runner.Enroll(t)
	runner.ReportCapacity(t)
	records := importRouteRecords()
	var imported []tracker.NativeIssue
	for _, record := range records {
		response := hub.Request(t, http.MethodPost, f.Base+"/local-board-import", f.Token, localboard.Request{Record: record})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var result localboard.Result
		hubfixture.Decode(t, response, &result)
		imported = append(imported, readLocalMigrationItem(t, f, result.WorkItemID))
	}
	if len(imported) != 2 || imported[0].State != "Todo" || imported[1].State != "Backlog" || !reflect.DeepEqual(imported[1].Dependencies, []tracker.NativeWorkItemID{imported[0].WorkItemID}) {
		t.Fatalf("imported lanes and dependencies = %+v", imported)
	}
	hubfixture.ApprovePolicy(t, hub, f.Base+"/policy", hubfixture.Policy())
	rules := tracker.ChangeReviewPolicy{PolicyID: hubfixture.Policy().ID, RequireReview: true}
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPut, f.Base+"/change-review-policy", hub.AdminToken(), tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "review-policy"}, Policy: rules}), http.StatusOK)
	worker := runner.Redemption.Credential
	claimRequest := tracker.NativeClaim{PolicyID: hubfixture.Policy().ID, WorkItemID: imported[0].WorkItemID, MachineID: runner.Binding.MachineID, SessionID: "migration", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
	itemPath := f.Base + "/work-items/" + string(imported[0].WorkItemID)
	claim := hub.Request(t, http.MethodPost, f.Base+"/claims", worker, claimRequest)
	hubfixture.RequireStatus(t, claim, http.StatusOK)
	var lease tracker.NativeLease
	hubfixture.Decode(t, claim, &lease)
	started := hubfixture.StartedEvent(lease)
	started.Data.Identity.Role = "code"
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, itemPath+"/events", worker, started), http.StatusOK)
	mutation := tracker.Mutation{IdempotencyKey: "change", LeaseID: lease.ID, FencingToken: lease.FencingToken}
	created := hub.Request(t, http.MethodPost, itemPath+"/changes", worker, tracker.CreateChange{Mutation: mutation, Title: "Example migration change"})
	hubfixture.RequireStatus(t, created, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, created, &change)
	changePath := itemPath + "/changes/" + change.ID
	mutation.IdempotencyKey = "publish"
	input := hubfixture.ChangeInput()
	input.Repository = "https://github.com/example-org/example-project"
	input.RunID, input.AttemptID = started.Data.RunID, started.Data.AttemptID
	bundle := sourcetest.Source(t, &input)
	diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: input.AttemptID, RunnerID: runner.Binding.RunnerID, LeaseID: lease.ID, FencingToken: lease.FencingToken}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, f.Base+"/attempts/"+input.AttemptID+"/diff", worker, diff), http.StatusAccepted)
	published := hub.Request(t, http.MethodPost, changePath+"/versions", worker, tracker.PublishChangeVersion{Mutation: mutation, ChangeVersionInput: input, SourceBundle: bundle})
	hubfixture.RequireStatus(t, published, http.StatusOK)
	var version tracker.ChangeVersion
	hubfixture.Decode(t, published, &version)
	versionPath := changePath + "/versions/" + version.ID
	mutation.IdempotencyKey = "land"
	landing := tracker.LandChangeVersion{Mutation: mutation, MergeSHA: strings.Repeat("e", 40), BaseRef: "main", Method: "squash"}
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, versionPath+"/landing", worker, landing), http.StatusConflict)
	hubfixture.RequireStatus(t, hub.Request(t, http.MethodPost, versionPath+"/reviews", f.Token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve"}, Decision: "approved"}), http.StatusOK)
	landed := hub.Request(t, http.MethodPost, versionPath+"/landing", worker, landing)
	hubfixture.RequireStatus(t, landed, http.StatusOK)
	hubfixture.Decode(t, landed, &change)
	if change.Landed == nil || change.Landed.VersionID != version.ID || change.Landed.HeadSHA != version.HeadSHA || change.Landed.MergeSHA != landing.MergeSHA {
		t.Fatalf("landing lost imported change identity: %+v", change)
	}
	for i, want := range []string{"Done", "Backlog"} {
		item := readLocalMigrationItem(t, f, imported[i].WorkItemID)
		if item.State != want || item.Terminal != (i == 0) || !reflect.DeepEqual(item.Dependencies, imported[i].Dependencies) {
			t.Fatalf("after landing item = %+v, want lane %s and original dependencies", item, want)
		}
	}
}

func readLocalMigrationItem(t *testing.T, project hubfixture.Native, id tracker.NativeWorkItemID) tracker.NativeIssue {
	t.Helper()
	response := project.Request(t, http.MethodGet, project.Base+"/work-items/"+string(id), nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue
}
