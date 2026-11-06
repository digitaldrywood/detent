package hubserver

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleasePublicationPostgres(t *testing.T) {
	for _, name := range []string{"publish", "lost response", "missing audit", "wrong source", "source advanced", "wrong repository", "missing dependency", "held delivery", "terminal workflow state", "stale revision", "revoked during verification", "provider revoked during verification", "hold during verification"} {
		t.Run(name, func(t *testing.T) {
			f := newReleaseTenantFixture(t)
			publisher := &releasePublisherFixture{releaseVerifierFixture: f.verifier}
			f.f.service.config.ReconcileBackend = publisher
			now := f.f.service.config.now()
			if name != "missing audit" {
				seedReleaseTenantAudit(t, f)
			}
			request := tracker.PublishReleasePick{Mutation: tracker.Mutation{IdempotencyKey: "publish-release"}, ReleasePickInput: f.request.ReleasePickInput, ExpectedRevision: f.request.ExpectedRevision}
			request.PRNumber = 0
			expected := http.StatusOK
			switch name {
			case "missing audit":
				expected = http.StatusUnprocessableEntity
			case "wrong source":
				request.SourceMergeSHA = strings.Repeat("9", 40)
				expected = http.StatusUnprocessableEntity
			case "source advanced":
				next := f.version
				next.ID, next.Number = newNativeID("version"), next.Number+1
				raw, err := json.Marshal(next)
				if err != nil {
					t.Fatal(err)
				}
				row, err := eventstore.NewContent(f.f.service.runDetailStore()).PrepareVersion(t.Context(), "org_security", raw)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateVersion(t.Context(), row); err != nil {
						return err
					}
					change, err := w.Change(t.Context(), f.change.ID)
					if err != nil {
						return err
					}
					change.CurrentVersionID = &row.ID
					_, err = w.UpdateChange(t.Context(), change)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				expected = http.StatusUnprocessableEntity
			case "wrong repository":
				request.Repository = "https://github.com/example-org/other-project"
				expected = http.StatusUnprocessableEntity
			case "missing dependency":
				if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: "org_security", ProjectID: f.f.project, WorkItemID: f.delivery.WorkItemID, RelatedProjectID: f.f.project, RelatedWorkItemID: f.source.WorkItemID, Type: "dependency"}, "remove", now)
				}); err != nil {
					t.Fatal(err)
				}
				expected = http.StatusUnprocessableEntity
			case "terminal workflow state":
				if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
					project, err := w.ProjectRecord(t.Context(), f.f.project)
					if err != nil {
						return err
					}
					for i := range project.States {
						if project.States[i].Name == f.delivery.State {
							project.States[i].Terminal = true
							project.States[i].Dispatchable = false
						}
					}
					_, err = w.UpdateProjectRecord(t.Context(), project)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				expected = http.StatusUnprocessableEntity
			case "stale revision":
				request.ExpectedRevision++
				expected = http.StatusConflict
			case "lost response":
				publisher.lostResponse = true
			case "revoked during verification":
				expected = http.StatusForbidden
				publisher.beforeCreate = func() {
					if err := f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
						return w.RevokeAccessMember(t.Context(), f.owner.identity.Subject, now)
					}); err != nil {
						t.Fatal(err)
					}
				}
			case "provider revoked during verification":
				expected = http.StatusForbidden
				publisher.beforeCreate = func() {
					if err := f.f.provider.RevokeMembership(t.Context(), "membership_"+f.owner.identity.Subject); err != nil {
						t.Fatal(err)
					}
				}
			case "hold during verification", "held delivery":
				expected = http.StatusConflict
				hold := func() {
					revision := holdReleaseTenantDelivery(t, f)
					if name == "held delivery" {
						request.ExpectedRevision = revision
					}
				}
				if name == "held delivery" {
					hold()
					expected = http.StatusUnprocessableEntity
				} else {
					publisher.beforeCreate = hold
				}

			}
			path := f.f.base + "/work-items/" + string(f.delivery.WorkItemID) + "/release-pick/publication"
			response := f.f.request(t, f.owner, http.MethodPost, path, request)
			if name == "lost response" {
				requireNativeStatus(t, response, http.StatusInternalServerError)
				response = f.f.request(t, f.owner, http.MethodPost, path, request)
			}

			requireNativeStatus(t, response, expected)
			if expected != http.StatusOK {
				if publisher.creates != 0 {
					t.Fatal("rejected publication created PR")
				}
				return
			}
			var result tracker.ReleasePickPublication
			decodeHubResponse(t, response, &result)
			if result.PRNumber != 7 || result.WorkItemID != f.delivery.WorkItemID || publisher.creates != 1 {
				t.Fatalf("publication=%+v creates=%d", result, publisher.creates)
			}
			replay := f.f.request(t, f.owner, http.MethodPost, path, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() || publisher.creates != 1 {
				t.Fatal("retry changed publication")
			}
			request.SourceMergeSHA = strings.Repeat("9", 40)
			rejected := f.f.request(t, f.owner, http.MethodPost, path, request)
			if rejected.Code == http.StatusOK {
				t.Fatal("changed command replay accepted")
			}
		})
	}
}

func TestReleasePRConflictRepositoryCasePostgres(t *testing.T) {
	f := newReleaseTenantFixture(t)
	version := f.version
	version.ID, version.Number = newNativeID("version"), version.Number+1
	version.Repository = strings.ToUpper(version.Repository)
	version.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: "https://github.com/example-org/example-project/pull/7"}
	raw, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	row, err := eventstore.NewContent(f.f.service.runDetailStore()).PrepareVersion(t.Context(), "org_security", raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.CreateVersion(t.Context(), row); return err }); err != nil {
		t.Fatal(err)
	}
	response := f.f.request(t, f.owner, http.MethodPost, f.f.base+"/work-items/"+string(f.delivery.WorkItemID)+"/release-pick", f.request)
	requireNativeStatus(t, response, http.StatusConflict)
}

func holdReleaseTenantDelivery(t *testing.T, f *releaseTenantFixture) tracker.Revision {
	t.Helper()
	issue := f.delivery
	issue.Body += "\n\ndetent:no-dispatch"
	issue.Revision++
	fields, err := f.f.service.prepareWorkItemFields(t.Context(), issue.OrganizationID, workItemRecordPlan{issue: issue})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	version, err := eventstore.NewContent(f.f.service.runDetailStore()).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.repository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.BodyObjectKey = &fields.Body
		if _, err = w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return issue.Revision
}
