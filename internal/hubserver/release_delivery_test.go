package hubserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releaseVerifierFixture struct {
	input  tracker.ReleasePickInput
	calls  int
	refuse bool
	verify func(tracker.ReleasePickInput) error
}

func (v *releaseVerifierFixture) Reconcile(context.Context, ReconcileRequest) (ReconcileSnapshot, error) {
	return ReconcileSnapshot{}, nil
}
func (v *releaseVerifierFixture) VerifyReleasePick(_ context.Context, input tracker.ReleasePickInput) error {
	v.calls++
	if v.refuse || input != v.input {
		return ErrReleasePickMismatch
	}
	if v.verify != nil {
		return v.verify(input)
	}
	return nil
}

func (v *releaseVerifierFixture) VerifyReleasePickRefresh(ctx context.Context, _, input tracker.ReleasePickInput) error {
	return v.VerifyReleasePick(ctx, input)
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

func (f *releaseDeliveryFixture) checkoutOnly(t *testing.T) {
	t.Helper()
	f.now = time.Now().UTC()
	repository := strings.TrimPrefix(f.request.Repository, "https://github.com/")
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET repository_id = NULL, github_repository_enabled = 0, checkout_repository = ? WHERE id = ?", repository, f.project.ID); err != nil {
		t.Fatal(err)
	}
	runner := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Heartbeat)
	runner.enroll(t)
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/"+string(runner.binding.MachineID)+"/heartbeat", runner.redemption.Credential, map[string]any{"version": "test", "capacity": 1, "checkout_repository": repository})
	requireNativeStatus(t, response, http.StatusOK)
	integration, err := f.service.projectIntegration(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
	if err != nil || !integration.CheckoutVerified || integration.RepositoryID != 0 || integration.RepositoryEnabled {
		t.Fatalf("checkout binding: %+v, %v", integration, err)
	}
}

func TestNativeReleaseDeliveryRefresh(t *testing.T) {
	auditCases := map[string]func(*tracker.ChangeReview){
		"historical source audit":                 func(*tracker.ChangeReview) {},
		"historical source with later discussion": func(*tracker.ChangeReview) {},
		"unknown source reviewer":                 func(r *tracker.ChangeReview) { r.SecurityAudit.Run.ReviewerVersion = "unknown" },
		"unknown source digest":                   func(r *tracker.ChangeReview) { r.SecurityAudit.Run.ReviewerDigest = strings.Repeat("0", 64) },
		"failed source audit":                     func(r *tracker.ChangeReview) { r.SecurityAudit.Run.ExitStatus = securityaudit.ExitStatusFailed },
		"failing source verdict": func(r *tracker.ChangeReview) {
			r.SecurityAudit.Run.Verdict = securityaudit.VerdictFail
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "hardening", Severity: "p3", Body: "Hardening finding"}}
		},
		"rejected source audit": func(r *tracker.ChangeReview) { r.Decision = "changes_requested" },
		"source audit finding": func(r *tracker.ChangeReview) {
			r.SecurityAudit.Run.Findings = []securityaudit.Finding{{ID: "auth", Severity: "p1", Body: "Missing authorization"}}
		},
		"foreign source issue":                     func(r *tracker.ChangeReview) { r.SecurityAudit.Run.IssueID = "wi_other" },
		"foreign source project":                   func(r *tracker.ChangeReview) { r.SecurityAudit.Run.ProjectID = "prj_other" },
		"foreign source repository":                func(r *tracker.ChangeReview) { r.SecurityAudit.Run.Repository = "https://github.com/owner/other" },
		"wrong source PR":                          func(r *tracker.ChangeReview) { r.SecurityAudit.Run.PRNumber = 99 },
		"wrong source base":                        func(r *tracker.ChangeReview) { r.SecurityAudit.Run.BaseSHA = strings.Repeat("8", 40) },
		"wrong source head":                        func(r *tracker.ChangeReview) { r.SecurityAudit.Run.HeadSHA = strings.Repeat("8", 40) },
		"missing source output":                    func(r *tracker.ChangeReview) { r.SecurityAudit.Run.OutputBytes = 0 },
		"wrong source session":                     func(r *tracker.ChangeReview) { r.SecurityAudit.SessionID++ },
		"historical source without delivery audit": func(*tracker.ChangeReview) {},
		"historical source without scope review":   func(*tracker.ChangeReview) {},
	}
	names := []string{"refresh", "stale revision", "different source", "different frozen series", "different release branch", "different repository", "different PR", "different branch", "held", "operator stop", "archived", "terminal", "active writer", "provenance", "no source capture transition", "missing source audit"}
	for name := range auditCases {
		names = append(names, name)
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			f, _, _ := releasePublicationFixture(t, name != "missing source audit")
			original := f.associate(t, f.request)
			if edit, ok := auditCases[name]; ok {
				var detail tracker.ChangeDetail
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.changeFixture.path, f.token, nil), &detail)
				review := detail.Reviews[len(detail.Reviews)-1]
				review.SecurityAudit.Run.ReviewerVersion = "2"
				review.SecurityAudit.Run.ReviewerDigest = "18feae8d1c5b5a50cbcfdd9feb1280302a3c888881b7facf4200d6af3c3ee761"
				edit(&review)
				raw, err := marshalNative(review)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, 'review', ?)", f.change.ID, review.VersionID, raw); err != nil {
					t.Fatal(err)
				}
				if name == "historical source with later discussion" {
					for range 33 {
						if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id, version_id, kind, record_json) VALUES (?, ?, 'discussion', '{}')", f.change.ID, review.VersionID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if name == "historical source without delivery audit" {
					descriptor := hubTestPolicy()
					approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
				}
				if name == "historical source without scope review" {
					rules := f.rules
					rules.RequireReview = false
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "no-scope-review"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
				}
			}
			var before tracker.NativeIssue
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &before)
			request := f.request
			request.IdempotencyKey = "refresh"
			request.ExpectedRevision = before.Revision
			request.BaseSHA, request.HeadSHA = strings.Repeat("1", 40), strings.Repeat("2", 40)
			want := http.StatusUnprocessableEntity
			switch name {
			case "refresh", "historical source audit", "historical source with later discussion":
				want = http.StatusOK
			case "stale revision":
				request.ExpectedRevision--
				want = http.StatusConflict
			case "different source":
				request.SourceMergeSHA = strings.Repeat("3", 40)
			case "different frozen series":
				request.FrozenSHA = strings.Repeat("3", 40)
			case "different release branch":
				request.BaseRef = "release/v2.0"
			case "different repository":
				request.Repository = "https://github.com/owner/other"
			case "different PR":
				request.PRNumber++
			case "different branch":
				request.HeadRef = "pick/other"
			case "held":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET issue_contract_json = '{"human_action":"Keep this delivery held"}' WHERE native_id = ?`, f.delivery.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "operator stop":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET body = body || '<!-- detent:no-dispatch -->' WHERE native_id = ?`, f.delivery.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "archived":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET archived = 1 WHERE native_id = ?`, f.delivery.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "terminal":
				if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND terminal = 1 LIMIT 1) WHERE native_id = ?`, f.project.ID, f.delivery.WorkItemID); err != nil {
					t.Fatal(err)
				}
			case "active writer":
				worker := f.worker(t, "refresh-worker")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "refresh-machine", "hostname": "runner", "version": "v0.117.50", "capacity": 1}), http.StatusOK)
				at := formatHubTime(f.now)
				if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, created_at, updated_at) VALUES ('refresh-lease', (SELECT id FROM issues WHERE native_id = ?), 'refresh-machine', 'refresh-session', ?, ?, ?, ?, ?)`, f.delivery.WorkItemID, formatHubTime(f.now.Add(time.Minute)), at, at, at, at); err != nil {
					t.Fatal(err)
				}
				want = http.StatusConflict
			case "provenance":
				f.verifier.refuse = true
			case "no source capture transition":
				states, err := marshalNative(append(nativeFixtureStates(), tracker.NativeState{Name: "Merging", Dispatchable: true}))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET states_json = ? WHERE id = ?", states, f.project.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO workflow_states (project_id, source_name, detent_state, dispatchable, created_at, updated_at) VALUES (?, 'Merging', 'Merging', 1, ?, ?)", f.project.ID, formatHubTime(f.now), formatHubTime(f.now)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id = (SELECT id FROM workflow_states WHERE project_id = ? AND detent_state = 'Merging') WHERE native_id = ?", f.project.ID, f.delivery.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			f.verifier.input = request.ReleasePickInput
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick", f.token, request)
			requireNativeStatus(t, response, want)
			var after tracker.NativeIssue
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &after)
			if want != http.StatusOK {
				if after.ReleaseDelivery == nil || *after.ReleaseDelivery != *original || after.Revision != before.Revision {
					t.Fatalf("refusal mutated association: %+v", after)
				}
				return
			}
			if after.ReleaseDelivery == nil || after.ReleaseDelivery.ReleasePickInput != request.ReleasePickInput || after.ReleaseDelivery.ChangeID != original.ChangeID || after.ReleaseDelivery.VersionID != "" || after.Revision <= before.Revision {
				t.Fatalf("refresh did not retain delivery owner: %+v", after)
			}
			f.associate(t, request)
			stale := request
			stale.IdempotencyKey = "concurrent-refresh"
			stale.BaseSHA, stale.HeadSHA = strings.Repeat("4", 40), strings.Repeat("5", 40)
			f.verifier.input = stale.ReleasePickInput
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick", f.token, stale), http.StatusConflict)
			var retained tracker.NativeIssue
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &retained)
			if retained.Revision != after.Revision || *retained.ReleaseDelivery != *after.ReleaseDelivery {
				t.Fatal("stale concurrent refresh partially mutated delivery")
			}
			stale.ExpectedRevision = retained.Revision
			stale.IdempotencyKey = "next-refresh"
			if next := f.associate(t, stale); next.ReleasePickInput != stale.ReleasePickInput || next.ChangeID != original.ChangeID {
				t.Fatalf("later recovery cycle did not reuse delivery: %+v", next)
			}
		})
	}
}

func TestNativeReleaseDeliveryAssociation(t *testing.T) {
	for _, test := range []struct {
		name     string
		edit     func(*testing.T, *releaseDeliveryFixture)
		status   int
		github   bool
		checkout bool
	}{
		{name: "native audited source", status: http.StatusOK},
		{name: "verified checkout only", status: http.StatusOK, checkout: true},
		{name: "checkout missing verification", status: http.StatusUnprocessableEntity, checkout: true, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "DELETE FROM runner_checkout_repositories WHERE project_id = ?", f.project.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "checkout wrong repository", status: http.StatusUnprocessableEntity, checkout: true, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.Repository = "https://github.com/owner/other" }},
		{name: "checkout wrong source", status: http.StatusUnprocessableEntity, checkout: true, edit: func(_ *testing.T, f *releaseDeliveryFixture) {
			f.request.SourceMergeSHA = strings.Repeat("2", 40)
			f.verifier.input = f.request.ReleasePickInput
		}},
		{name: "checkout wrong PR", status: http.StatusUnprocessableEntity, checkout: true, edit: func(_ *testing.T, f *releaseDeliveryFixture) { f.request.PRNumber = 8 }},
		{name: "checkout PR belongs to another issue", status: http.StatusConflict, checkout: true, edit: func(_ *testing.T, f *releaseDeliveryFixture) {
			f.request.PRNumber = 1
			f.verifier.input = f.request.ReleasePickInput
		}},
		{name: "checkout projected repository case", status: http.StatusConflict, checkout: true, edit: func(t *testing.T, f *releaseDeliveryFixture) {
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE repositories SET github_owner = upper(github_owner), github_name = upper(github_name) WHERE id = ?", f.repositoryID); err != nil {
				t.Fatal(err)
			}
			f.request.PRNumber = 1
			f.verifier.input = f.request.ReleasePickInput
		}},
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
			approval, _, err := readProjectPolicy(t.Context(), f.service.database.reader, string(f.project.OrganizationID)+"/"+string(f.project.ID))
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
			if test.checkout {
				f.checkoutOnly(t)
			}
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
		name     string
		github   bool
		checkout bool
	}{{name: "native audited source"}, {name: "GitHub source", github: true}, {name: "verified checkout only", checkout: true}} {
		t.Run(test.name, func(t *testing.T) {
			github := test.github
			f := newReleaseDeliveryFixture(t, github)
			if test.checkout {
				f.checkoutOnly(t)
			}
			input := changeTestInput()
			bundle := changeTestSource(t, &input)
			f.request.BaseSHA, f.request.HeadSHA = input.BaseSHA, input.HeadSHA
			f.verifier.input = f.request.ReleasePickInput
			d := f.associate(t, f.request)
			path := f.itemPath + "/changes/" + d.ChangeID
			descriptor, _, err := readProjectPolicy(t.Context(), f.service.database.reader, string(f.project.OrganizationID)+"/"+string(f.project.ID))
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
	migration, err := migrationFiles.ReadFile("migrations/20261026191000_release_checkout_binding.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, down, found := strings.Cut(string(migration), "-- +goose Down")
	if !found {
		t.Fatal("release checkout migration has no downgrade")
	}
	for _, statement := range []string{down, up} {
		tx, err := f.service.database.db.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(t.Context(), statement); err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range []tracker.NativeWorkItemID{f.delivery.WorkItemID, second.WorkItemID} {
		var d tracker.NativeIssue
		decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item), f.token, nil), &d)
		want := first
		if item == second.WorkItemID {
			want = other
		}
		if d.ReleaseDelivery == nil || *d.ReleaseDelivery != want {
			t.Fatalf("unusable delivery %s", item)
		}
	}
	f.checkoutOnly(t)
	f.request.IdempotencyKey = "duplicate-pr-other-issue"
	f.request.PRNumber = 7
	f.verifier.input = f.request.ReleasePickInput
	third := f.create(t, "third release delivery")
	thirdPath := f.base + "/work-items/" + string(third.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, thirdPath+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "third-dependency"}, ExpectedRevision: third.Revision, RelatedWorkItemID: f.issue.WorkItemID, Operation: "add"}), http.StatusOK)
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, thirdPath, f.token, nil), &third)
	f.request.ExpectedRevision = third.Revision
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, thirdPath+"/release-pick", f.token, f.request), http.StatusConflict)
	f.request.IdempotencyKey = "checkout-distinct-pr"
	f.request.PRNumber = 9
	f.verifier.input = f.request.ReleasePickInput
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, thirdPath+"/release-pick", f.token, f.request), http.StatusOK)
	worker := f.worker(t, "association-worker")
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, thirdPath+"/release-pick", worker, f.request), http.StatusNotFound)
	args.ProjectID = "prj_other"
	if _, err := (hubChangeApplication{service: f.service, scope: scope}).MutateChange(t.Context(), operatortool.AssociateReleasePick, args); err == nil {
		t.Fatal("cross-project association authorized")
	}
}

func TestNativeReleaseDeliveryMachineAdmission(t *testing.T) {
	for _, name := range []string{"authorized", "no delegation", "different criteria", "changed body", "missing dependency", "held", "no dispatch", "stale policy", "replay", "stale policy replay", "changed source", "reopened source", "ordinary machine", "unverified", "changed body replay"} {
		t.Run(name, func(t *testing.T) {
			f := newReleaseDeliveryFixture(t, false)
			prompt := "## Release delivery contract\nVerified release picks of reviewed develop fixes may receive the approved delivery contract.\n" + strings.ReplaceAll(nativeContractTestBody, "## ", "### ")
			if name == "no delegation" {
				prompt = strings.ReplaceAll(prompt, "Verified release picks of reviewed develop fixes may receive the approved delivery contract.", "No automatic confirmation is delegated.")
			}
			workflow, err := config.ParseProjectDefinition(config.ProjectDefinitionSources{Workflow: []byte(prompt), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\n  repository: example-org/example-project\n"), HasConfig: true, ConfigPath: "detent.yaml"})
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := config.ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			approveHubTestPolicy(t, f.service, f.base+"/policy", descriptor)
			if name == "authorized" || name == "no delegation" {
				ctx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
				credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
				if err != nil {
					t.Fatal(err)
				}
				reader := operatorWorkReads{service: f.service, scope: nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}}
				result, err := reader.ReadWork(ctx, operatortool.WorkConfig, operatortool.WorkReadRequest{ProjectID: string(f.project.ID)})
				if err != nil {
					t.Fatal(err)
				}
				var read struct {
					Data struct {
						IssueContract nativeWorkIssueContract `json:"issue_contract"`
					} `json:"data"`
				}
				if err := json.Unmarshal(result.Content, &read); err != nil {
					t.Fatal(err)
				}
				delivery := read.Data.IssueContract.ReleaseDelivery
				if name == "no delegation" && delivery != nil || name == "authorized" && (delivery == nil || delivery.PolicyID != descriptor.ID || delivery.Body != "## Acceptance criteria\nThe change works.\n\n## Must not break\nExisting behavior.\n\n## How we know it worked\nThe focused tests pass.") {
					t.Fatalf("work_config delegation=%+v", delivery)
				}
			}
			rules := f.rules
			rules.PolicyID = descriptor.ID
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/change-review-policy", testHubAdminToken, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "delivery-contract-rules"}, ExpectedID: f.rules.ID, Policy: rules}), http.StatusOK)
			body := issueorigin.Stamp(nativeContractTestBody, issueorigin.Origin{Kind: "doctor", Instance: "github-actions", Source: f.request.Repository + "/actions/runs/123", Fingerprint: "fresh-release-delivery"})
			if name == "different criteria" {
				body = strings.Replace(body, "The change works.", "Change an unrelated endpoint.", 1)
			}
			if name == "no dispatch" {
				body += "\ndetent:no-dispatch"
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "fresh-machine-delivery"}, Title: "Deliver the reviewed release repair", Body: body, State: "Backlog"})
			requireNativeStatus(t, response, http.StatusOK)
			f.delivery = tracker.NativeIssue{}
			decodeHubResponse(t, response, &f.delivery)
			if len(f.delivery.IssueContract.ConfirmedSections) != 0 {
				t.Fatal("fresh machine delivery unexpectedly confirmed")
			}
			f.itemPath = f.base + "/work-items/" + string(f.delivery.WorkItemID)
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/dependencies", f.token, tracker.DependencyMutation{Mutation: tracker.Mutation{IdempotencyKey: "fresh-source-dependency"}, ExpectedRevision: f.delivery.Revision, RelatedWorkItemID: f.issue.WorkItemID, Operation: "add"}), http.StatusOK)
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &f.delivery)
			if name == "held" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET issue_contract_json=? WHERE native_id=?", `{"human_action":"Keep this delivery held"}`, f.delivery.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			f.request.ExpectedRevision = f.delivery.Revision
			if name == "unverified" {
				f.verifier.refuse = true
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick", f.token, f.request), http.StatusUnprocessableEntity)
			} else if name != "ordinary machine" {
				f.associate(t, f.request)
			}
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &f.delivery)
			if f.delivery.State != "Backlog" || name == "held" && f.delivery.IssueContract.HumanAction != "Keep this delivery held" {
				t.Fatalf("association changed workflow or human hold: %+v", f.delivery)
			}
			if f.delivery.Body != body {
				t.Fatal("association changed caller diagnostics or provenance blocks")
			}
			if name == "replay" {
				f.request.IdempotencyKey = "machine-association-replay"
				f.associate(t, f.request)
			}
			if name == "changed body" || name == "changed body replay" {
				f.delivery.Body += "\nAdditional scope"
				if name == "changed body replay" {
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET body=? WHERE native_id=?", f.delivery.Body, f.delivery.WorkItemID); err != nil {
						t.Fatal(err)
					}
					f.request.IdempotencyKey = "changed-body-replay"
					f.associate(t, f.request)
					decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &f.delivery)
				}
			}
			if name == "missing dependency" {
				f.delivery.Dependencies = nil
			}
			if name == "stale policy" || name == "stale policy replay" {
				sources := *workflow.DefinitionSources
				sources.Workflow = []byte(prompt + "\n\nUpdated project policy.\n")
				updated, err := config.ParseProjectDefinition(sources)
				if err != nil {
					t.Fatal(err)
				}
				changed, err := config.ResolvePolicy(updated)
				if err != nil {
					t.Fatal(err)
				}
				approveHubTestPolicy(t, f.service, f.base+"/policy", changed)
			}
			if name == "stale policy replay" {
				f.request.IdempotencyKey = "stale-policy-replay"
				f.associate(t, f.request)
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &f.delivery)
			}
			if name == "changed source" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE change_requests SET record_json=json_set(record_json,'$.current_version_id','changed-version'),claim_version_id='changed-version',claim_head_sha='' WHERE id=?", f.change.ID); err != nil {
					t.Fatal(err)
				}
			}
			if name == "reopened source" {
				if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Todo') WHERE native_id=?", f.project.ID, f.issue.WorkItemID); err != nil {
					t.Fatal(err)
				}
			}
			f.delivery.State = "Todo"
			evaluation, err := evaluateNativeIssueContract(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, f.delivery)
			if err != nil {
				t.Fatal(err)
			}
			want := name == "authorized" || name == "replay"
			if evaluation.Allowed != want {
				t.Fatalf("machine delivery allowed=%t, want %t: %+v", evaluation.Allowed, want, evaluation)
			}
			if want {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "admit-delivery"}, ExpectedRevision: f.delivery.Revision, State: "Todo", Reason: "user_requested"}), http.StatusOK)
				worker := f.worker(t, "delivery-worker")
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/machines/register", worker, map[string]any{"id": "delivery-machine", "hostname": "runner", "capacity": 1, "version": "test"}), http.StatusOK)
				claim := tracker.NativeClaim{WorkItemID: f.delivery.WorkItemID, PolicyID: descriptor.ID, MachineID: "delivery-machine", SessionID: "delivery-session", TTLSeconds: 90, ProtocolMajor: 2, Capabilities: []string{"native_issues", "scoped_collaboration"}}
				preview := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims/preview", worker, tracker.NativeCapacityPreview{NativeClaim: claim})
				requireNativeStatus(t, preview, http.StatusOK)
				var page tracker.NativeCapacityPage
				decodeHubResponse(t, preview, &page)
				if !slices.ContainsFunc(page.Items, func(candidate tracker.NativeIssue) bool { return candidate.WorkItemID == f.delivery.WorkItemID }) {
					t.Fatalf("authorized delivery absent from claim preview: %+v", page)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/claims", worker, claim), http.StatusOK)
			}
		})
	}
}
