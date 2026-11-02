package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type failingChangeEventRepository struct {
	tenantstore.Repository
}

type committedChangeWriteResponseLossRepository struct {
	tenantstore.Repository
}

type changeArchiveMutationStore struct {
	attachment.DetailStore
	afterLandingPayload func(context.Context) error
	fired               bool
}

type failingChangeEventWriter struct {
	tenantstore.Writer
}

type changeLandingLockRepository struct {
	tenantstore.Repository
	locked       chan struct{}
	prepared     chan struct{}
	appendStart  chan struct{}
	release      chan struct{}
	lockedOnce   sync.Once
	preparedOnce sync.Once
	appendOnce   sync.Once
}

type changeLandingLockWriter struct {
	tenantstore.Writer
	repository *changeLandingLockRepository
}

type changeReviewLockRepository struct {
	tenantstore.Repository
	writing *bool
	locked  bool
}

type changeReviewLockWriter struct {
	tenantstore.Writer
	repository *changeReviewLockRepository
}

type changeReviewDetailReadGuard struct {
	attachment.DetailStore
	writing    *bool
	readInside bool
}

func (r failingChangeEventRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(failingChangeEventWriter{Writer: w})
	})
}

func (r committedChangeWriteResponseLossRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if err := r.Repository.WriteTenant(ctx, organization, workload, write); err != nil {
		return err
	}
	return errors.New("injected committed write response loss")
}

func (s *changeArchiveMutationStore) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	ref, err := s.DetailStore.StoreDetail(ctx, organization, raw)
	if err != nil || s.fired {
		return ref, err
	}
	var change tracker.ChangeRequest
	if json.Unmarshal(raw, &change) == nil && change.Landed != nil {
		s.fired = true
		if err := s.afterLandingPayload(ctx); err != nil {
			return attachment.ArchiveReference{}, err
		}
	}
	return ref, nil
}

func (w failingChangeEventWriter) AppendEvent(context.Context, eventstore.Event) (eventstore.Event, error) {
	return eventstore.Event{}, errors.New("injected event write failure")
}

func (r *changeLandingLockRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(changeLandingLockWriter{Writer: w, repository: r})
	})
}

func (w changeLandingLockWriter) LockChange(ctx context.Context, id string) (eventstore.Change, error) {
	change, err := w.Writer.LockChange(ctx, id)
	if err == nil {
		w.repository.lockedOnce.Do(func() { close(w.repository.locked) })
	}
	return change, err
}

func (w changeLandingLockWriter) CreateLandingReceipt(ctx context.Context, receipt eventstore.LandingReceipt) (eventstore.LandingReceipt, error) {
	w.repository.preparedOnce.Do(func() { close(w.repository.prepared) })
	select {
	case <-w.repository.release:
		return eventstore.LandingReceipt{}, errors.New("injected landing transaction failure")
	case <-ctx.Done():
		return eventstore.LandingReceipt{}, ctx.Err()
	}
}

func (w changeLandingLockWriter) AppendEvidence(ctx context.Context, evidence eventstore.Evidence) (eventstore.Evidence, error) {
	w.repository.appendOnce.Do(func() { close(w.repository.appendStart) })
	return w.Writer.AppendEvidence(ctx, evidence)
}

func (r *changeReviewLockRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	*r.writing = true
	defer func() { *r.writing = false }()
	r.locked = false
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(changeReviewLockWriter{Writer: w, repository: r})
	})
}

func (w changeReviewLockWriter) LockChange(ctx context.Context, id string) (eventstore.Change, error) {
	change, err := w.Writer.LockChange(ctx, id)
	if err == nil {
		w.repository.locked = true
	}
	return change, err
}

func (w changeReviewLockWriter) AppendEvidence(ctx context.Context, evidence eventstore.Evidence) (eventstore.Evidence, error) {
	if !w.repository.locked {
		return eventstore.Evidence{}, errors.New("Change evidence appended without locking its sequence")
	}
	return w.Writer.AppendEvidence(ctx, evidence)
}

func (s *changeReviewDetailReadGuard) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if *s.writing {
		s.readInside = true
		return nil, errors.New("archive read while Change writer is held")
	}
	return s.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestChangeRecordBackendParity(t *testing.T) {
	repository, f, owner := newChangePolicyPostgresFixture(t)
	createItem := func(key string) tracker.NativeIssue {
		t.Helper()
		response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example issue " + key, Body: workItemRecordBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return issue
	}
	primary, linked := createItem("primary"), createItem("linked")
	changePath := f.base + "/work-items/" + string(primary.WorkItemID) + "/changes"
	request := tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "create-change"}, Title: "Example Change", Body: "Immutable discussion", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}}
	created := f.request(t, owner, http.MethodPost, changePath, request)
	requireNativeStatus(t, created, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, created, &change)
	if change.WorkItemID != primary.WorkItemID || change.Body != request.Body || len(change.LinkedIssues) != 2 {
		t.Fatalf("created change=%+v", change)
	}
	replay := f.request(t, owner, http.MethodPost, changePath, request)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != created.Body.String() {
		t.Fatalf("create replay changed response: %s vs %s", replay.Body.String(), created.Body.String())
	}
	discussion := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "discussion"}, Body: "Append-only discussion"}
	discussionPath := changePath + "/" + change.ID + "/discussion"
	posted := f.request(t, owner, http.MethodPost, discussionPath, discussion)
	requireNativeStatus(t, posted, http.StatusOK)
	discussionReplay := f.request(t, owner, http.MethodPost, discussionPath, discussion)
	requireNativeStatus(t, discussionReplay, http.StatusOK)
	if discussionReplay.Body.String() != posted.Body.String() {
		t.Fatal("discussion replay changed the archived response")
	}
	list := f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(linked.WorkItemID)+"/changes", nil)
	requireNativeStatus(t, list, http.StatusOK)
	var changes []tracker.ChangeRequest
	decodeHubResponse(t, list, &changes)
	if len(changes) != 1 || changes[0].ID != change.ID || len(changes[0].LinkedIssues) != 2 {
		t.Fatalf("linked issue changes=%+v", changes)
	}
	detailResponse := f.request(t, owner, http.MethodGet, changePath+"/"+change.ID, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, detailResponse, &detail)
	if detail.Change.ID != change.ID || detail.Change.Body != request.Body || len(detail.Discussion) != 1 || detail.Discussion[0].Body != discussion.Body {
		t.Fatalf("change detail=%+v", detail.Change)
	}
	var localCount int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_requests WHERE id=?", change.ID).Scan(&localCount); err != nil {
		t.Fatal(err)
	}
	if localCount != 0 {
		t.Fatal("selected change route wrote to SQLite")
	}
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		links, err := r.Links(t.Context(), change.ID, "", 10)
		if err != nil {
			return err
		}
		if len(links) != 2 {
			t.Fatalf("stored links=%+v", links)
		}
		events, err := r.Events(t.Context(), f.project, primary.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		foundCreated, foundDiscussion := false, false
		for _, event := range events {
			foundCreated = foundCreated || event.Type == "change.created"
			foundDiscussion = foundDiscussion || event.Type == "change.discussion"
		}
		if !foundCreated || !foundDiscussion {
			t.Fatal("Change and discussion events were not committed with their records")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChangeRecordReceiptRejectsChangedRequest(t *testing.T) {
	_, f, owner := newChangePolicyPostgresFixture(t)
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, response, &issue)
	changePath := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes"
	first := tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "same-key"}, Title: "Original title", Body: "Example body"}
	response = f.request(t, owner, http.MethodPost, changePath, first)
	requireNativeStatus(t, response, http.StatusOK)
	changed := first
	changed.Title = "Changed title"
	response = f.request(t, owner, http.MethodPost, changePath, changed)
	requireNativeStatus(t, response, http.StatusConflict)
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
}

func TestChangeReviewPolicyUsesSelectedTenantPolicyAndReceipt(t *testing.T) {
	repository, f, owner := newChangePolicyPostgresFixture(t)
	descriptor := hubTestPolicy()
	path := f.base + "/change-review-policy"
	response := f.request(t, owner, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var current tracker.ChangeReviewPolicy
	decodeHubResponse(t, response, &current)
	if current.ID != defaultChangeReviewPolicy(descriptor).ID {
		t.Fatalf("default policy = %+v, want %+v", current, defaultChangeReviewPolicy(descriptor))
	}
	updated := current
	updated.RequireReview = true
	request := tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "update-review-policy"}, ExpectedID: current.ID, Policy: updated}
	response = f.request(t, owner, http.MethodPut, path, request)
	requireNativeStatus(t, response, http.StatusOK)
	var saved tracker.ChangeReviewPolicy
	decodeHubResponse(t, response, &saved)
	if saved.ID == current.ID || !saved.RequireReview || saved.PolicyID != descriptor.ID {
		t.Fatalf("saved review policy = %+v", saved)
	}
	replay := f.request(t, owner, http.MethodPut, path, request)
	requireNativeStatus(t, replay, http.StatusOK)
	var replayed tracker.ChangeReviewPolicy
	decodeHubResponse(t, replay, &replayed)
	if replayed.ID != saved.ID || replayed.PolicyID != saved.PolicyID || replayed.RequireReview != saved.RequireReview {
		t.Fatalf("policy replay changed value: %+v vs %+v", replayed, saved)
	}
	stale := request
	stale.IdempotencyKey = "stale-review-policy"
	stale.ExpectedID = current.ID
	response = f.request(t, owner, http.MethodPut, path, stale)
	requireNativeStatus(t, response, http.StatusConflict)
	itemsPath := f.base + "/work-items"
	response = f.request(t, owner, http.MethodPost, itemsPath, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-summary-item"}, Title: "Summary issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var item tracker.NativeIssue
	decodeHubResponse(t, response, &item)
	response = f.request(t, owner, http.MethodPost, itemsPath+"/"+string(item.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "create-summary-change"}, Title: "Summary change", Body: "Example change"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	var member accessstore.Member
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), owner.identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: "org_security", project: f.project, credential: apiCredential{ID: member.PrincipalID, Scope: apiScopeOperator, Hosted: owner.identity.Hosted, HostedRole: "owner", HostedMembership: member.MembershipID, HostedPrincipal: member.PrincipalID, NativeOnly: true}}
	reference, summary, found, err := f.service.selectedLatestChangeSummary(t.Context(), scope, string(item.WorkItemID), f.service.config.now())
	if err != nil || !found || reference.ChangeID != change.ID || reference.VersionID != "" || summary.Status != "draft" {
		t.Fatalf("latest selected Change = %+v, %+v, found=%v, err=%v", reference, summary, found, err)
	}
}

func TestApprovedChangeReviewRejectsPolicyChangedAfterPreflight(t *testing.T) {
	repository, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 3)
	service := f.service
	organization, project := tracker.OrganizationID("org_security"), f.project
	itemResponse := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "policy-race-item"}, Title: "Policy race issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, itemResponse, http.StatusOK)
	var item tracker.NativeIssue
	decodeHubResponse(t, itemResponse, &item)
	changePath := f.base + "/work-items/" + string(item.WorkItemID) + "/changes"
	changeResponse := f.request(t, owner, http.MethodPost, changePath, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "policy-race-change"}, Title: "Policy race change", Body: "Example source"})
	requireNativeStatus(t, changeResponse, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, changeResponse, &change)
	changePath += "/" + change.ID

	service.config.NativeRunEvents = &nativeRunEventParticipantFixture{}
	attemptID, runID, leaseID := newNativeID("attempt"), newNativeID("run"), newNativeID("lease")
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		workItem, err := w.WorkItem(t.Context(), project, item.WorkItemID)
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "policy-race-machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		_, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: "policy-race-machine", SessionID: "policy-race-session", LeaseID: leaseID, TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var member accessstore.Member
	var principal accessstore.Credential
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		principal, err = r.AccessCredential(t.Context(), member.PrincipalID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: organization, project: project, credential: apiCredential{ID: principal.ID, Hash: principal.Hash, Scope: apiScopeOperator, Hosted: owner.identity.Hosted, HostedRole: "owner", HostedMembership: member.MembershipID, HostedPrincipal: member.PrincipalID, NativeOnly: true}}
	policy := hubTestPolicy()
	start := tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: "policy-race-run-start"}, Type: "run.started", SchemaVersion: 1, Data: tracker.NativeRunData{AttemptID: attemptID, RunID: runID, LeaseID: tracker.LeaseID(leaseID), FencingToken: 1, PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "example"}}}
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(item.WorkItemID), start); err != nil {
		t.Fatal(err)
	}
	finish := start
	finish.IdempotencyKey = "policy-race-run-finish"
	finish.Type = "run.finished"
	finish.Data.Sequence = 2
	finish.Data.Outcome = "succeeded"
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(item.WorkItemID), finish); err != nil {
		t.Fatal(err)
	}
	input := changeTestInput()
	input.AttemptID, input.RunID = attemptID, runID
	bundle := changeTestSource(t, &input)
	publish := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "policy-race-publish"}, ChangeVersionInput: input, SourceBundle: bundle}
	versionResponse := f.request(t, owner, http.MethodPost, changePath+"/versions", publish)
	requireNativeStatus(t, versionResponse, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, versionResponse, &version)
	detailResponse := f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, detailResponse, &detail)
	approve := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "policy-race-approve"}, ExpectedRevision: detail.Change.Revision, ExpectedVersionID: version.ID, Decision: "approved", Body: "Reviewed current source"}

	var policyRows currentChangePolicyRows
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		policyRows, err = readCurrentChangePolicyRows(t.Context(), r, scope)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	originalRules := defaultChangeReviewPolicy(policy)
	if policyRows.hasReview {
		var err error
		originalRules, err = eventstore.NewContent(service.runDetailStore()).ReviewPolicy(t.Context(), policyRows.Review)
		if err != nil {
			t.Fatal(err)
		}
	}
	changedRules := originalRules
	changedRules.RequireReview = !changedRules.RequireReview
	updatedAt := service.config.now().UTC().Truncate(time.Microsecond)
	changedRow, err := eventstore.NewContent(service.runDetailStore()).PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: organization, ProjectID: project, PolicyRevisionID: policyRows.Project.PolicyID, Revision: policyRows.Review.Revision + 1, UpdatedBy: principal.ID, UpdatedAt: updatedAt}, changedRules)
	if err != nil {
		t.Fatal(err)
	}
	expectedReviewID := ""
	if policyRows.hasReview {
		expectedReviewID = policyRows.Review.ReviewPolicyID
	}
	interleaved := &projectUpdateBeforeWriteRepository{Repository: repository}
	interleaved.before = func() error {
		return repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(t.Context(), project); err != nil {
				return err
			}
			return w.SetReviewPolicy(t.Context(), changedRow, expectedReviewID)
		})
	}
	originalDetails := service.config.RunDetails
	var writing bool
	guardDetails := &changeReviewDetailReadGuard{DetailStore: originalDetails, writing: &writing}
	service.config.RunDetails = guardDetails
	service.config.TenantRepository = &changeReviewLockRepository{Repository: interleaved, writing: &writing}
	response := f.request(t, owner, http.MethodPost, changePath+"/versions/"+version.ID+"/reviews", approve)
	service.config.RunDetails = originalDetails
	service.config.TenantRepository = repository
	requireNativeStatus(t, response, http.StatusConflict)
	if guardDetails.readInside {
		t.Fatal("approval hydrated archived policy content while its writer transaction was held")
	}
	if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		evidence, err := r.Evidence(t.Context(), change.ID, 0, 20)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(evidence, func(row eventstore.Evidence) bool { return row.Kind == "review" }) {
			t.Fatal("policy change race retained approval evidence")
		}
		if _, err := r.Receipt(t.Context(), principal.ID, "POST "+changePath+"/versions/"+version.ID+"/reviews", approve.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("policy change race receipt lookup error = %v, want not found", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func newChangePolicyPostgresFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser) {
	repository, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 2)
	return repository, f, owner
}

func newChangePolicyPostgresFixtureWithAdmin(t *testing.T, maxConnections int) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	repository, admin := projectRecordPostgresWithAdmin(t, maxConnections)
	f := newHostedSecurityFixture(t, func(cfg *Config) {
		cfg.AccessRepository = repository
		cfg.TenantRepository = repository
		cfg.RunDetails = &memoryRunDetails{}
	})
	now := f.service.config.now()
	descriptor := hubTestPolicy()
	content := eventstore.NewContent(f.service.runDetailStore())
	policyRevision, err := content.PrepareProjectPolicy(t.Context(), "org_security", descriptor)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = f.project
	policyRevision.ApprovedBy = "example-owner"
	policyRevision.ApprovedAt = now.UTC().Truncate(time.Microsecond)
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"}); err != nil {
			return err
		}
		project := projectstore.Record{Core: corestore.Project{OrganizationID: "org_security", ID: f.project, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}
		if _, err := w.CreateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
	}); err != nil {
		t.Fatal(err)
	}
	owner := f.user(t, "review_admin", "owner", "review-admin@example.test", "", "")
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_security", UserID: member.UserID, ProjectID: f.project, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	return repository, f, owner, admin
}

func TestChangeReviewPolicyRechecksDemotedAdminInTenantWriter(t *testing.T) {
	repository, f, owner := newChangePolicyPostgresFixture(t)
	now := f.service.config.now()
	var member accessstore.Member
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		var err error
		member, err = w.AccessMember(t.Context(), owner.identity.Subject)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rules := defaultChangeReviewPolicy(hubTestPolicy())
	rules.RequireReview = true
	request := tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "demoted-review-admin"}, ExpectedID: rules.ID, Policy: rules}
	intercepted := &projectUpdateBeforeWriteRepository{Repository: repository}
	intercepted.before = func() error {
		return repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			current, err := w.AccessMember(t.Context(), member.UserID)
			if err != nil {
				return err
			}
			current.Role = "member"
			current.UpdatedAt = now.Add(time.Second)
			return w.UpdateAccessMember(t.Context(), current)
		})
	}
	f.service.config.TenantRepository = intercepted
	response := f.request(t, owner, http.MethodPut, f.base+"/change-review-policy", request)
	requireNativeStatus(t, response, http.StatusForbidden)
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		_, err := r.ReviewPolicy(t.Context(), f.project)
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Errorf("demoted admin review policy lookup error = %v, want not found", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestChangeVersionRecordPublishesImmutableSourceAndRevisionAtomically(t *testing.T) {
	repository, f, owner, admin := newChangePolicyPostgresFixtureWithAdmin(t, 3)
	service := f.service
	orgID, projectID := tracker.OrganizationID("org_security"), f.project
	response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-item"}, Title: "Version issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var item tracker.NativeIssue
	decodeHubResponse(t, response, &item)
	changesPath := f.base + "/work-items/" + string(item.WorkItemID) + "/changes"
	response = f.request(t, owner, http.MethodPost, changesPath, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "create-change"}, Title: "Version change", Body: "Source history"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	changePath := changesPath + "/" + change.ID
	policy := hubTestPolicy()
	reviewPolicy := defaultChangeReviewPolicy(policy)
	service.config.NativeRunEvents = &nativeRunEventParticipantFixture{}
	attemptID, runID, leaseID := newNativeID("attempt"), newNativeID("run"), newNativeID("lease")
	var lease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
		workItem, err := w.WorkItem(t.Context(), projectID, item.WorkItemID)
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: orgID, ID: "source-machine", Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: "source-machine", SessionID: "source-session", LeaseID: leaseID, TTL: time.Hour})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var member accessstore.Member
	var principal accessstore.Credential
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		member, err = r.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		principal, err = r.AccessCredential(t.Context(), member.PrincipalID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	scope := nativeScope{organization: orgID, project: projectID, credential: apiCredential{ID: principal.ID, Hash: principal.Hash, Scope: apiScopeOperator, Hosted: owner.identity.Hosted, HostedRole: "owner", HostedMembership: member.MembershipID, HostedPrincipal: member.PrincipalID, NativeOnly: true}}
	start := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: "source-run-start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{AttemptID: attemptID, RunID: runID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: policy.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "example"}},
	}
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(item.WorkItemID), start); err != nil {
		t.Fatal(err)
	}
	finish := start
	finish.IdempotencyKey = "source-run-finish"
	finish.Type = "run.finished"
	finish.Data.Sequence = 2
	finish.Data.Outcome = "succeeded"
	finish.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", LocalAttemptID: 42, Generation: 1, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 42, Generation: 1, SessionID: 42}}
	if _, err := service.appendNativeRunEventTenant(t.Context(), scope, string(item.WorkItemID), finish); err != nil {
		t.Fatal(err)
	}
	input := changeTestInput()
	input.AttemptID, input.RunID = start.Data.AttemptID, start.Data.RunID
	bundle := changeTestSource(t, &input)
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish-version"}, ExpectedVersionID: "", ChangeVersionInput: input, SourceBundle: bundle}
	service.config.TenantRepository = failingChangeEventRepository{Repository: repository}
	failed := f.request(t, owner, http.MethodPost, changePath+"/versions", request)
	requireNativeStatus(t, failed, http.StatusInternalServerError)
	service.config.TenantRepository = repository
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		versions, err := r.Versions(t.Context(), change.ID, 0, 10)
		if err != nil || len(versions) != 0 {
			t.Fatalf("failed publication left versions=%+v err=%v", versions, err)
		}
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.Revision != int64(change.Revision) || current.CurrentVersionID != nil {
			t.Fatalf("failed publication changed current Change: %+v", current)
		}
		if _, err := r.Receipt(t.Context(), principal.ID, "POST "+changePath+"/versions", request.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("failed publication receipt lookup error = %v, want not found", err)
		}
		events, err := r.Events(t.Context(), projectID, item.WorkItemID, 0, 100)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Type == "change.version_published" {
				t.Fatalf("failed publication retained event %+v", event)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.config.TenantRepository = committedChangeWriteResponseLossRepository{Repository: repository}
	uncertain := f.request(t, owner, http.MethodPost, changePath+"/versions", request)
	requireNativeStatus(t, uncertain, http.StatusInternalServerError)
	service.config.TenantRepository = repository
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		versions, err := r.Versions(t.Context(), change.ID, 0, 10)
		if err != nil || len(versions) != 1 {
			t.Fatalf("uncertain publication did not commit exactly once: versions=%+v err=%v", versions, err)
		}
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.CurrentVersionID == nil || *current.CurrentVersionID != versions[0].ID {
			t.Fatalf("uncertain publication did not move the current version: %+v", current)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	versionResponse := f.request(t, owner, http.MethodPost, changePath+"/versions", request)
	requireNativeStatus(t, versionResponse, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, versionResponse, &version)
	if version.Source == nil || version.Source.BundleSHA256 != input.Source.BundleSHA256 || version.Policy.ID != policy.ID || version.ReviewPolicy.ID != reviewPolicy.ID || version.Number != 1 {
		t.Fatalf("published version=%+v", version)
	}
	replay := f.request(t, owner, http.MethodPost, changePath+"/versions", request)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != versionResponse.Body.String() {
		t.Fatalf("version replay changed immutable payload: %s != %s", replay.Body.String(), versionResponse.Body.String())
	}
	changed := request
	changed.SourceBundle = append(bytes.Clone(bundle), 0)
	conflict := f.request(t, owner, http.MethodPost, changePath+"/versions", changed)
	requireNativeStatus(t, conflict, http.StatusConflict)
	sourceResponse := f.request(t, owner, http.MethodGet, changePath+"/versions/"+version.ID+"/source", nil)
	requireNativeStatus(t, sourceResponse, http.StatusOK)
	if !bytes.Equal(sourceResponse.Body.Bytes(), bundle) {
		t.Fatal("hydrated source differed from published immutable bundle")
	}
	detailResponse := f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, detailResponse, &detail)
	if detail.Change.CurrentVersion != version.ID || detail.Change.Revision != 2 || len(detail.Versions) != 1 || detail.Versions[0].Policy.ID != policy.ID {
		t.Fatalf("published change detail=%+v", detail)
	}
	approve := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-version"}, ExpectedRevision: detail.Change.Revision, ExpectedVersionID: version.ID, Decision: "approved", Body: "Reviewed current source"}
	reviewPath := changePath + "/versions/" + version.ID + "/reviews"
	originalDetails := service.config.RunDetails
	var writing bool
	guardDetails := &changeReviewDetailReadGuard{DetailStore: originalDetails, writing: &writing}
	lockRepository := &changeReviewLockRepository{Repository: repository, writing: &writing}
	service.config.RunDetails = guardDetails
	service.config.TenantRepository = lockRepository
	approved := f.request(t, owner, http.MethodPost, reviewPath, approve)
	service.config.RunDetails = originalDetails
	service.config.TenantRepository = repository
	requireNativeStatus(t, approved, http.StatusOK)
	if !lockRepository.locked {
		t.Fatal("approval appended Change evidence without locking its sequence")
	}
	if guardDetails.readInside {
		t.Fatal("approval hydrated archived policy content while its writer transaction was held")
	}
	var approval tracker.ChangeReview
	decodeHubResponse(t, approved, &approval)
	if approval.Decision != "approved" || approval.VersionID != version.ID || approval.Actor.PrincipalID == "" {
		t.Fatalf("approval=%+v", approval)
	}
	approvedReplay := f.request(t, owner, http.MethodPost, reviewPath, approve)
	requireNativeStatus(t, approvedReplay, http.StatusOK)
	if approvedReplay.Body.String() != approved.Body.String() {
		t.Fatal("approval receipt replay changed immutable review")
	}
	detailResponse = f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	decodeHubResponse(t, detailResponse, &detail)
	if detail.Summary.Status != "reviewed" || len(detail.Reviews) != 1 || detail.Reviews[0].ID != approval.ID {
		t.Fatalf("approved change detail=%+v", detail)
	}
	changesRequested := approve
	changesRequested.IdempotencyKey = "request-changes-version"
	changesRequested.Decision = "changes_requested"
	changesRequested.Body = "Address the source issue"
	requested := f.request(t, owner, http.MethodPost, reviewPath, changesRequested)
	requireNativeStatus(t, requested, http.StatusOK)
	requestedReplay := f.request(t, owner, http.MethodPost, reviewPath, changesRequested)
	requireNativeStatus(t, requestedReplay, http.StatusOK)
	if requestedReplay.Body.String() != requested.Body.String() {
		t.Fatal("changes-requested receipt replay changed immutable review")
	}
	detailResponse = f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	decodeHubResponse(t, detailResponse, &detail)
	if detail.Summary.NativeReview != "changes_requested" || len(detail.Reviews) != 2 {
		t.Fatalf("change request detail=%+v", detail)
	}
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		comments, err := r.Comments(t.Context(), projectID, item.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		if len(comments) != 1 || comments[0].BodyObjectKey == "" || comments[0].Sequence < 1 {
			t.Fatalf("review instructions comments=%+v", comments)
		}
		storedItem, err := r.WorkItem(t.Context(), projectID, item.WorkItemID)
		if err != nil {
			return err
		}
		if storedItem.Revision != 1 {
			t.Fatalf("review instructions changed item revision without a version: %+v", storedItem)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		versions, err := r.Versions(t.Context(), change.ID, 0, 10)
		if err != nil || len(versions) != 1 || versions[0].SourceObjectKey == "" {
			t.Fatalf("version rows=%+v err=%v", versions, err)
		}
		if versions[0].ImplementingSessionID != 42 || versions[0].ImplementingMachineID != "source-machine" {
			t.Fatalf("published source attribution = session %d machine %q", versions[0].ImplementingSessionID, versions[0].ImplementingMachineID)
		}
		events, err := r.Events(t.Context(), projectID, item.WorkItemID, 0, 10)
		if err != nil {
			return err
		}
		published := 0
		for _, event := range events {
			if event.Type == "change.version_published" && event.VersionID == version.ID {
				published++
			}
		}
		if published != 1 {
			t.Fatalf("published events=%d", published)
		}
		if len(events) == 0 || events[len(events)-1].Type != "comment.created" {
			t.Fatalf("last review instruction event=%+v", events)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var sqliteVersions int
	if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_versions").Scan(&sqliteVersions); err != nil {
		t.Fatal(err)
	}
	if sqliteVersions != 0 {
		t.Fatal("selected Change version wrote to SQLite")
	}
	nextInput := changeTestInput()
	nextInput.AttemptID, nextInput.RunID = start.Data.AttemptID, start.Data.RunID
	nextBundle := changeTestSource(t, &nextInput)
	nextRequest := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish-next-version"}, ExpectedVersionID: version.ID, ChangeVersionInput: nextInput, SourceBundle: nextBundle}
	nextResponse := f.request(t, owner, http.MethodPost, changePath+"/versions", nextRequest)
	requireNativeStatus(t, nextResponse, http.StatusOK)
	var nextVersion tracker.ChangeVersion
	decodeHubResponse(t, nextResponse, &nextVersion)
	if nextVersion.Number != 2 || nextVersion.ID == version.ID {
		t.Fatalf("next immutable version = %+v", nextVersion)
	}
	detailResponse = f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	decodeHubResponse(t, detailResponse, &detail)
	approveNext := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-next-version"}, ExpectedRevision: detail.Change.Revision, ExpectedVersionID: nextVersion.ID, Decision: "approved", Body: "Reviewed current source"}
	approveNextResponse := f.request(t, owner, http.MethodPost, changePath+"/versions/"+nextVersion.ID+"/reviews", approveNext)
	requireNativeStatus(t, approveNextResponse, http.StatusOK)
	mergeSHA := strings.Repeat("d", 40)
	landingRequest := tracker.LandChangeVersion{
		Mutation: tracker.Mutation{IdempotencyKey: "land-next-version"}, MergeSHA: mergeSHA, BaseRef: "main", Method: "squash",
		Receipt: &tracker.NativeLandingReceipt{Landed: true, ChangeID: change.ID, VersionID: nextVersion.ID, HeadSHA: nextVersion.HeadSHA, MergeSHA: mergeSHA, BaseRef: "main", BaseSHA: nextVersion.BaseSHA, Method: "squash", ObservedAt: service.config.now()},
	}
	landingPath := changePath + "/versions/" + nextVersion.ID + "/landing"
	var lateReview tracker.ReviewChange
	lateReview.IdempotencyKey = "review-after-landing-preparation"
	lateReview.ExpectedRevision = change.Revision + 2
	lateReview.ExpectedVersionID = nextVersion.ID
	lateReview.Decision = "approved"
	lateReview.Body = "Additional approval evidence"
	var lateReviewID string
	originalRunDetails := service.config.RunDetails
	raceStore := &changeArchiveMutationStore{DetailStore: originalRunDetails}
	raceStore.afterLandingPayload = func(ctx context.Context) error {
		lateReviewID = payloadNativeID(ctx, "review")
		review := tracker.ChangeReview{ID: lateReviewID, VersionID: nextVersion.ID, Decision: lateReview.Decision, Body: lateReview.Body, Actor: scope.actor(), CreatedAt: service.config.now()}
		raw, err := json.Marshal(review)
		if err != nil {
			return err
		}
		content := eventstore.NewContent(service.runDetailStore())
		evidence, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: orgID, ChangeID: change.ID, Kind: "review"}, raw)
		if err != nil {
			return err
		}
		return repository.WriteTenant(ctx, orgID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.AppendEvidence(ctx, evidence)
			return err
		})
	}
	service.config.RunDetails = raceStore
	staleLanding := landingRequest
	staleLanding.IdempotencyKey = "land-after-review-evidence-change"
	staleLandingResponse := f.request(t, owner, http.MethodPost, landingPath, staleLanding)
	service.config.RunDetails = originalRunDetails
	if !raceStore.fired {
		t.Fatal("landing payload preparation did not trigger the concurrent review evidence")
	}
	requireNativeStatus(t, staleLandingResponse, http.StatusConflict)
	service.config.TenantRepository = repository
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.Revision != int64(change.Revision+2) || current.CurrentVersionID == nil || *current.CurrentVersionID != nextVersion.ID {
			t.Fatalf("stale landing changed Change after new review evidence: %+v", current)
		}
		if _, err := r.LandingReceipt(t.Context(), string(attemptID)); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("stale landing retained receipt: %v", err)
		}
		if _, err := r.Receipt(t.Context(), principal.ID, "POST "+landingPath, staleLanding.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("stale landing retained command receipt: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	detailResponse = f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	decodeHubResponse(t, detailResponse, &detail)
	if detail.Summary.Status != "reviewed" || detail.Change.Landed != nil || !slices.ContainsFunc(detail.Reviews, func(review tracker.ChangeReview) bool { return review.ID == lateReviewID }) {
		t.Fatalf("late review or stale landing state = %+v", detail)
	}
	var evidenceSequence int64
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Change(t.Context(), change.ID)
		evidenceSequence = current.EvidenceSequence
		return err
	}); err != nil {
		t.Fatal(err)
	}
	concurrentReview := tracker.ChangeReview{ID: payloadNativeID(t.Context(), "review"), VersionID: nextVersion.ID, Decision: "approved", Body: "Concurrent approval evidence", Actor: scope.actor(), CreatedAt: service.config.now()}
	concurrentReviewRaw, err := json.Marshal(concurrentReview)
	if err != nil {
		t.Fatal(err)
	}
	concurrentEvidence, err := eventstore.NewContent(service.runDetailStore()).PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: orgID, ChangeID: change.ID, Kind: "review"}, concurrentReviewRaw)
	if err != nil {
		t.Fatal(err)
	}
	barrier := &changeLandingLockRepository{Repository: repository, locked: make(chan struct{}), prepared: make(chan struct{}), appendStart: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	releaseLanding := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	var landingJoined, appendStarted, appendJoined bool
	landingResult := make(chan int, 1)
	appendResult := make(chan error, 1)
	appendFinished := make(chan struct{})
	service.config.TenantRepository = barrier
	concurrentLanding := landingRequest
	concurrentLanding.IdempotencyKey = "land-concurrent-evidence-lock"
	go func() {
		landingResult <- f.request(t, owner, http.MethodPost, landingPath, concurrentLanding).Code
	}()
	t.Cleanup(func() {
		releaseLanding()
		if !landingJoined {
			select {
			case <-landingResult:
			case <-time.After(10 * time.Second):
				t.Error("concurrent landing request did not exit")
			}
		}
		if appendStarted && !appendJoined {
			select {
			case <-appendFinished:
			case <-time.After(10 * time.Second):
				t.Error("concurrent evidence append did not exit")
			}
		}
	})
	select {
	case <-barrier.prepared:
	case <-time.After(30 * time.Second):
		t.Fatal("landing did not reach the post-validation transaction point")
	}
	select {
	case <-barrier.locked:
	default:
		t.Fatal("landing did not lock the Change before validating review evidence")
	}
	appendStarted = true
	go func() {
		err := barrier.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.AppendEvidence(t.Context(), concurrentEvidence)
			return err
		})
		appendResult <- err
		close(appendFinished)
	}()
	select {
	case <-barrier.appendStart:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent evidence append did not enter its writer")
	}
	probe, err := admin.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.ExecContext(t.Context(), "SELECT set_config('detent.organization_id', $1, true)", string(orgID)); err != nil {
		_ = probe.Rollback()
		t.Fatal(err)
	}
	var probeID string
	probeErr := probe.QueryRowContext(t.Context(), "SELECT id FROM change_requests WHERE organization_id=$1 AND id=$2 FOR UPDATE NOWAIT", orgID, change.ID).Scan(&probeID)
	_ = probe.Rollback()
	var pgErr *pq.Error
	if !errors.As(probeErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("Change row was not locked during concurrent evidence append: %v", probeErr)
	}
	releaseLanding()
	landingStatus := <-landingResult
	landingJoined = true
	if landingStatus != http.StatusInternalServerError {
		t.Fatalf("injected concurrent landing status = %d, want %d", landingStatus, http.StatusInternalServerError)
	}
	if err := <-appendResult; err != nil {
		t.Fatalf("append review evidence after landing rollback: %v", err)
	}
	appendJoined = true
	service.config.TenantRepository = repository
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.Revision != int64(change.Revision+2) || current.EvidenceSequence != evidenceSequence+1 || current.CurrentVersionID == nil || *current.CurrentVersionID != nextVersion.ID {
			t.Fatalf("concurrent evidence/landing state = %+v", current)
		}
		if _, err := r.LandingReceipt(t.Context(), string(attemptID)); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("rolled-back concurrent landing receipt lookup = %v, want not found", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.config.TenantRepository = failingChangeEventRepository{Repository: repository}
	failedLanding := f.request(t, owner, http.MethodPost, landingPath, landingRequest)
	requireNativeStatus(t, failedLanding, http.StatusInternalServerError)
	service.config.TenantRepository = repository
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.LandingReceipt(t.Context(), string(attemptID)); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("rolled-back landing receipt lookup error = %v, want not found", err)
		}
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.Revision != int64(change.Revision+2) || current.CurrentVersionID == nil || *current.CurrentVersionID != nextVersion.ID {
			t.Fatalf("failed landing changed current Change: %+v", current)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	landedResponse := f.request(t, owner, http.MethodPost, landingPath, landingRequest)
	requireNativeStatus(t, landedResponse, http.StatusOK)
	landedReplay := f.request(t, owner, http.MethodPost, landingPath, landingRequest)
	requireNativeStatus(t, landedReplay, http.StatusOK)
	if landedReplay.Body.String() != landedResponse.Body.String() {
		t.Fatal("landing retry changed the archived response")
	}
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.LandingReceipt(t.Context(), string(attemptID))
		if err != nil {
			return err
		}
		receipt, err := eventstore.NewContent(service.runDetailStore()).LandingReceipt(t.Context(), stored)
		if err != nil {
			return err
		}
		if receipt.VersionID != nextVersion.ID || receipt.MergeSHA != mergeSHA || !receipt.Landed {
			t.Fatalf("stored landing receipt = %+v", receipt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	staleRequest := request
	staleRequest.IdempotencyKey = "publish-from-moved-version"
	stale := f.request(t, owner, http.MethodPost, changePath+"/versions", staleRequest)
	requireNativeStatus(t, stale, http.StatusConflict)
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		versions, err := r.Versions(t.Context(), change.ID, 0, 10)
		if err != nil || len(versions) != 2 {
			t.Fatalf("stale publication changed versions=%+v err=%v", versions, err)
		}
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.CurrentVersionID == nil || *current.CurrentVersionID != nextVersion.ID {
			t.Fatalf("stale publication moved current version: %+v", current)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
