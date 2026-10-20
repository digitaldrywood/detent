package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseVerifierFixture struct {
	input  tracker.ReleasePickInput
	calls  int
	refuse bool
}

func (v *releaseVerifierFixture) Reconcile(context.Context, ReconcileRequest) (ReconcileSnapshot, error) {
	return ReconcileSnapshot{}, nil
}
func (v *releaseVerifierFixture) VerifyReleasePick(_ context.Context, input tracker.ReleasePickInput) error {
	v.calls++
	if v.refuse || input != v.input {
		return ErrReleasePickMismatch
	}
	return nil
}

type releaseDeliveryFixture struct {
	*pullRequestFixture
	delivery tracker.NativeIssue
	request  tracker.AssociateReleasePick
	verifier *releaseVerifierFixture
	itemPath string
}

func newReleaseDeliveryFixture(t *testing.T, github bool) *releaseDeliveryFixture {
	t.Helper()
	f := &releaseDeliveryFixture{pullRequestFixture: newPullRequestFixture(t, true)}
	connector, _, err := projectConnector(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
	if err != nil || connector == nil {
		t.Fatalf("repository binding: %v", err)
	}
	sourceInput := changeTestInput()
	sourceInput.Repository = "https://github.com/" + connector.Repository
	sourceResponse := performHubAPIRequest(t, f.service, http.MethodPost, f.changeFixture.path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source"}, ChangeVersionInput: sourceInput})
	requireNativeStatus(t, sourceResponse, http.StatusOK)
	var sourceVersion tracker.ChangeVersion
	decodeHubResponse(t, sourceResponse, &sourceVersion)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.changeFixture.path+"/versions/"+sourceVersion.ID+"/checks", f.token, changeTestResult(sourceVersion)), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.changeFixture.path+"/versions/"+sourceVersion.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "source-review"}, Decision: "approved"}), http.StatusOK)
	merge := strings.Repeat("c", 40)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.changeFixture.path+"/versions/"+sourceVersion.ID+"/landing", f.token, tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source-land"}, MergeSHA: merge, BaseRef: "develop", Method: "squash"}), http.StatusOK)
	descriptor := hubTestPolicy()
	descriptor.Gates.GitHubPullRequest = github
	descriptor.Gates.SecurityAudit = !github
	descriptor = descriptor.WithID()
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: hubTestPolicy().ID, Policy: descriptor}), http.StatusOK)
	rules := f.rules
	rules.PolicyID = descriptor.ID
	rulesResponse := performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "release-rules"}, ExpectedID: f.rules.ID, Policy: rules})
	requireNativeStatus(t, rulesResponse, http.StatusOK)
	decodeHubResponse(t, rulesResponse, &f.rules)
	f.delivery = f.create(t, "release delivery")
	f.itemPath = f.base + "/work-items/" + string(f.delivery.WorkItemID)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "release-dependency"}, ExpectedRevision: f.delivery.Revision, RelatedWorkItemID: f.issue.WorkItemID, Operation: "add"})
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &f.delivery)
	f.request = tracker.AssociateReleasePick{Mutation: tracker.Mutation{IdempotencyKey: "associate-release"}, ExpectedRevision: f.delivery.Revision, ReleasePickInput: tracker.ReleasePickInput{SourceIssueID: f.issue.WorkItemID, SourceChangeID: f.change.ID, SourceMergeSHA: merge, Repository: sourceVersion.Repository, PRNumber: 7, HeadRef: "pick/release-fix", HeadSHA: strings.Repeat("d", 40), BaseRef: "release/v1.2", BaseSHA: strings.Repeat("e", 40), FrozenSHA: strings.Repeat("f", 40)}}
	f.verifier = &releaseVerifierFixture{input: f.request.ReleasePickInput}
	f.service.config.ReconcileBackend = f.verifier
	return f
}

func (f *releaseDeliveryFixture) associate(t *testing.T, r tracker.AssociateReleasePick) *tracker.ReleaseDelivery {
	t.Helper()
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick", f.token, r)
	requireNativeStatus(t, response, http.StatusOK)
	var d tracker.ReleaseDelivery
	decodeHubResponse(t, response, &d)
	return &d
}

func TestNativeReleaseDeliveryAssociation(t *testing.T) {
	for _, test := range []struct {
		name   string
		edit   func(*testing.T, *releaseDeliveryFixture)
		status int
		github bool
	}{
		{name: "native audited source", status: http.StatusOK},
		{name: "native repository without GitHub tracking", status: http.StatusOK, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET github_repository_enabled = 0 WHERE id = ?", f.project.ID); err != nil {
				t.Fatal(err)
			}
			connector, _, err := projectConnector(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
			if err != nil || connector != nil {
				t.Fatalf("disabled tracker connector changed: %v, %v", connector, err)
			}
		}},
		{name: "GitHub source", status: http.StatusOK, github: true},
		{name: "security audit supplies independent review", status: http.StatusOK, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			rules := f.rules
			rules.RequireReview = false
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "audit-review"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
		}},
		{name: "no independent review", status: http.StatusUnprocessableEntity, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			approval, err := readProjectPolicy(t.Context(), f.service.database.reader, string(f.project.OrganizationID)+"/"+string(f.project.ID))
			if err != nil {
				t.Fatal(err)
			}
			descriptor := approval.Policy
			descriptor.Gates.SecurityAudit = false
			descriptor = descriptor.WithID()
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/policy", testHubAdminToken, policy.Change{ExpectedID: approval.Policy.ID, Policy: descriptor}), http.StatusOK)
			rules := f.rules
			rules.PolicyID, rules.RequireReview = descriptor.ID, false
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "no-review"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
		}},
		{name: "missing dependency", status: http.StatusUnprocessableEntity, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM issue_dependencies WHERE dependent_issue_id = (SELECT id FROM issues WHERE native_id = ?)", f.delivery.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "held", status: http.StatusOK, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, created_at, updated_at) VALUES (?, 'Blocked', 'Blocked', ?, ?)", f.project.ID, formatHubTime(f.now), formatHubTime(f.now)); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Blocked'), issue_contract_json = ? WHERE native_id = ?", f.project.ID, `{"human_action":"Keep this release held"}`, f.delivery.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "stale revision", status: http.StatusConflict, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.ExpectedRevision-- }},
		{name: "stale head", status: http.StatusUnprocessableEntity, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.HeadSHA = strings.Repeat("1", 40) }},
		{name: "wrong PR", status: http.StatusUnprocessableEntity, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.PRNumber = 8 }},
		{name: "wrong frozen head", status: http.StatusUnprocessableEntity, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.FrozenSHA = strings.Repeat("3", 40) }},
		{name: "wrong repository", status: http.StatusUnprocessableEntity, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.Repository = "https://github.com/owner/other" }},
		{name: "wrong base", status: http.StatusUnprocessableEntity, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.BaseRef = "develop" }},
		{name: "wrong source", status: http.StatusUnprocessableEntity, edit: func(_ *testing.T, f *releaseDeliveryFixture) {
			f.request.SourceMergeSHA = strings.Repeat("2", 40)
			f.verifier.input = f.request.ReleasePickInput
		}},
		{name: "unmerged source", status: http.StatusUnprocessableEntity, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Todo') WHERE native_id = ?", f.project.ID, f.issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "dependency admission disabled", status: http.StatusUnprocessableEntity, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_dependencies = 0 WHERE id = ?", f.project.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "PR belongs to another issue", status: http.StatusConflict, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			f.request.PRNumber = 1
			f.verifier.input = f.request.ReleasePickInput
		}},
		{name: "active owner", status: http.StatusConflict, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			worker := f.worker(t, "release-worker")
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "release-machine", "hostname": "runner", "version": "v0.117.50", "capacity": 1}), http.StatusOK)
			at := formatHubTime(f.now)
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, created_at, updated_at) VALUES ('release-lease', (SELECT id FROM issues WHERE native_id = ?), 'release-machine', 'release-session', ?, ?, ?, ?, ?)`, f.delivery.WorkItemID, formatHubTime(f.now.Add(90*time.Second)), at, at, at, at); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newReleaseDeliveryFixture(t, test.github)
			if test.edit != nil {
				test.edit(t, f)
			}
			var priorViews []tracker.PullRequestView
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath+"/pull-requests", f.token, nil), &priorViews)
			if len(priorViews) != 0 {
				t.Fatalf("delivery already has PRs: %+v", priorViews)
			}
			var before tracker.NativeIssue
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &before)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick", f.token, f.request)
			requireNativeStatus(t, response, test.status)
			var after tracker.NativeIssue
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &after)
			if after.State != before.State || after.IssueContract.HumanAction != before.IssueContract.HumanAction {
				t.Fatalf("association changed hold: before=%+v after=%+v", before, after)
			}
			var count int
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT count(*) FROM change_requests WHERE work_item_id = ?", f.delivery.WorkItemID).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if test.status != http.StatusOK {
				if count != 0 || after.ReleaseDelivery != nil || after.Revision != before.Revision {
					t.Fatalf("refusal partially associated: %+v changes=%d", after, count)
				}
				return
			}
			var associated tracker.ReleaseDelivery
			decodeHubResponse(t, response, &associated)
			if associated.ChangeID == "" || associated.VersionID != "" || after.ReleaseDelivery == nil || after.ReleaseDelivery.ReleasePickInput != f.request.ReleasePickInput {
				t.Fatalf("association=%+v issue=%+v", associated, after)
			}
			original := associated
			f.request.IdempotencyKey = "duplicate-release"
			duplicate := f.associate(t, f.request)
			if *duplicate != original || f.verifier.calls != 1 {
				t.Fatalf("duplicate=%+v original=%+v calls=%d", duplicate, original, f.verifier.calls)
			}
			tx, err := f.service.database.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: apiCredential{Scope: apiScopeAdmin}}
			current, _, err := readNativeIssue(t.Context(), tx, scope, string(f.delivery.WorkItemID))
			if err != nil {
				t.Fatal(err)
			}
			var versionsBefore, eventsBefore int
			if err := tx.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions),(SELECT count(*) FROM collaboration_events)").Scan(&versionsBefore, &eventsBefore); err != nil {
				t.Fatal(err)
			}
			same, err := persistNativeIssue(t.Context(), tx, scope, current, "issue.edited", tracker.CollaborationData{Change: &tracker.NativeChangeReference{ChangeID: associated.ChangeID, VersionID: associated.VersionID, HeadSHA: associated.HeadSHA}}, time.Now().UTC())
			if err != nil || same.Revision != current.Revision {
				t.Fatalf("same release reference changed revision: %d/%d %v", same.Revision, current.Revision, err)
			}
			var versionsAfter, eventsAfter int
			if err := tx.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM collaboration_versions),(SELECT count(*) FROM collaboration_events)").Scan(&versionsAfter, &eventsAfter); err != nil || versionsAfter != versionsBefore || eventsAfter != eventsBefore {
				t.Fatalf("same release reference wrote history: versions=%d/%d events=%d/%d %v", versionsAfter, versionsBefore, eventsAfter, eventsBefore, err)
			}
			if err := tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			var views []tracker.PullRequestView
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath+"/pull-requests", f.token, nil), &views)
			if len(views) != 1 || views[0].Number != 7 || views[0].Base.Ref != f.request.BaseRef || views[0].Head.SHA != f.request.HeadSHA {
				t.Fatalf("views=%+v", views)
			}
		})
	}
}

func TestNativeReleaseDeliveryReviewAndLanding(t *testing.T) {
	for _, test := range []struct {
		name   string
		github bool
	}{{name: "native audited source"}, {name: "GitHub source", github: true}} {
		t.Run(test.name, func(t *testing.T) {
			github := test.github
			f := newReleaseDeliveryFixture(t, github)
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			f.request.BaseSHA, f.request.HeadSHA = input.BaseSHA, input.HeadSHA
			f.verifier.input = f.request.ReleasePickInput
			d := f.associate(t, f.request)
			path := f.itemPath + "/changes/" + d.ChangeID
			descriptor, err := readProjectPolicy(t.Context(), f.service.database.reader, string(f.project.OrganizationID)+"/"+string(f.project.ID))
			if err != nil {
				t.Fatal(err)
			}
			input.Repository, input.PolicyID = d.Repository, descriptor.Policy.ID
			response := performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "release-version"}, ChangeVersionInput: input, SourceBundle: bundle})
			requireNativeStatus(t, response, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, response, &version)
			if version.Policy.Gates.GitHubPullRequest != github || version.Policy.Gates.SecurityAudit != !github || version.External == nil || version.External.ID != "7" {
				t.Fatalf("version lost release PR: %+v", version)
			}
			land := tracker.LandChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "release-land"}, MergeSHA: strings.Repeat("9", 40), BaseRef: d.BaseRef, Method: "squash"}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, land), http.StatusConflict)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/reviews", f.token, tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "release-review"}, Decision: "approved"}), http.StatusOK)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, land), http.StatusConflict)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/checks", f.token, changeTestResult(version)), http.StatusOK)
			if !github {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, land), http.StatusConflict)
				audit := tracker.SecurityAuditReview{SessionID: 42, Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "release-audit", ProjectID: string(f.project.ID), IssueID: string(f.delivery.WorkItemID), Repository: version.Repository, PRNumber: 7, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: f.now, StartedAt: f.now, CompletedAt: f.now.Add(time.Second), OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
				raw, err := marshalNative(tracker.ChangeReview{VersionID: version.ID, Decision: "approved", SecurityAudit: &audit})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, 'review', ?)", d.ChangeID, version.ID, raw); err != nil {
					t.Fatal(err)
				}
			}
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, land), http.StatusUnprocessableEntity)
			land.Receipt = &tracker.NativeLandingReceipt{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, MergeSHA: land.MergeSHA}
			for _, mismatch := range []string{"base", "head", "merge"} {
				wrong := land
				receipt := *land.Receipt
				wrong.Receipt = &receipt
				wrong.IdempotencyKey = "wrong-release-" + mismatch
				switch mismatch {
				case "base":
					receipt.BaseSHA = strings.Repeat("4", 40)
				case "head":
					receipt.HeadSHA = strings.Repeat("4", 40)
				case "merge":
					receipt.MergeSHA = strings.Repeat("4", 40)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, wrong), http.StatusUnprocessableEntity)
			}
			wrong := land
			wrong.IdempotencyKey = "wrong-landing-base"
			wrong.BaseRef = "develop"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, wrong), http.StatusUnprocessableEntity)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions/"+version.ID+"/landing", f.token, land), http.StatusOK)
			var detail tracker.ChangeDetail
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil), &detail)
			if detail.Change.CurrentLanding() == nil || detail.Change.Landed.BaseRef != d.BaseRef || detail.Change.Landed.HeadSHA != d.HeadSHA {
				t.Fatalf("landing=%+v", detail.Change.Landed)
			}
			input.HeadSHA = strings.Repeat("8", 40)
			response = performHubAPIRequest(t, f.service, http.MethodPost, path+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "moved-release-version"}, ExpectedVersionID: version.ID, ChangeVersionInput: input, SourceBundle: bundle})
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
		})
	}
}

func TestNativeReleaseDeliveryMCPAuthorityAndDistinctRows(t *testing.T) {
	f := newReleaseDeliveryFixture(t, true)
	var scope nativeScope
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope.organization = f.project.OrganizationID
	scope.project = f.project.ID
	scope.credential = credential
	args := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.delivery.WorkItemID), RequestID: "mcp-release", ExpectedRevision: int64(f.request.ExpectedRevision), ReleasePick: &f.request.ReleasePickInput}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operatortool.DecodeChangeArguments(operatortool.AssociateReleasePick, raw); err != nil {
		t.Fatal(err)
	}
	result, err := (hubChangeApplication{service: f.service, scope: scope}).MutateChange(t.Context(), operatortool.AssociateReleasePick, args)
	if err != nil {
		t.Fatal(err)
	}
	var first tracker.ReleaseDelivery
	if err := json.Unmarshal(result.Receipt, &first); err != nil {
		t.Fatal(err)
	}
	second := f.create(t, "second release delivery")
	secondPath := f.base + "/work-items/" + string(second.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, secondPath+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "second-dependency"}, ExpectedRevision: second.Revision, RelatedWorkItemID: f.issue.WorkItemID, Operation: "add"}), http.StatusOK)
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, secondPath, f.token, nil), &second)
	f.request.ExpectedRevision = second.Revision
	f.request.IdempotencyKey = "other-pr"
	f.request.PRNumber = 8
	f.request.HeadRef = "pick/second-fix"
	f.verifier.input = f.request.ReleasePickInput
	response := performHubAPIRequest(t, f.service, http.MethodPost, secondPath+"/release-pick", f.token, f.request)
	requireNativeStatus(t, response, http.StatusOK)
	var other tracker.ReleaseDelivery
	decodeHubResponse(t, response, &other)
	if other.ChangeID == first.ChangeID {
		t.Fatal("two deliveries share a Change")
	}
	for _, item := range []tracker.NativeWorkItemID{f.delivery.WorkItemID, second.WorkItemID} {
		var d tracker.NativeIssue
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item), f.token, nil), &d)
		if d.ReleaseDelivery == nil || d.ReleaseDelivery.ChangeID == "" {
			t.Fatalf("unusable delivery %s", item)
		}
	}
	f.request.IdempotencyKey = "duplicate-pr-other-issue"
	f.request.PRNumber = 7
	f.verifier.input = f.request.ReleasePickInput
	third := f.create(t, "third release delivery")
	thirdPath := f.base + "/work-items/" + string(third.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, thirdPath+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "third-dependency"}, ExpectedRevision: third.Revision, RelatedWorkItemID: f.issue.WorkItemID, Operation: "add"}), http.StatusOK)
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, thirdPath, f.token, nil), &third)
	f.request.ExpectedRevision = third.Revision
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, thirdPath+"/release-pick", f.token, f.request), http.StatusConflict)
	worker := f.worker(t, "association-worker")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, thirdPath+"/release-pick", worker, f.request), http.StatusNotFound)
	args.ProjectID = "prj_other"
	if _, err := (hubChangeApplication{service: f.service, scope: scope}).MutateChange(t.Context(), operatortool.AssociateReleasePick, args); err == nil {
		t.Fatal("cross-project association authorized")
	}
}
