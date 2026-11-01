package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releasePublisherFixture struct {
	*releaseVerifierFixture
	creates      int
	beforeCreate func()
	lostResponse bool
	publication  tracker.ReleasePickPublication
}

func (p *releasePublisherFixture) PublishReleasePick(ctx context.Context, item tracker.NativeWorkItemID, input tracker.ReleasePickInput, authorize func(context.Context) error) (tracker.ReleasePickPublication, error) {
	if p.beforeCreate != nil {
		p.beforeCreate()
	}
	if err := authorize(ctx); err != nil {
		return tracker.ReleasePickPublication{}, err
	}
	if input.PRNumber > 0 {
		if err := p.VerifyReleasePick(ctx, input); err != nil {
			return tracker.ReleasePickPublication{}, err
		}
		return tracker.ReleasePickPublication{ReleasePickInput: input, WorkItemID: item, URL: fmt.Sprintf("%s/pull/%d", input.Repository, input.PRNumber)}, nil
	}
	expected := p.input
	expected.PRNumber = 0
	if input != expected {
		return tracker.ReleasePickPublication{}, ErrReleasePickMismatch
	}
	if p.creates == 0 {
		p.creates++
		p.publication = tracker.ReleasePickPublication{ReleasePickInput: p.input, WorkItemID: item, URL: "https://github.com/example-org/example-project/pull/7"}
	}
	if p.lostResponse {
		p.lostResponse = false
		return tracker.ReleasePickPublication{}, errors.New("lost create response")
	}
	return p.publication, nil
}

func releasePublicationFixture(t *testing.T, audited bool) (*releaseDeliveryFixture, *releasePublisherFixture, tracker.PublishReleasePick) {
	t.Helper()
	f := newReleaseDeliveryFixture(t, false)
	p := &releasePublisherFixture{releaseVerifierFixture: f.verifier}
	f.service.config.ReconcileBackend = p
	var detail tracker.ChangeDetail
	decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.changeFixture.path, f.token, nil), &detail)
	version := detail.Versions[len(detail.Versions)-1]
	now := f.now
	audit := &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{ID: 42, InvocationID: "source-audit", ProjectID: string(f.project.ID), IssueID: string(f.issue.WorkItemID), Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
	raw, err := json.Marshal(tracker.ChangeReview{VersionID: version.ID, Decision: "approved", SecurityAudit: audit})
	if err != nil {
		t.Fatal(err)
	}
	if audited {
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_evidence (change_id,version_id,kind,record_json) VALUES (?,?,'review',?)", f.change.ID, version.ID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	input := f.request.ReleasePickInput
	input.PRNumber = 0
	return f, p, tracker.PublishReleasePick{Mutation: tracker.Mutation{IdempotencyKey: "publish-release"}, ReleasePickInput: input, ExpectedRevision: f.request.ExpectedRevision}
}

func TestNativeReleasePickPublication(t *testing.T) {
	for _, name := range []string{"publish", "native repository", "lost response", "stale revision", "wrong source", "wrong repository", "missing repository", "wrong head", "wrong base", "archived", "terminal", "human hold", "blocked", "missing audit", "missing dependency", "worker", "foreign project", "missing transport", "grant revoked during verification", "token downgraded during verification", "hold during verification", "changed retry"} {
		t.Run(name, func(t *testing.T) {
			f, p, request := releasePublicationFixture(t, name != "missing audit")
			token := f.token
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := f.service.database.db.ExecContext(t.Context(), query, args...); err != nil {
					t.Fatal(err)
				}
			}
			want := http.StatusUnprocessableEntity
			switch name {
			case "publish", "lost response":
				want = http.StatusOK
				p.lostResponse = name == "lost response"
			case "native repository":
				exec("UPDATE projects SET github_repository_enabled=0 WHERE id=?", f.project.ID)
				want = http.StatusOK
				connector, _, err := projectConnector(t.Context(), f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID})
				if err != nil || connector != nil {
					t.Fatalf("disabled repository connector=%+v error=%v", connector, err)
				}
			case "stale revision":
				request.ExpectedRevision--
				want = http.StatusConflict
			case "wrong source":
				request.SourceMergeSHA = strings.Repeat("0", 40)
			case "wrong repository":
				request.Repository = "https://github.com/owner/other"
			case "missing repository":
				exec("UPDATE projects SET repository_id=NULL WHERE id=?", f.project.ID)
			case "wrong head":
				request.HeadSHA = strings.Repeat("0", 40)
			case "wrong base":
				request.BaseRef = "develop"
			case "archived":
				exec("UPDATE issues SET archived=1 WHERE native_id=?", f.delivery.WorkItemID)
			case "terminal":
				exec("UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Done') WHERE native_id=?", f.project.ID, f.delivery.WorkItemID)
			case "human hold":
				exec("UPDATE issues SET issue_contract_json=? WHERE native_id=?", `{"human_action":"Hold this delivery"}`, f.delivery.WorkItemID)
			case "blocked":
				exec("INSERT INTO workflow_states (project_id,source_name,detent_state,created_at,updated_at) VALUES (?,'Blocked','Blocked',?,?)", f.project.ID, formatHubTime(f.now), formatHubTime(f.now))
				exec("UPDATE issues SET workflow_state_id=(SELECT id FROM workflow_states WHERE project_id=? AND detent_state='Blocked') WHERE native_id=?", f.project.ID, f.delivery.WorkItemID)
			case "missing audit":

			case "missing dependency":
				request.SourceIssueID = f.delivery.WorkItemID
			case "worker":
				token = f.worker(t, "publication-worker")
				want = http.StatusNotFound
			case "foreign project":
				f.itemPath = strings.Replace(f.itemPath, string(f.project.ID), "prj_foreign", 1)
				want = http.StatusNotFound
			case "missing transport":
				f.service.config.ReconcileBackend = f.verifier
				want = http.StatusInternalServerError
			case "grant revoked during verification":
				p.beforeCreate = func() {
					exec("DELETE FROM token_grants WHERE token_id=(SELECT id FROM api_tokens WHERE name='operator') AND project_id=?", f.project.ID)
					exec("DELETE FROM token_grants WHERE project_id=?", f.project.ID)
				}
				want = http.StatusNotFound
			case "token downgraded during verification":
				credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
				if err != nil {
					t.Fatal(err)
				}
				p.beforeCreate = func() { exec("UPDATE api_tokens SET scope='worker' WHERE id=?", credential.ID) }
				want = http.StatusNotFound
			case "hold during verification":
				p.beforeCreate = func() {
					exec("UPDATE issues SET issue_contract_json=? WHERE native_id=?", `{"human_action":"Hold this delivery"}`, f.delivery.WorkItemID)
				}
			case "changed retry":
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick/publication", token, request), http.StatusOK)
				request.HeadSHA = strings.Repeat("0", 40)
				want = http.StatusConflict
			}
			if name == "foreign project" {
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick/publication", token, request), want)
				if p.creates != 0 {
					t.Fatal("foreign scope published a PR")
				}
				return
			}
			var before tracker.NativeIssue
			decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &before)
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick/publication", token, request)
			if name == "lost response" {
				requireNativeStatus(t, response, http.StatusInternalServerError)
				response = performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick/publication", token, request)
			}
			requireNativeStatus(t, response, want)
			if name != "grant revoked during verification" {
				var after tracker.NativeIssue
				decodeHubResponse(t, performHubAPIRequest(t, f.service, http.MethodGet, f.itemPath, f.token, nil), &after)
				if after.State != before.State || after.Revision != before.Revision || after.ReleaseDelivery != nil {
					t.Fatalf("publication changed delivery: %+v", after)
				}
			}
			var changes int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_requests WHERE work_item_id=?", f.delivery.WorkItemID).Scan(&changes); err != nil {
				t.Fatal(err)
			}
			if changes != 0 {
				t.Fatalf("publication created %d Changes", changes)
			}
			if want == http.StatusOK {
				var result tracker.ReleasePickPublication
				decodeHubResponse(t, response, &result)
				if result.PRNumber != 7 || result.WorkItemID != f.delivery.WorkItemID || result.HeadSHA != request.HeadSHA {
					t.Fatalf("publication=%+v", result)
				}
				requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.itemPath+"/release-pick/publication", token, request), http.StatusOK)
				if p.creates != 1 {
					t.Fatalf("created PRs=%d", p.creates)
				}
				association := f.associate(t, f.request)
				if association.PRNumber != result.PRNumber {
					t.Fatal("publication could not be associated")
				}
			} else if name != "changed retry" && p.creates != 0 {
				t.Fatalf("refused request created %d PRs", p.creates)
			}
		})
	}
}

func TestNativeReleasePickPublicationMCP(t *testing.T) {
	f, p, request := releasePublicationFixture(t, true)
	credential, _, err := f.service.authenticateAPIToken(t.Context(), f.token, "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID, credential: credential}
	args := operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.delivery.WorkItemID), RequestID: request.IdempotencyKey, ExpectedRevision: int64(request.ExpectedRevision), ReleasePick: &request.ReleasePickInput}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operatortool.DecodeChangeArguments(operatortool.PublishReleasePick, raw); err != nil {
		t.Fatal(err)
	}
	result, err := (hubChangeApplication{service: f.service, scope: scope}).MutateChange(t.Context(), operatortool.PublishReleasePick, args)
	if err != nil {
		t.Fatal(err)
	}
	var receipt tracker.ReleasePickPublication
	if err := json.Unmarshal(result.Receipt, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ReleasePickInput != f.request.ReleasePickInput || receipt.WorkItemID != f.delivery.WorkItemID || receipt.URL != "https://github.com/example-org/example-project/pull/7" || p.creates != 1 {
		t.Fatalf("receipt=%+v creates=%d", receipt, p.creates)
	}
}
