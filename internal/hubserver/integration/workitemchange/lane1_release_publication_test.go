package workitemchange_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleasePublication(t *testing.T) {
	t.Parallel()
	backend := releasePublicationBackend{releasePickVerification: make(releasePickVerification), publishers: make(map[string]*releasePublisherProbe)}
	cfg := hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider())
	cfg.ReconcileBackend = backend
	h := hubfixture.OpenHosted(t, cfg)
	for _, name := range []string{"publish", "lost response", "missing audit", "wrong source", "source advanced", "wrong repository", "missing dependency", "held delivery", "terminal workflow state", "stale revision", "revoked during verification", "provider revoked during verification", "hold during verification"} {
		t.Run(name, func(t *testing.T) {
			f := seedReleaseAssociation(t, h, backend.releasePickVerification, strings.ReplaceAll(name, " ", "-"))
			publisher := &releasePublisherProbe{lostResponse: name == "lost response"}
			backend.publishers[f.version.ChangeID] = publisher
			if name != "missing audit" {
				f.seedAudit(t)
			}
			request := tracker.PublishReleasePick{Mutation: tracker.Mutation{IdempotencyKey: f.request.IdempotencyKey + "-publish"}, ReleasePickInput: f.request.ReleasePickInput, ExpectedRevision: f.request.ExpectedRevision}
			request.PRNumber = 0
			want := http.StatusOK
			switch name {
			case "missing audit":
				want = http.StatusUnprocessableEntity
			case "wrong source":
				request.SourceMergeSHA = strings.Repeat("9", 40)
				want = http.StatusUnprocessableEntity
			case "source advanced":
				f.seedNextReleaseSourceVersion(t, true, nil)
				want = http.StatusUnprocessableEntity
			case "wrong repository":
				request.Repository = "https://github.com/example-org/other-project"
				want = http.StatusUnprocessableEntity
			case "missing dependency":
				if err := h.Repository.WriteTenant(t.Context(), h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
					return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: h.Primary, ProjectID: f.project, WorkItemID: f.delivery.WorkItemID, RelatedProjectID: f.project, RelatedWorkItemID: f.request.SourceIssueID, Type: "dependency"}, "remove", h.Now())
				}); err != nil {
					t.Fatal(err)
				}
				want = http.StatusUnprocessableEntity
			case "terminal workflow state":
				f.markReleaseWorkflowTerminal(t)
				want = http.StatusUnprocessableEntity
			case "stale revision":
				request.ExpectedRevision++
				want = http.StatusConflict
			case "revoked during verification":
				want = http.StatusForbidden
				publisher.beforeCreate = func(ctx context.Context) error {
					return h.Repository.WriteTenant(ctx, h.Primary, corestore.Serving, func(w tenantstore.Writer) error {
						return w.RevokeAccessMember(ctx, f.owner.Identity.Subject, h.Now())
					})
				}
			case "provider revoked during verification":
				want = http.StatusForbidden
				publisher.beforeCreate = func(ctx context.Context) error {
					return h.Provider.RevokeMembership(ctx, "membership_"+f.owner.Identity.Subject)
				}
			case "held delivery":
				request.ExpectedRevision = f.holdReleaseIssue(t)
				want = http.StatusUnprocessableEntity
			case "hold during verification":
				want = http.StatusConflict
				publisher.beforeCreate = func(context.Context) error {
					f.holdReleaseIssue(t)
					return nil
				}
			}

			path := f.path + "/release-pick/publication"
			response := h.Request(t, f.owner, http.MethodPost, path, request)
			if name == "lost response" {
				hubfixture.RequireStatus(t, response, http.StatusInternalServerError)
				response = h.Request(t, f.owner, http.MethodPost, path, request)
			}
			hubfixture.RequireStatus(t, response, want)
			if want != http.StatusOK {
				if publisher.beforeCreate != nil && !publisher.reachedCreate {
					t.Fatal("publication did not reach the authorization recheck")
				}
				if publisher.creates != 0 {
					t.Fatal("rejected publication created a pull request")
				}
				return
			}
			var result tracker.ReleasePickPublication
			hubfixture.Decode(t, response, &result)
			if result.PRNumber != f.request.PRNumber || result.WorkItemID != f.delivery.WorkItemID || publisher.creates != 1 {
				t.Fatalf("publication=%+v creates=%d", result, publisher.creates)
			}
			if name == "lost response" {
				publisher.publication.URL += "?replay=1"
			}
			replay := h.Request(t, f.owner, http.MethodPost, path, request)
			hubfixture.RequireStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() || publisher.creates != 1 {
				t.Fatal("retry changed publication")
			}
			request.FencingToken = 1
			hubfixture.RequireStatus(t, h.Request(t, f.owner, http.MethodPost, path, request), http.StatusConflict)
		})
	}
	t.Run("repository spelling conflict", func(t *testing.T) {
		f := seedReleaseAssociation(t, h, backend.releasePickVerification, "repository-spelling")
		f.seedNextReleaseSourceVersion(t, false, func(version *tracker.ChangeVersion) {
			version.External = &tracker.ChangeExternalReference{Provider: "github", ID: "7", URL: version.Repository + "/pull/7"}
			version.Repository = strings.ToUpper(version.Repository)
		})
		hubfixture.RequireStatus(t, h.Request(t, f.owner, http.MethodPost, f.path+"/release-pick", f.request), http.StatusConflict)
	})
}

type releasePublicationBackend struct {
	releasePickVerification
	publishers map[string]*releasePublisherProbe
}

type releasePublisherProbe struct {
	beforeCreate  func(context.Context) error
	lostResponse  bool
	reachedCreate bool
	creates       int
	publication   tracker.ReleasePickPublication
}

func (b releasePublicationBackend) PublishReleasePick(ctx context.Context, item tracker.NativeWorkItemID, input tracker.ReleasePickInput, authorize func(context.Context) error) (tracker.ReleasePickPublication, error) {
	publisher, verifier := b.publishers[input.SourceChangeID], b.releasePickVerification[input.SourceChangeID]
	if publisher == nil || verifier == nil {
		return tracker.ReleasePickPublication{}, githubcontract.ErrReleasePickMismatch
	}
	publisher.reachedCreate = true
	if publisher.beforeCreate != nil {
		if err := publisher.beforeCreate(ctx); err != nil {
			return tracker.ReleasePickPublication{}, err
		}
	}
	if err := authorize(ctx); err != nil {
		return tracker.ReleasePickPublication{}, err
	}
	expected := verifier.input
	expected.PRNumber = 0
	if input != expected {
		return tracker.ReleasePickPublication{}, githubcontract.ErrReleasePickMismatch
	}
	if publisher.creates == 0 {
		publisher.creates++
		publisher.publication = tracker.ReleasePickPublication{ReleasePickInput: verifier.input, WorkItemID: item, URL: fmt.Sprintf("%s/pull/%d", verifier.input.Repository, verifier.input.PRNumber)}
	}
	if publisher.lostResponse {
		publisher.lostResponse = false
		return tracker.ReleasePickPublication{}, errors.New("lost create response")
	}
	return publisher.publication, nil
}

func (f releaseAssociationFixture) holdReleaseIssue(t *testing.T) tracker.Revision {
	t.Helper()
	issue := f.readIssue(t)
	body := issue.Body + "\n\ndetent:no-dispatch"
	response := f.hub.Request(t, f.owner, http.MethodPatch, f.path, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: f.request.IdempotencyKey + "-hold"}, ExpectedRevision: issue.Revision, Body: &body})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	hubfixture.Decode(t, response, &issue)
	return issue.Revision
}

func (f releaseAssociationFixture) markReleaseWorkflowTerminal(t *testing.T) {
	t.Helper()
	if err := f.hub.Repository.WriteTenant(t.Context(), f.hub.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project)
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
}

func (f releaseAssociationFixture) seedNextReleaseSourceVersion(t *testing.T, current bool, patch func(*tracker.ChangeVersion)) {
	t.Helper()
	next := f.version
	next.ID, next.Number = hubapi.NewID("version"), next.Number+1
	if patch != nil {
		patch(&next)
	}
	row, err := f.content.PrepareVersion(t.Context(), f.hub.Primary, testJSON(t, next))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.hub.Repository.WriteTenant(t.Context(), f.hub.Primary, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.CreateVersion(t.Context(), row); err != nil || !current {
			return err
		}
		change, err := w.Change(t.Context(), next.ChangeID)
		if err != nil {
			return err
		}
		change.CurrentVersionID = &row.ID
		_, err = w.UpdateChange(t.Context(), change)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
