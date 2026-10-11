//go:build !windows

package cloudentry

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/billing"
	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSharedOriginRunnerConversationBind(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	for _, test := range []struct {
		name    string
		billing billing.Provider
		bind    int
	}{
		{name: "conversations enabled", billing: previewBillingProvider{}, bind: http.StatusOK},
		{name: "conversations disabled", bind: http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newSharedOriginPilotWith(t, 1, 1, fstest.MapFS{}, test.billing)
			store := newSpacesFixture(t, false)
			storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
			if err != nil {
				t.Fatal(err)
			}
			p.service.attachments = storage
			owner := p.browser(t)
			pilotStatus(t, "sign-in", owner.login("/auth/oidc/start", "user_dana:"), http.StatusSeeOther)
			created := owner.createOrganization("Alpha Labs")
			pilotStatus(t, "create", created, http.StatusSeeOther)
			o := pilotOrganization{id: organizationFromLocation(t, created.location), owner: owner}
			p.waitState(t, o.id, "ready")
			pilotStatus(t, "organization sign-in", owner.login(o.page(), "user_dana:porg_"+o.id), http.StatusSeeOther)
			o.ownerCSRF = owner.csrf(o.page())
			project := owner.form("/organizations/"+o.id+"/projects", url.Values{"name": {"Alpha project"}, "grant_access": {"true"}, "csrf": {o.ownerCSRF}})
			pilotStatus(t, "project", project, http.StatusSeeOther)
			o.project = strings.TrimPrefix(project.location, "/organizations/"+o.id+"/projects/")

			pilotStatus(t, "runner grant", owner.form("/organizations/"+o.id+"/organization/grants", url.Values{"user": {"user_dana"}, "project": {o.project}, "write": {"true"}, "runner": {"true"}, "csrf": {o.ownerCSRF}}), http.StatusSeeOther)
			binding := runnerauth.NewBinding()
			enrollment := owner.json(http.MethodPost, o.api()+"/runner-enrollments", o.ownerCSRF, runnerauth.EnrollmentRequest{Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(o.project)}, Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events}, TTLSeconds: 900})
			pilotStatus(t, "enrollment", enrollment, http.StatusCreated)
			var issued runnerauth.Enrollment
			pilotDecode(t, enrollment, &issued)
			credential, err := apikey.GenerateToken()
			if err != nil {
				t.Fatal(err)
			}
			redemption := runnerauth.Redemption{BackendIsolation: isolation.Report{"codex": {isolation.Sandbox, isolation.NativeTrusted}}, Binding: binding, Credential: credential, Hostname: "runner.example.test", DisplayName: "runner.example.test", Capacity: 1, Version: "test", OS: "linux", Architecture: "amd64"}
			pilotStatus(t, "redemption", p.machine(t, http.MethodPost, o.api()+"/runner-enrollments/redeem", issued.Token, redemption), http.StatusCreated)

			descriptor := pilotPolicy()
			pilotStatus(t, "policy", owner.json(http.MethodPut, o.projectAPI()+"/onboarding/policy", o.ownerCSRF, policy.Change{Policy: descriptor}), http.StatusOK)
			var issue tracker.NativeIssue
			if test.billing == nil {
				issueResponse := owner.json(http.MethodPost, o.projectAPI()+"/work-items", o.ownerCSRF, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "runner-work"}, Title: "Runner work", Body: issueContractTestSections, State: "Todo"})
				pilotStatus(t, "issue", issueResponse, http.StatusOK)
				pilotDecode(t, issueResponse, &issue)
			} else {
				chat := owner.json(http.MethodPost, o.projectAPI()+"/conversations", o.ownerCSRF, map[string]any{"key": "chat", "title": "Runner chat"})
				pilotStatus(t, "conversation", chat, http.StatusCreated)
				var started struct {
					Conversation struct {
						ID string `json:"id"`
					} `json:"conversation"`
				}
				pilotDecode(t, chat, &started)
				linked := owner.json(http.MethodPost, o.projectAPI()+"/conversations/"+started.Conversation.ID+"/link", o.ownerCSRF, map[string]any{"key": "link", "share_history": true, "issue": map[string]any{"title": "Runner work", "description": issueContractTestSections}})
				pilotStatus(t, "link", linked, http.StatusOK)
				var link struct {
					Issue tracker.NativeIssue `json:"issue"`
				}
				pilotDecode(t, linked, &link)
				issue = link.Issue
			}
			hub := "/organizations/" + o.id + o.projectAPI()
			claim := tracker.NativeClaim{PolicyID: descriptor.ID, WorkItemID: issue.WorkItemID, MachineID: binding.MachineID, SessionID: "conversation-bind", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration", tracker.NativeExecutionCapability}}
			claimed := p.machine(t, http.MethodPost, hub+"/claims", credential, claim)
			pilotStatus(t, "claim", claimed, http.StatusOK)
			var lease tracker.NativeLease
			pilotDecode(t, claimed, &lease)
			run, attempt := "run_"+strings.Repeat("a", 32), "attempt_"+strings.Repeat("b", 32)
			start := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "start"}, Type: "run.started", SchemaVersion: 1,
				Data: tracker.NativeRunData{Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "test-model"}, LeaseID: lease.ID, FencingToken: lease.FencingToken, PolicyID: lease.PolicyID, RunID: run, AttemptID: attempt}}
			item := hub + "/work-items/" + string(issue.WorkItemID)
			pilotStatus(t, "run started", p.machine(t, http.MethodPost, item+"/events", credential, start), http.StatusOK)

			diffPath := o.projectAPI() + "/attempts/" + attempt + "/diff"
			diffRequest := tracker.AttemptDiffRequest{
				Producer:   tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: attempt, RunnerID: binding.RunnerID, LeaseID: lease.ID, FencingToken: lease.FencingToken},
				Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2},
				BaseSHA:    strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
				Files: []tracker.AttemptDiffFile{{Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Patch: "+fixed\n"}},
			}
			uploaded := p.machine(t, http.MethodPost, diffPath, credential, diffRequest)
			pilotStatus(t, "runner diff upload", uploaded, http.StatusAccepted)
			var receipt tracker.AttemptDiffReceipt
			pilotDecode(t, uploaded, &receipt)
			read := p.machine(t, http.MethodGet, diffPath+"?at=2", credential, nil)
			pilotStatus(t, "runner diff read", read, http.StatusOK)
			var diff tracker.AttemptDiff
			pilotDecode(t, read, &diff)
			if diff.ID != receipt.DiffID || diff.AttemptID != attempt || diff.Generation.Seq != 2 || diff.Producer != diffRequest.Producer || diff.BaseSHA != diffRequest.BaseSHA || diff.HeadSHA != diffRequest.HeadSHA || len(diff.Files) != 1 || diff.Files[0].Patch != "+fixed\n" {
				t.Fatalf("public diff identity/body mismatch: %+v", diff)
			}
			diffRequest.Generation.Seq = 3
			diffRequest.Producer.FencingToken++
			before := len(store.requests())
			denied := p.machine(t, http.MethodPost, diffPath, credential, diffRequest)
			pilotStatus(t, "stale runner diff", denied, http.StatusConflict)
			if !strings.Contains(denied.body, "stale_execution") || len(store.requests()) != before {
				t.Fatal("denied diff reached object storage or lost fencing error")
			}

			bind := map[string]any{"lease_id": lease.ID, "fencing_token": lease.FencingToken, "attempt_id": attempt, "run_id": run, "capabilities": map[string]bool{"steer": true}}
			for _, request := range []struct {
				name string
				got  pilotResponse
				want int
			}{
				{name: "bearer bind", got: p.machine(t, http.MethodPost, item+"/conversation/bind", credential, bind), want: test.bind},
				{name: "bearer unregistered route", got: p.machine(t, http.MethodPost, item+"/unregistered", credential, bind), want: http.StatusNotFound},
				{name: "browser bind without CSRF", got: owner.json(http.MethodPost, item+"/conversation/bind", "", bind), want: http.StatusForbidden},
			} {
				if strings.Contains(request.got.body, "invalid_csrf") != (request.want == http.StatusForbidden) || request.got.status != request.want {
					t.Errorf("%s = %d %.300s, want %d", request.name, request.got.status, request.got.body, request.want)
				}
			}
		})
	}
}
