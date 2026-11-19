package workitemchange_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReleaseAssociation(t *testing.T) {
	t.Parallel()
	verification := make(releasePickVerification)
	dispatches := make(releaseRefreshDispatches)
	cfg := hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider())
	cfg.ReconcileBackend = verification
	cfg.ConversationDispatch = dispatches
	h := hubfixture.OpenHosted(t, cfg)
	t.Run("refresh and replay", func(t *testing.T) {
		f := seedReleaseAssociation(t, h, verification, "refresh")
		first := f.associateAndCheckStatus(t)
		refresh := f.refresh(f.delivery.Revision + 1)
		hubfixture.RequireStatus(t, h.Request(t, f.owner, http.MethodPost, f.path+"/release-pick", refresh), http.StatusUnprocessableEntity)
		f.seedAudit(t)
		response := h.Request(t, f.owner, http.MethodPost, f.path+"/release-pick", refresh)
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var updated tracker.ReleaseDelivery
		hubfixture.Decode(t, response, &updated)
		if updated.ChangeID != first.ChangeID || updated.VersionID != "" || updated.ReleasePickInput != refresh.ReleasePickInput {
			t.Fatalf("refreshed delivery = %+v", updated)
		}
		if issue := f.readIssue(t); issue.Revision != f.delivery.Revision+2 {
			t.Fatalf("refreshed issue revision = %d", issue.Revision)
		}
		if changes := f.readChanges(t); len(changes) != 1 || changes[0].Revision != 2 || changes[0].CurrentVersion != "" {
			t.Fatalf("refreshed changes = %+v", changes)
		}
		replay := h.Request(t, f.owner, http.MethodPost, f.path+"/release-pick", refresh)
		hubfixture.RequireStatus(t, replay, http.StatusOK)
		if replay.Body.String() != response.Body.String() {
			t.Fatal("refresh replay changed")
		}
	})
	t.Run("provider revoked before write", func(t *testing.T) {
		f := seedReleaseAssociation(t, h, verification, "revoke")
		revoked := false
		f.verifier.onVerify = func(ctx context.Context) error {
			err := h.Provider.RevokeMembership(ctx, "membership_"+f.owner.Identity.Subject)
			revoked = err == nil
			return err
		}
		response := h.Request(t, f.owner, http.MethodPost, f.path+"/release-pick", f.request)
		if !revoked || (response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized) {
			t.Fatalf("provider revoked=%v response=%d: %s", revoked, response.Code, response.Body.String())
		}
		err := h.Repository.ReadTenant(t.Context(), h.Primary, corestore.Serving, func(r tenantstore.Reader) error {
			reader, ok := r.(eventstore.ReleaseDeliveryReader)
			if !ok {
				t.Fatal("repository cannot read release deliveries")
			}
			if _, err := reader.ReleaseDelivery(t.Context(), f.project, f.delivery.WorkItemID); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("revoked provider wrote delivery: %v", err)
			}
			changes, err := r.Changes(t.Context(), f.project, f.delivery.WorkItemID, "", 10)
			if err == nil && len(changes) != 0 {
				t.Fatalf("revoked provider wrote %d Changes", len(changes))
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	t.Run("refresh resumes work from landing", func(t *testing.T) {
		f := seedReleaseAssociation(t, h, verification, "landing")
		f.seedAudit(t)
		hubfixture.RequireStatus(t, h.Request(t, f.owner, http.MethodPost, f.path+"/release-pick", f.request), http.StatusOK)
		issue := f.seedMergingIssue(t)
		delete(dispatches, f.project)
		refresh := f.refresh(issue.Revision)
		hubfixture.RequireStatus(t, h.Request(t, f.owner, http.MethodPost, f.path+"/release-pick", refresh), http.StatusOK)
		after := f.readIssue(t)
		if after.State != "In Progress" || after.Revision != issue.Revision+2 || dispatches[f.project] != string(issue.WorkItemID) {
			t.Fatalf("state=%s revision=%d dispatched item=%s", after.State, after.Revision, dispatches[f.project])
		}
		var events []eventstore.Event
		if err := h.Repository.ReadTenant(t.Context(), h.Primary, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			events, err = r.Events(t.Context(), f.project, issue.WorkItemID, 0, 50)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range events {
			event, err := f.content.Event(t.Context(), row)
			if err != nil {
				t.Fatal(err)
			}
			if event.Type == "workflow.transitioned" && event.Data.ToState == "In Progress" {
				found = true
				if event.Data.Revision != after.Revision {
					t.Fatalf("event revision=%d, issue=%d", event.Data.Revision, after.Revision)
				}
			}
		}
		if !found {
			t.Fatalf("missing refresh workflow event: %+v", events)
		}
	})
}

type releasePickVerification map[string]*releasePickVerifier

type releasePickVerifier struct {
	input    tracker.ReleasePickInput
	onVerify func(context.Context) error
}

func (releasePickVerification) Reconcile(context.Context, githubcontract.ReconcileRequest) (githubcontract.ReconcileSnapshot, error) {
	return githubcontract.ReconcileSnapshot{}, nil
}

func (v releasePickVerification) VerifyReleasePick(ctx context.Context, input tracker.ReleasePickInput) error {
	verifier := v[input.SourceChangeID]
	if verifier == nil || input != verifier.input {
		return githubcontract.ErrReleasePickMismatch
	}
	if verifier.onVerify != nil {
		return verifier.onVerify(ctx)
	}
	return nil
}

func (v releasePickVerification) VerifyReleasePickRefresh(ctx context.Context, _, input tracker.ReleasePickInput) error {
	return v.VerifyReleasePick(ctx, input)
}

func (v releasePickVerification) ReleaseDeliveryStatus(_ context.Context, input tracker.ReleasePickInput) (tracker.ReleaseDeliveryStatus, error) {
	verifier := v[input.SourceChangeID]
	if verifier == nil || input != verifier.input {
		return tracker.ReleaseDeliveryStatus{}, githubcontract.ErrReleasePickMismatch
	}
	return tracker.ReleaseDeliveryStatus{ReleasePickInput: input, ReleaseHeadSHA: input.HeadSHA}, nil
}

type releaseRefreshDispatches map[tracker.ProjectID]string

func (d releaseRefreshDispatches) RequestDispatch(_ context.Context, _ tenantstore.Writer, project tracker.ProjectID, item string, _ leasestore.Fence, _ time.Time) error {
	d[project] = item
	return nil
}

func (d releaseRefreshDispatches) QueueDispatch(ctx context.Context, w tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, now time.Time) error {
	return d.RequestDispatch(ctx, w, project, item, fence, now)
}

type releaseAssociationFixture struct {
	hub      *hubfixture.Hosted
	owner    hubfixture.HostedUser
	project  tracker.ProjectID
	path     string
	content  *eventstore.Content
	delivery tracker.NativeIssue
	version  tracker.ChangeVersion
	request  tracker.AssociateReleasePick
	verifier *releasePickVerifier
}

func seedReleaseAssociation(t *testing.T, h *hubfixture.Hosted, verification releasePickVerification, name string) releaseAssociationFixture {
	t.Helper()
	organization, now := h.Primary, h.Now().UTC().Truncate(time.Microsecond)
	project := hubtest.Project(t, h.Repository, organization, hubtest.ProjectOptions{Name: "Release " + name, RequireDependencies: true})
	hubtest.Policy(t, h.Repository, h.Config.RunDetails, project, hubtest.PolicyOptions{Now: now})
	owner := h.User(t, "release_"+name, "owner", "release-"+name+"@example.test", "", "")
	base := hubfixture.ProjectBase(organization, project.ID)
	create := func(kind string) tracker.NativeIssue {
		body := "## Acceptance criteria\n" + testenv.DistinctText(name+kind, 16) + "\n## Must not break\nKeep the reviewed source.\n## How we know it worked\nRead the release delivery."
		response := h.Request(t, owner, http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: name + "-" + kind}, Title: name + " release " + kind, Body: body, State: "Todo"})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		hubfixture.Decode(t, response, &issue)
		return issue
	}
	source, delivery := create("source"), create("delivery")
	policyID := hubfixture.Policy().ID
	rules := tracker.ChangeReviewPolicy{PolicyID: policyID, RequireReview: true}
	hubfixture.RequireStatus(t, h.Request(t, owner, http.MethodPut, base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: name + "-rules"}, Policy: rules}), http.StatusOK)
	response := h.Request(t, owner, http.MethodPost, base+"/work-items/"+string(source.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: name + "-change"}, Title: "Source Change"})
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	hubfixture.Decode(t, response, &change)
	runner := hubtest.Runner(t, h.Repository, organization, hubtest.RunnerOptions{Projects: []tracker.ProjectID{project.ID}, Operations: []string{runnerauth.Read}, Now: now})
	checkout := "example-org/example-" + name
	version := tracker.ChangeVersion{ID: hubapi.NewID("version"), ChangeID: change.ID, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), Repository: "https://github.com/" + checkout, PolicyID: policyID}, Actor: tracker.Actor{Kind: "human", PrincipalID: "source-author"}, CreatedAt: now}
	mergeSHA := strings.Repeat("c", 40)
	content := eventstore.NewContent(h.Config.RunDetails)
	versionRecord, err := content.PrepareVersion(t.Context(), organization, testJSON(t, version))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		projectRecord, err := w.ProjectRecord(t.Context(), project.ID)
		if err != nil {
			return err
		}
		projectRecord.Settings.CheckoutRepository = checkout
		if _, err = w.UpdateProjectRecord(t.Context(), projectRecord); err != nil {
			return err
		}
		if err = w.ReplaceRunnerCheckoutReport(t.Context(), runnerstore.CheckoutReport{OrganizationID: organization, RunnerID: runner.Record.RunnerID, ProjectID: project.ID, Repository: checkout, ReportedAt: now}); err != nil {
			return err
		}
		item, err := w.WorkItem(t.Context(), project.ID, source.WorkItemID)
		if err != nil {
			return err
		}
		item.WorkflowStateName = new("Done")
		if _, err = w.UpdateWorkItem(t.Context(), item); err != nil {
			return err
		}
		if _, err = w.CreateVersion(t.Context(), versionRecord); err != nil {
			return err
		}
		landed, err := w.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		landed.CurrentVersionID, landed.LandedVersionID = &version.ID, &version.ID
		landed.LandedAt, landed.LandedHeadSHA, landed.LandedMergeSHA, landed.LandedBaseRef = &now, &version.HeadSHA, &mergeSHA, new("develop")
		if _, err = w.UpdateChange(t.Context(), landed); err != nil {
			return err
		}
		return w.SetRelationship(t.Context(), corestore.Relationship{OrganizationID: organization, ProjectID: project.ID, WorkItemID: delivery.WorkItemID, RelatedProjectID: project.ID, RelatedWorkItemID: source.WorkItemID, Type: "dependency"}, "add", now)
	}); err != nil {
		t.Fatal(err)
	}
	input := tracker.ReleasePickInput{SourceIssueID: source.WorkItemID, SourceChangeID: change.ID, SourceMergeSHA: mergeSHA, Repository: version.Repository, PRNumber: 7, HeadRef: "pick/example", HeadSHA: strings.Repeat("d", 40), BaseRef: "release/v1.2", BaseSHA: strings.Repeat("e", 40), FrozenSHA: strings.Repeat("f", 40)}
	verifier := &releasePickVerifier{input: input}
	verification[change.ID] = verifier
	return releaseAssociationFixture{
		hub: h, owner: owner, project: project.ID, path: base + "/work-items/" + string(delivery.WorkItemID), content: content, delivery: delivery, version: version, verifier: verifier,
		request: tracker.AssociateReleasePick{Mutation: tracker.Mutation{IdempotencyKey: name + "-associate"}, ExpectedRevision: delivery.Revision, ReleasePickInput: input},
	}
}

func (f releaseAssociationFixture) refresh(revision tracker.Revision) tracker.AssociateReleasePick {
	request := f.request
	request.IdempotencyKey += "-refresh"
	request.ExpectedRevision = revision
	request.HeadSHA, request.BaseSHA = strings.Repeat("1", 40), strings.Repeat("2", 40)
	f.verifier.input = request.ReleasePickInput
	return request
}

func (f releaseAssociationFixture) associateAndCheckStatus(t *testing.T) tracker.ReleaseDelivery {
	t.Helper()
	path := f.path + "/release-pick"
	first := f.hub.Request(t, f.owner, http.MethodPost, path, f.request)
	hubfixture.RequireStatus(t, first, http.StatusOK)
	var delivery tracker.ReleaseDelivery
	hubfixture.Decode(t, first, &delivery)
	if delivery.ChangeID == "" || delivery.ReleasePickInput != f.request.ReleasePickInput {
		t.Fatalf("delivery = %+v", delivery)
	}
	replay := f.hub.Request(t, f.owner, http.MethodPost, path, f.request)
	hubfixture.RequireStatus(t, replay, http.StatusOK)
	if replay.Body.String() != first.Body.String() {
		t.Fatal("replay changed delivery")
	}
	same := f.request
	same.IdempotencyKey += "-same-pick"
	replay = f.hub.Request(t, f.owner, http.MethodPost, path, same)
	hubfixture.RequireStatus(t, replay, http.StatusOK)
	var repeated tracker.ReleaseDelivery
	hubfixture.Decode(t, replay, &repeated)
	if repeated != delivery {
		t.Fatalf("same pick returned a different delivery: %+v", repeated)
	}
	if issue := f.readIssue(t); issue.Revision != f.delivery.Revision+1 {
		t.Fatalf("associated issue revision = %d", issue.Revision)
	}
	if changes := f.readChanges(t); len(changes) != 1 || changes[0].ID != delivery.ChangeID {
		t.Fatalf("associated changes = %+v", changes)
	}
	statusResponse := f.hub.Request(t, f.owner, http.MethodGet, path, nil)
	hubfixture.RequireStatus(t, statusResponse, http.StatusOK)
	var status tracker.ReleaseDeliveryStatus
	hubfixture.Decode(t, statusResponse, &status)
	if status.ReleasePickInput != f.request.ReleasePickInput || status.ReleaseHeadSHA != f.request.HeadSHA {
		t.Fatalf("release status = %+v", status)
	}
	return delivery
}

func (f releaseAssociationFixture) readIssue(t *testing.T) tracker.NativeIssue {
	t.Helper()
	response := f.hub.Request(t, f.owner, http.MethodGet, f.path, nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	hubfixture.Decode(t, response, &issue)
	return issue
}

func (f releaseAssociationFixture) readChanges(t *testing.T) []tracker.ChangeRequest {
	t.Helper()
	response := f.hub.Request(t, f.owner, http.MethodGet, f.path+"/changes", nil)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var changes []tracker.ChangeRequest
	hubfixture.Decode(t, response, &changes)
	return changes
}

func (f releaseAssociationFixture) seedAudit(t *testing.T) {
	t.Helper()
	now := f.hub.Now()
	audit := &tracker.SecurityAuditReview{SessionID: 42, DiffDigest: policy.Digest([]byte("diff")), Verdict: "pass", Run: securityaudit.Run{
		ID: 42, InvocationID: f.version.ID, ProjectID: string(f.project), IssueID: string(f.request.SourceIssueID), Repository: f.version.Repository, BaseSHA: f.version.BaseSHA, HeadSHA: f.version.HeadSHA,
		ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess,
		WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now.Add(-time.Second), CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings",
	}}
	review := tracker.ChangeReview{ID: hubapi.NewID("review"), VersionID: f.version.ID, Decision: "approved", SecurityAudit: audit, Actor: tracker.Actor{Kind: "human", PrincipalID: "example-reviewer"}, CreatedAt: now}
	organization := f.hub.Primary
	row, err := f.content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: organization, ChangeID: f.version.ChangeID, Kind: "review"}, testJSON(t, review))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.hub.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.AppendEvidence(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f releaseAssociationFixture) seedMergingIssue(t *testing.T) tracker.NativeIssue {
	t.Helper()
	issue := f.readIssue(t)
	issue.State = "Merging"
	issue.Revision++
	organization := f.hub.Primary
	version, err := f.content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: organization, ProjectID: f.project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, testJSON(t, issue))
	if err != nil {
		t.Fatal(err)
	}
	if err = f.hub.Repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		project.States = append(project.States, tracker.NativeState{Name: "Merging", Transitions: []string{"In Progress"}})
		if _, err = w.UpdateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		row, err := w.WorkItem(t.Context(), f.project, issue.WorkItemID)
		if err != nil {
			return err
		}
		row.WorkflowStateName = &issue.State
		if _, err = w.UpdateWorkItem(t.Context(), row); err != nil {
			return err
		}
		_, err = w.CreateCollaborationVersion(t.Context(), version)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return issue
}
