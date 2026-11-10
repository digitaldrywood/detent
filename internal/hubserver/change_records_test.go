package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	hubapi "github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workflowmetrics"
)

type reviewExecutionHost struct {
	executioncontract.ExecutionHost
	client *hubapi.Client
}

func (h reviewExecutionHost) Transport() *hubapi.Client                                   { return h.client }
func (reviewExecutionHost) Now() time.Time                                                { return time.Now() }
func (reviewExecutionHost) LeaseDuration() time.Duration                                  { return time.Hour }
func (reviewExecutionHost) RemainingExecution(tracker.NativeLease) time.Duration          { return time.Hour }
func (reviewExecutionHost) ValidateExecutionPolicy(context.Context, string, string) error { return nil }
func (reviewExecutionHost) ProviderObservations() func() ([]providercapacity.Report, error) {
	return nil
}

type failingChangeEventRepository struct {
	tenantstore.Repository
}

type failingReviewPolicyRepository struct{ tenantstore.Repository }
type failingReviewPolicyWriter struct{ tenantstore.Writer }

func (r failingReviewPolicyRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error { return write(failingReviewPolicyWriter{w}) })
}

func (w failingReviewPolicyWriter) SetReviewPolicy(ctx context.Context, row eventstore.ReviewPolicy, expected string) error {
	if err := w.Writer.SetReviewPolicy(ctx, row, expected); err != nil {
		return err
	}
	return errors.New("injected review policy write failure")
}

type committedChangeWriteResponseLossRepository struct {
	tenantstore.Repository
}

type changeArchiveMutationStore struct {
	attachment.DetailStore
	reviewPolicyID      string
	afterReviewPayload  func(context.Context) error
	afterLandingPayload func(context.Context) error
	fired               bool
}

type failingChangeEventWriter struct {
	tenantstore.Writer
	runnersession.Reader
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
	runnersession.Reader
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
		return write(failingChangeEventWriter{Writer: w, Reader: w.(runnersession.Reader)})
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
	if s.afterReviewPayload != nil {
		var review tracker.ChangeReviewPolicy
		if json.Unmarshal(raw, &review) == nil && review.ID == s.reviewPolicyID {
			after := s.afterReviewPayload
			s.afterReviewPayload = nil
			if err := after(ctx); err != nil {
				return attachment.ArchiveReference{}, err
			}
		}
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

func (w failingChangeEventWriter) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	return w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, id)
}

func (r *changeLandingLockRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(changeLandingLockWriter{Writer: w, Reader: w.(runnersession.Reader), repository: r})
	})
}

func (w changeLandingLockWriter) LockChange(ctx context.Context, id string) (eventstore.Change, error) {
	change, err := w.Writer.LockChange(ctx, id)
	if err == nil {
		w.repository.lockedOnce.Do(func() { close(w.repository.locked) })
	}
	return change, err
}

func (w changeLandingLockWriter) LockLease(ctx context.Context, id string) (leasestore.Lease, error) {
	return w.Writer.(leasestore.FencedLeaseLocker).LockLease(ctx, id)
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

func TestChangeDetailEmptyCollectionsAreArrays(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openLocalPostgresHubTestService(t, repository, Config{RunDetails: &memoryRunDetails{}, SecretKeys: secretTestKeys(t, "1", "1")})
	for _, id := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(id), func(t *testing.T) {
			seedWorkItemRecordOrganization(t, repository, service, nativeOrganization{ID: id, Name: string(id), Slug: string(id)})
			projects := "/api/v2/organizations/" + string(id) + "/projects"
			response := performHubAPIRequest(t, service, http.MethodPost, projects, testHubAdminToken, map[string]any{"idempotency_key": "project", "name": "Example project", "states": nativeFixtureStates()})
			requireNativeStatus(t, response, http.StatusOK)
			var project tracker.NativeProject
			decodeHubResponse(t, response, &project)
			base := projects + "/" + string(project.ID)
			approveHubTestPolicy(t, service, base+"/policy", hubTestPolicy())
			response = performHubAPIRequest(t, service, http.MethodPost, base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example issue", Body: workItemRecordBody})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			path := base + "/work-items/" + string(issue.WorkItemID) + "/changes"
			response = performHubAPIRequest(t, service, http.MethodPost, path, testHubAdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "change"}, Title: "Example Change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			path += "/" + change.ID
			for _, published := range []bool{false, true} {
				if published {
					input := changeTestInput()
					input.Artifacts = nil
					response = performHubAPIRequest(t, service, http.MethodPost, path+"/versions", testHubAdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish"}, ChangeVersionInput: input})
					requireNativeStatus(t, response, http.StatusOK)
				}
				for _, view := range []string{"", "?view=bounded"} {
					response = performHubAPIRequest(t, service, http.MethodGet, path+view, testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var payload json.RawMessage
					decodeHubResponse(t, response, &payload)
					if view != "" {
						var envelope struct{ Detail json.RawMessage }
						if err := json.Unmarshal(payload, &envelope); err != nil {
							t.Fatal(err)
						}
						payload = envelope.Detail
					}
					var typed tracker.ChangeDetail
					if err := json.Unmarshal(payload, &typed); err != nil {
						t.Fatal(err)
					}
					if typed.Change.OrganizationID != id || len(typed.Change.LinkedIssues) != 1 || typed.Change.LinkedIssues[0] != issue.WorkItemID || typed.Summary.Messages == nil {
						t.Errorf("view=%q invalid Change identity or collection shape: %+v", view, typed)
					}
					var detail struct {
						Checks, Reviews, Discussion json.RawMessage
						Versions                    []struct {
							Artifacts, Checks json.RawMessage
							ReviewPolicy      struct {
								RequiredChecks json.RawMessage `json:"required_checks"`
							} `json:"review_policy"`
						}
					}
					if err := json.Unmarshal(payload, &detail); err != nil {
						t.Fatal(err)
					}
					for name, raw := range map[string]json.RawMessage{"checks": detail.Checks, "reviews": detail.Reviews, "discussion": detail.Discussion} {
						if string(raw) != "[]" {
							t.Errorf("published=%t %s=%s, want []", published, name, raw)
						}
					}
					if !published {
						if detail.Versions == nil || len(detail.Versions) != 0 {
							t.Errorf("draft versions=%+v, want []", detail.Versions)
						}
						continue
					}
					if len(detail.Versions) != 1 {
						t.Fatalf("versions=%d, want 1", len(detail.Versions))
					}
					version := detail.Versions[0]
					for name, raw := range map[string]json.RawMessage{"artifacts": version.Artifacts, "checks": version.Checks, "review_policy.required_checks": version.ReviewPolicy.RequiredChecks} {
						if string(raw) != "[]" {
							t.Errorf("version %s=%s, want []", name, raw)
						}
					}
				}
			}
			response = performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var published tracker.ChangeDetail
			decodeHubResponse(t, response, &published)
			for _, test := range []struct {
				name, version string
			}{
				{name: "whole Change"},
				{name: "current version", version: published.Change.CurrentVersion},
			} {
				t.Run("recovery discussion "+test.name, func(t *testing.T) {
					body := "Preserve exact feedback for " + string(id) + " " + test.name
					response := performHubAPIRequest(t, service, http.MethodPost, path+"/discussion", testHubAdminToken, tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "discuss-" + test.name}, VersionID: test.version, Body: body})
					requireNativeStatus(t, response, http.StatusOK)
					response = performHubAPIRequest(t, service, http.MethodGet, base+"/work-items/"+string(issue.WorkItemID)+"?view=recovery", testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var recovery tracker.NativeRecovery
					decodeHubResponse(t, response, &recovery)
					if recovery.ChangeDetail == nil || !slices.ContainsFunc(recovery.ChangeDetail.Discussion, func(discussion tracker.ChangeDiscussion) bool {
						return discussion.Body == body && discussion.VersionID == test.version
					}) {
						t.Fatalf("source recovery lost current Change feedback: %+v", recovery.ChangeDetail)
					}
					response = performHubAPIRequest(t, service, http.MethodGet, path, testHubAdminToken, nil)
					requireNativeStatus(t, response, http.StatusOK)
					var complete tracker.ChangeDetail
					decodeHubResponse(t, response, &complete)
					if !reflect.DeepEqual(recovery.ChangeDetail.Versions, complete.Versions) {
						t.Fatalf("recovery source version differs: expected=%+v actual=%+v", complete.Versions, recovery.ChangeDetail.Versions)
					}
				})
			}
		})
	}
}

func TestChangeDetailResponsePreservesStoredCollections(t *testing.T) {
	stored := tracker.ChangeDetail{Versions: []tracker.ChangeVersion{{ID: "version_example"}}, Checks: []tracker.ChangeCheck{{VersionID: "version_example"}}}
	raw, err := json.Marshal(changeDetailResponse(stored))
	if err != nil {
		t.Fatal(err)
	}
	var response tracker.ChangeDetail
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	version := response.Versions[0]
	for name, missing := range map[string]bool{"linked_issues": response.Change.LinkedIssues == nil, "messages": response.Summary.Messages == nil, "evidence": response.Checks[0].Evidence == nil, "artifacts": version.Artifacts == nil, "checks": version.Checks == nil, "required_checks": version.ReviewPolicy.RequiredChecks == nil} {
		if missing {
			t.Errorf("response %s is null, want []", name)
		}
	}
	if stored.Versions[0].Artifacts != nil || stored.Versions[0].Checks != nil || stored.Versions[0].ReviewPolicy.RequiredChecks != nil || stored.Checks[0].Evidence != nil {
		t.Fatal("formatting a response mutated stored collection values")
	}
	if version.ID != "version_example" || response.Checks[0].VersionID != "version_example" {
		t.Fatal("formatting a response dropped populated values")
	}
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
	next := descriptor
	next.ConfigDigest = policy.Digest([]byte("updated repository configuration"))
	next = next.WithID()
	response = f.request(t, owner, http.MethodPut, f.base+"/policy", policy.Change{ExpectedID: descriptor.ID, Policy: next})
	requireNativeStatus(t, response, http.StatusOK)
	response = f.request(t, owner, http.MethodGet, path, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var retained tracker.ChangeReviewPolicy
	decodeHubResponse(t, response, &retained)
	if retained.ID != defaultChangeReviewPolicy(next).ID || retained.PolicyID != next.ID || retained.RequireReview {
		t.Fatalf("default review rules did not follow repository approval: %+v", retained)
	}
	retained.RequireReview = true
	response = f.request(t, owner, http.MethodPut, path, tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rebind-review-policy"}, ExpectedID: retained.ID, Policy: retained})
	requireNativeStatus(t, response, http.StatusOK)
	var rebound tracker.ChangeReviewPolicy
	decodeHubResponse(t, response, &rebound)
	if rebound.PolicyID != next.ID || !rebound.RequireReview || rebound.ID == saved.ID {
		t.Fatalf("review policy did not bind current repository approval: %+v", rebound)
	}

}

func TestApprovedChangeReviewRejectsPolicyChangedAfterPreflight(t *testing.T) {
	repository, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 3)
	service := f.service
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
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
	input := changeTestInput()
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
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Background, func(tenantstore.Writer) error { return nil }); err != nil {
		t.Fatal(err)
	}
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
	repository, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 3)
	return repository, f, owner
}

func newChangePolicyPostgresFixtureWithAdmin(t *testing.T, maxConnections int) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	return newChangePolicyPostgresFixtureWithDetails(t, maxConnections, &memoryRunDetails{})
}

func newChangePolicyPostgresFixtureWithDetails(t *testing.T, maxConnections int, details *memoryRunDetails) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	repository, admin := projectRecordPostgresWithAdmin(t, maxConnections)
	f, owner := newChangePolicyPostgresTenantFixture(t, repository, "org_security", details)
	return repository, f, owner, admin
}

func newChangePolicyPostgresTenantFixture(t *testing.T, repository tenantstore.Repository, organization tracker.OrganizationID, details *memoryRunDetails) (hostedSecurityFixture, hostedSecurityUser) {
	t.Helper()
	projectID := tracker.ProjectID("prj_security")
	now := time.Now().UTC().Truncate(time.Microsecond)
	descriptor := hubTestPolicy()
	content := eventstore.NewContent(details)
	policyRevision, err := content.PrepareProjectPolicy(t.Context(), organization, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = projectID
	policyRevision.ApprovedBy = "example-owner"
	policyRevision.ApprovedAt = now.UTC().Truncate(time.Microsecond)
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "org_provider"}); err != nil {
			return err
		}
		project := projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: projectID, Name: "Example project", Slug: "example-project", Profile: "native"}, States: nativeFixtureStates()}
		if _, err := w.CreateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		if err := w.ApproveProjectPolicy(t.Context(), policyRevision, ""); err != nil {
			return err
		}
		free := normalizeHostedPlan(pilotHostedPlans().Plans[0])
		plan := billingstore.Plan{OrganizationID: organization, PlanReference: billingstore.PlanReference{ID: free.ID, Version: free.Version}, Name: free.Name, MonthlyUSDCents: free.MonthlyUSDCents, Allowances: free.Allowances, Features: free.Features}
		if err := w.CreateBillingPlan(t.Context(), plan); err != nil {
			return err
		}
		return w.SetBillingAssignment(t.Context(), billingstore.Assignment{OrganizationID: organization, Base: plan.PlanReference}, 0)
	}); err != nil {
		t.Fatal(err)
	}
	provider := newHostedSecurityProvider()
	plans := pilotHostedPlans()
	service := openTestService(t, Config{
		AccessRepository:  repository,
		BillingRepository: repository,
		TenantRepository:  repository,
		RunDetails:        details,
		Hosted: &HostedConfig{
			Plans:                &plans,
			OrganizationID:       string(organization),
			WorkOSOrganizationID: "org_provider",
			BootstrapSubject:     "user_owner",
			PublicURL:            "http://127.0.0.1:7777",
			StaffEmails:          []string{"staff@example.test", "support@example.test"},
			SupportActors:        []string{"support@example.test"},
			Provider:             provider,
		},
	})
	f := hostedSecurityFixture{service: service, provider: provider, project: projectID, base: "/api/v2/organizations/" + string(organization) + "/projects/" + string(projectID)}
	owner := f.user(t, "review_admin", "owner", "review-admin@example.test", "", "")
	if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), owner.identity.Subject)
		if err != nil {
			return err
		}
		if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: member.UserID, ProjectID: f.project, CanWrite: true}); err != nil {
			return err
		}
		return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: member.PrincipalID, ProjectID: f.project})
	}); err != nil {
		t.Fatal(err)
	}
	return f, owner
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
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	details := &memoryRunDetails{}
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		t.Run(string(organization), func(t *testing.T) {
			f, owner := newChangePolicyPostgresTenantFixture(t, repository, organization, details)
			if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				project, err := w.ProjectRecord(t.Context(), f.project)
				if err != nil {
					return err
				}
				states := make([]tracker.NativeState, 0, len(project.States)+1)
				for _, state := range project.States {
					state.Transitions = append(slices.Clone(state.Transitions), "Cancelled")
					states = append(states, state)
				}
				project.States = append(states, tracker.NativeState{Name: "Cancelled", Terminal: true, Transitions: []string{"Todo"}})
				_, err = w.UpdateProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			testChangeVersionRecordPublication(t, false, repository, f, owner, admin, details)
		})
	}
}

func TestChangeVersionRecordPublishesRunningSource(t *testing.T) {
	for _, mode := range []string{"current", "audit", "default approval", "default revert", "atomic rollback", "concurrent pin", "imported approval", "imported observation", "pinned"} {
		t.Run(mode, func(t *testing.T) {
			repository, admin := projectRecordPostgresWithAdmin(t, 4)
			details := &memoryRunDetails{}
			for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
				t.Run(string(organization), func(t *testing.T) {
					f, owner := newChangePolicyPostgresTenantFixture(t, repository, organization, details)
					testChangeVersionRecordPublication(t, true, repository, f, owner, admin, details, mode)
				})
			}
		})
	}
}

func testChangeVersionRecordPublication(t *testing.T, running bool, repository tenantstore.Repository, f hostedSecurityFixture, owner hostedSecurityUser, admin *sql.DB, details *memoryRunDetails, reviewModes ...string) {
	t.Helper()
	service := f.service
	originalPostgres, originalAccess, originalTenant := service.database.postgres, service.config.AccessRepository, service.config.TenantRepository
	service.database.postgres = repository.(*postgres.Repository)
	service.config.AccessRepository = repository
	service.config.TenantRepository = repository
	t.Cleanup(func() {
		service.database.postgres, service.config.AccessRepository, service.config.TenantRepository = originalPostgres, originalAccess, originalTenant
	})
	orgID, projectID := tracker.OrganizationID(service.config.Hosted.OrganizationID), f.project
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
	descriptor := hubTestPolicy()
	reviewPolicy := defaultChangeReviewPolicy(descriptor)
	reviewMode := "current"
	if len(reviewModes) > 0 {
		reviewMode = reviewModes[0]
	}
	if reviewMode == "audit" {
		previous := descriptor.ID
		descriptor.Gates.SecurityAudit = true
		descriptor = descriptor.WithID()
		requireNativeStatus(t, f.request(t, owner, http.MethodPut, f.base+"/policy", policy.Change{ExpectedID: previous, Policy: descriptor}), http.StatusOK)
		reviewPolicy = defaultChangeReviewPolicy(descriptor)
	}
	var originalReview eventstore.ReviewPolicy
	if reviewMode != "current" && reviewMode != "audit" {
		oldRules := reviewPolicy
		if reviewMode == "pinned" {
			var principal string
			if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), owner.identity.Subject)
				principal = member.PrincipalID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			oldRules.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "example-check", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: strings.Repeat("e", 64), Source: "independent", MaxAgeSeconds: 3600}}
			oldRules.ID = changerequest.PolicyID(oldRules)
		}
		updatedBy, updatedAt := "example-owner", service.config.now().UTC().Truncate(time.Microsecond)
		if strings.HasPrefix(reviewMode, "imported") {
			updatedBy, updatedAt = "production-import", time.Unix(0, 0).UTC()
		}
		var err error
		originalReview, err = eventstore.NewContent(service.runDetailStore()).PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: orgID, ProjectID: projectID, PolicyRevisionID: descriptor.ID, Revision: 1, UpdatedBy: updatedBy, UpdatedAt: updatedAt}, oldRules)
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error { return w.SetReviewPolicy(t.Context(), originalReview, "") }); err != nil {
			t.Fatal(err)
		}
		next := descriptor
		next.ConfigDigest = strings.Repeat("b", 64)
		next = next.WithID()
		if strings.HasPrefix(reviewMode, "imported") {
			row, err := eventstore.NewContent(service.runDetailStore()).PrepareProjectPolicy(t.Context(), orgID, next)
			if err != nil {
				t.Fatal(err)
			}
			row.ProjectID, row.ApprovedBy, row.ApprovedAt = projectID, "repository-default-branch", service.config.now().UTC()
			if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error { return w.ApproveProjectPolicy(t.Context(), row, descriptor.ID) }); err != nil {
				t.Fatal(err)
			}
		}
		originalDetails := service.config.RunDetails
		var reviewRace *changeArchiveMutationStore
		expectedReview := originalReview
		if reviewMode == "atomic rollback" {
			service.config.TenantRepository = failingReviewPolicyRepository{Repository: repository}
		}
		if reviewMode == "concurrent pin" {
			var principal string
			if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
				member, err := r.AccessMember(t.Context(), owner.identity.Subject)
				principal = member.PrincipalID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			pinned := oldRules
			pinned.RequiredChecks = []tracker.ChangeCheckSpec{{Name: "example-check", PrincipalID: principal, WorkflowID: "ci.yml", WorkflowSHA256: strings.Repeat("e", 64), Source: "independent", MaxAgeSeconds: 3600}}
			pinned.ID = changerequest.PolicyID(pinned)
			row, err := eventstore.NewContent(service.runDetailStore()).PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: orgID, ProjectID: projectID, PolicyRevisionID: descriptor.ID, Revision: 2, UpdatedBy: "example-owner", UpdatedAt: updatedAt}, pinned)
			if err != nil {
				t.Fatal(err)
			}
			expectedReview = row
			reviewRace = &changeArchiveMutationStore{DetailStore: originalDetails, reviewPolicyID: defaultChangeReviewPolicy(next).ID, afterReviewPayload: func(ctx context.Context) error {
				return repository.WriteTenant(ctx, orgID, corestore.Serving, func(w tenantstore.Writer) error { return w.SetReviewPolicy(ctx, row, originalReview.ReviewPolicyID) })
			}}
			service.config.RunDetails = reviewRace
		}
		if reviewMode != "imported observation" {
			approved := f.request(t, owner, http.MethodPut, f.base+"/policy", policy.Change{ExpectedID: descriptor.ID, Policy: next})
			service.config.TenantRepository = repository
			service.config.RunDetails = originalDetails
			if reviewRace != nil && reviewRace.afterReviewPayload != nil {
				t.Fatal("approval did not reach prepared review content")
			}
			if reviewMode == "atomic rollback" || reviewMode == "concurrent pin" {
				status := http.StatusInternalServerError
				if reviewMode == "concurrent pin" {
					status = http.StatusConflict
				}
				requireNativeStatus(t, approved, status)
				if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
					current, err := r.ProjectPolicy(t.Context(), projectID)
					if err != nil {
						return err
					}
					if current.PolicyID != descriptor.ID {
						t.Fatalf("failed approval changed repository policy: %+v", current)
					}
					review, err := r.ReviewPolicy(t.Context(), projectID)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(review, expectedReview) {
						t.Fatalf("failed approval changed review: got=%+v want=%+v", review, expectedReview)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var history int
				if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM change_review_policy_revisions WHERE organization_id=$1 AND project_id=$2 AND review_policy_id=$3`, orgID, projectID, defaultChangeReviewPolicy(next).ID).Scan(&history); err != nil {
					t.Fatal(err)
				}
				if history != 0 {
					t.Fatalf("failed approval retained default revision rows=%d", history)
				}
				return
			}
			requireNativeStatus(t, approved, http.StatusOK)
		}
		if reviewMode == "default revert" {
			reverted := f.request(t, owner, http.MethodPut, f.base+"/policy", policy.Change{ExpectedID: next.ID, Policy: descriptor})
			requireNativeStatus(t, reverted, http.StatusOK)
			next.ConfigDigest = strings.Repeat("c", 64)
			next = next.WithID()
			advanced := f.request(t, owner, http.MethodPut, f.base+"/policy", policy.Change{ExpectedID: descriptor.ID, Policy: next})
			requireNativeStatus(t, advanced, http.StatusOK)
		}
		descriptor, reviewPolicy = next, defaultChangeReviewPolicy(next)
	}
	service.config.NativeRunEvents = tenantNativeRunEventParticipant{service: service}
	attemptID, runID, leaseID := newNativeID("attempt"), newNativeID("run"), newNativeID("lease")
	var lease leasestore.Lease
	workerToken, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	binding := runnerauth.NewBinding()
	workerID, runnerID := newNativeID("credential"), binding.RunnerID
	workerHash := apikey.HashToken(workerToken)
	now := service.config.now()
	expires := now.Add(time.Hour)
	if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
		workItem, err := w.WorkItem(t.Context(), projectID, item.WorkItemID)
		if err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: orgID, ID: string(binding.MachineID), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: orgID, ID: workerID, Name: "Source runner", Hash: workerHash, Fingerprint: tokenFingerprint(workerHash), Scope: "worker", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: orgID, TokenID: workerID, ProjectID: projectID}); err != nil {
			return err
		}
		enrollmentID := newNativeID("enrollment")
		operations := []string{runnerauth.Read, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events, runnerauth.Collaborate}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: orgID, ID: enrollmentID, RunnerID: runnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken("example-source-enrollment"), Scope: "projects", ProjectIDs: []tracker.ProjectID{projectID}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-owner"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, runnerID, binding.MachineID, now); err != nil {
			return err
		}
		if err := w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: orgID, RunnerID: runnerID, MachineID: binding.MachineID, TokenID: workerID, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{projectID}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now}); err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: string(binding.MachineID), SessionID: "source-session", LeaseID: leaseID, TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: orgID, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
			return err
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: orgID, ProjectID: projectID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now})
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
	server := httptest.NewServer(service.Handler())
	t.Cleanup(server.Close)
	client, err := hubapi.New(hubapi.Config{URL: server.URL, TokenSource: func() string { return workerToken }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(orgID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	start := tracker.NativeRunEvent{
		Mutation: tracker.Mutation{IdempotencyKey: "source-run-start"}, Type: "run.started", SchemaVersion: 1,
		Data: tracker.NativeRunData{AttemptID: attemptID, RunID: runID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: descriptor.ID, Sequence: 1, Identity: &tracker.NativeExecutionIdentity{Role: "code", Backend: "codex", Model: "example"}},
	}
	if running {
		start.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", LocalAttemptID: 42, Generation: 1, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 42, Generation: 1, SessionID: 42}}
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", workerToken, start), http.StatusOK)
	if reviewMode == "audit" {
		ctx := hubapi.WithMutationAuthority(t.Context(), native.Scope(), tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, nil)
		sessions := hubapi.NewRunnerSessions(native)
		id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(item.WorkItemID), StartedAt: now, AgentRole: "code", Model: "example-model"})
		if err != nil {
			t.Fatalf("coding session setup: %v", err)
		}
		if err := sessions.FinishSession(ctx, id, store.SessionFinish{CompletedAt: now, FinalState: "completed"}); err != nil {
			t.Fatalf("coding session setup: %v", err)
		}
		start.Data.Runtime.Activity.SessionID = id
	}
	finish := start
	finish.IdempotencyKey = "source-run-finish"
	finish.Type = "run.finished"
	finish.Data.Sequence = 2
	finish.Data.Outcome = "succeeded"
	finish.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "implementation", LocalAttemptID: 42, Generation: 1, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 42, Generation: 1, SessionID: 42}}

	if !running {
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", workerToken, finish), http.StatusOK)
	}
	input := changeTestInput()
	input.AttemptID, input.RunID, input.PolicyID = start.Data.AttemptID, start.Data.RunID, descriptor.ID
	bundle := changeTestSource(t, &input)
	if reviewMode == "audit" {
		input.Validation = &gate.CommandResult{Command: "make check", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 1}
	}
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish-version", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, ExpectedVersionID: "", ChangeVersionInput: input, SourceBundle: bundle}
	if running {
		diff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: attemptID, RunnerID: runnerID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2}, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/attempts/"+attemptID+"/diff", workerToken, diff), http.StatusAccepted)
		for sequence := int64(2); sequence <= 3; sequence++ {
			observation := start
			observation.Type, observation.Data.Sequence = "run.observed", sequence
			observation.IdempotencyKey = fmt.Sprintf("source-run-progress-%d", sequence)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", workerToken, observation), http.StatusOK)
		}
	}
	if reviewMode != "current" && reviewMode != "audit" {
		if reviewMode == "imported observation" {
			observed := performHubAPIRequest(t, service, http.MethodPost, f.base+"/policy/observed", workerToken, policy.Observation{Descriptor: descriptor})
			requireNativeStatus(t, observed, http.StatusNoContent)
		}
		published := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, request)
		if reviewMode == "pinned" {
			requireNativeStatus(t, published, http.StatusConflict)
			var refusal struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			decodeHubResponse(t, published, &refusal)
			if refusal.Code != "policy_mismatch" || !strings.Contains(refusal.Message, "read the current policy and retry") {
				t.Fatalf("stale pinned policy refusal=%q", refusal.Code)
			}
		} else {
			requireNativeStatus(t, published, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, published, &version)
			if version.ReviewPolicy.ID != reviewPolicy.ID || version.Policy.ID != descriptor.ID {
				t.Fatalf("published policy=%+v review=%+v", version.Policy, version.ReviewPolicy)
			}
			if reviewMode == "imported observation" {
				repeated := performHubAPIRequest(t, service, http.MethodPost, f.base+"/policy/observed", workerToken, policy.Observation{Descriptor: descriptor})
				requireNativeStatus(t, repeated, http.StatusNoContent)
			} else {
				repeated := f.request(t, owner, http.MethodPut, f.base+"/policy", policy.Change{ExpectedID: descriptor.ID, Policy: descriptor})
				requireNativeStatus(t, repeated, http.StatusOK)
			}
		}
		if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
			current, err := r.ReviewPolicy(t.Context(), projectID)
			if err != nil {
				return err
			}
			versions, err := r.Versions(t.Context(), change.ID, 0, 10)
			if err != nil {
				return err
			}
			expectedVersions := 1
			if reviewMode == "pinned" {
				expectedVersions = 0
			}
			if len(versions) != expectedVersions {
				t.Fatalf("published version rows=%d want=%d", len(versions), expectedVersions)
			}
			if reviewMode == "pinned" {
				if !reflect.DeepEqual(current, originalReview) {
					t.Fatalf("pinned review changed: got=%+v want=%+v", current, originalReview)
				}
			} else {
				expectedRevision := int64(2)
				if reviewMode == "default revert" {
					expectedRevision = 4
				}
				if current.Revision != expectedRevision || current.PolicyRevisionID != descriptor.ID || current.ReviewPolicyID != reviewPolicy.ID || current.UpdatedBy == "production-import" {
					t.Fatalf("default review did not advance: %+v", current)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var history int
		if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM change_review_policy_revisions WHERE organization_id=$1 AND project_id=$2 AND review_policy_id=$3 AND policy_id=$4 AND revision=1 AND updated_by=$5 AND updated_at=$6`, orgID, projectID, originalReview.ReviewPolicyID, originalReview.PolicyRevisionID, originalReview.UpdatedBy, originalReview.UpdatedAt).Scan(&history); err != nil {
			t.Fatal(err)
		}
		if history != 1 {
			t.Fatalf("original immutable review rows=%d, want 1", history)
		}
		if reviewMode == "pinned" {
			rules, err := eventstore.NewContent(service.runDetailStore()).ReviewPolicy(t.Context(), originalReview)
			if err != nil {
				t.Fatal(err)
			}
			rules.PolicyID = descriptor.ID
			rebound := f.request(t, owner, http.MethodPut, f.base+"/change-review-policy", tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "rebind-pinned"}, ExpectedID: originalReview.ReviewPolicyID, Policy: rules})
			requireNativeStatus(t, rebound, http.StatusOK)
			accepted := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, request)
			requireNativeStatus(t, accepted, http.StatusOK)
			var version tracker.ChangeVersion
			decodeHubResponse(t, accepted, &version)
			if version.PolicyID != descriptor.ID || version.ReviewPolicy.ID == originalReview.ReviewPolicyID || !reflect.DeepEqual(version.ReviewPolicy.RequiredChecks, rules.RequiredChecks) {
				t.Fatalf("rebound pinned publication changed review checks: %+v", version.ReviewPolicy)
			}
		}
		return
	}
	service.config.TenantRepository = failingChangeEventRepository{Repository: repository}
	failed := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, request)
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
		if _, err := r.Receipt(t.Context(), workerID, "POST "+changePath+"/versions", request.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
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
	uncertain := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, request)
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
	versionResponse := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, request)
	requireNativeStatus(t, versionResponse, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, versionResponse, &version)
	if running && reviewMode != "audit" {
		finish.Data.Sequence = 4
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", workerToken, finish), http.StatusOK)
	}
	if version.Source == nil || version.Source.BundleSHA256 != input.Source.BundleSHA256 || version.Policy.ID != descriptor.ID || version.ReviewPolicy.ID != reviewPolicy.ID || version.Number != 1 {
		t.Fatalf("published version=%+v", version)
	}
	replay, err := native.PublishChangeVersion(t.Context(), item.WorkItemID, change.ID, request)
	if err != nil || !reflect.DeepEqual(replay, version) {
		t.Fatalf("runner version replay changed immutable payload: got=%+v want=%+v error=%v", replay, version, err)
	}
	foreignOrganization := tracker.OrganizationID("org_first")
	if orgID == foreignOrganization {
		foreignOrganization = "org_second"
	}
	foreign, err := client.Native(foreignOrganization, projectID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = foreign.PublishChangeVersion(t.Context(), item.WorkItemID, change.ID, request)
	var refusal *hubapi.APIError
	if !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound {
		t.Fatalf("foreign runner publication error=%v, want not found", err)
	}
	changed := request
	changed.SourceBundle = append(bytes.Clone(bundle), 0)
	conflict := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, changed)
	requireNativeStatus(t, conflict, http.StatusConflict)
	sourceResponse := f.request(t, owner, http.MethodGet, changePath+"/versions/"+version.ID+"/source", nil)
	requireNativeStatus(t, sourceResponse, http.StatusOK)
	if !bytes.Equal(sourceResponse.Body.Bytes(), bundle) {
		t.Fatal("hydrated source differed from published immutable bundle")
	}
	if running && reviewMode == "audit" {
		ctx := hubapi.WithMutationAuthority(t.Context(), native.Scope(), tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, nil)
		sessions := hubapi.NewRunnerSessions(native)
		id, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(item.WorkItemID), StartedAt: now, AgentRole: "security_audit", Model: "example-model"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sessions.FinishSession(ctx, id, store.SessionFinish{CompletedAt: now, FinalState: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.RecordUsageEvent(ctx, store.UsageEvent{IssueID: string(item.WorkItemID), SessionID: id, StartedAt: now, FinishedAt: now, Outcome: "completed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.RecordWorkflowPhaseEvent(ctx, store.WorkflowPhaseEvent{IssueID: string(item.WorkItemID), SessionID: id, StartedAt: now, FinishedAt: now, PhaseType: store.WorkflowPhaseTypeAgentSession, PhaseName: "agent_active", Status: "completed"}); err != nil {
			t.Fatal(err)
		}
		observation := start
		observation.Type, observation.Data.Sequence, observation.IdempotencyKey = "run.observed", 4, "audit-runtime"
		observation.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "validation", LocalAttemptID: 42, Generation: 1, Activity: &workflowmetrics.ActivityProfile{Schema: 1, AttemptID: 42, Generation: 1, SessionID: id}}
		requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", workerToken, observation), http.StatusOK)
		audit := tracker.SecurityAuditReview{VersionID: version.ID, SessionID: id, DiffDigest: version.Source.DiffSHA256, Verdict: "pass", Run: securityaudit.Run{ID: id, InvocationID: "review-invocation", ProjectID: string(projectID), IssueID: string(item.WorkItemID), Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, ReviewerVersion: securityaudit.ReviewerVersion, ReviewerDigest: securityaudit.ReviewerDigest(), AuthenticationMode: "api_key", ExitStatus: securityaudit.ExitStatusSuccess, WorkerPID: 1, WorkerPGID: 1, WorkerStartedAt: now, StartedAt: now, CompletedAt: now, OutputBytes: 1, OutputDigest: securityaudit.OutputDigest("result"), Verdict: "pass", Summary: "No findings"}}
		connector, err := hubapi.NewNativeConnector(native)
		if err != nil {
			t.Fatal(err)
		}
		activeLease := tracker.NativeLease{ID: tracker.LeaseID(lease.LeaseID), WorkItemID: item.WorkItemID, FencingToken: tracker.FencingToken(lease.FencingToken), PolicyID: descriptor.ID}
		execution, err := nativechange.NewRecoveredExecution(reviewExecutionHost{client: client}, ctx, executioncontract.ExecutionClaim{Source: connector, Lease: activeLease}, checkpoint.State{Data: observation.Data, Diff: &tracker.AttemptDiffRequest{BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}, PreparedFinalization: &tracker.NativeFinalization{Changed: true, ChangeID: change.ID, VersionID: version.ID, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA}})
		if err != nil {
			t.Fatal(err)
		}
		wrongRole, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(item.WorkItemID), StartedAt: now, AgentRole: "code", Model: "example-model"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sessions.FinishSession(ctx, wrongRole, store.SessionFinish{CompletedAt: now, FinalState: "completed"}); err != nil {
			t.Fatal(err)
		}
		incomplete, err := sessions.StartSession(ctx, store.SessionStart{IssueID: string(item.WorkItemID), StartedAt: now, AgentRole: "security_audit", Model: "example-model"})
		if err != nil {
			t.Fatal(err)
		}

		err = execution.(runner.NativeSecurityAuditExecution).RecordSecurityAudit(ctx, audit)
		if err != nil {
			var apiError *hubapi.APIError
			if errors.As(err, &apiError) {
				t.Fatalf("running-source audit publication: status=%d code=%s rejection=%+v message=%s", apiError.Status, apiError.Code, apiError.Rejection, apiError.Message)
			}
			t.Fatal(err)
		}
		if err := execution.(runner.NativeSecurityAuditExecution).RecordSecurityAudit(ctx, audit); err != nil {
			t.Fatal(err)
		}
		detail, err := native.RuntimeChange(ctx, item.WorkItemID, change.ID)
		if err != nil || len(detail.Reviews) != 1 || detail.Summary.Status != "reviewed" {
			t.Fatalf("audit publication or replay lost its evidence: reviews=%d status=%s error=%v", len(detail.Reviews), detail.Summary.Status, err)
		}
		for _, test := range []struct {
			name string
			edit func(*tracker.SecurityAuditReview)
		}{
			{name: "coding session", edit: func(a *tracker.SecurityAuditReview) {
				a.SessionID, a.Run.ID = start.Data.Runtime.Activity.SessionID, start.Data.Runtime.Activity.SessionID
			}},
			{name: "wrong session role", edit: func(a *tracker.SecurityAuditReview) { a.SessionID, a.Run.ID = wrongRole, wrongRole }},
			{name: "unfinished audit session", edit: func(a *tracker.SecurityAuditReview) { a.SessionID, a.Run.ID = incomplete, incomplete }},
			{name: "different repository", edit: func(a *tracker.SecurityAuditReview) {
				a.Run.Repository = "https://github.com/example-org/other-project"
			}},
			{name: "different digest", edit: func(a *tracker.SecurityAuditReview) { a.DiffDigest = strings.Repeat("0", 64) }},
			{name: "stale reviewer", edit: func(a *tracker.SecurityAuditReview) { a.Run.ReviewerDigest = strings.Repeat("0", 64) }},
			{name: "foreign project", edit: func(a *tracker.SecurityAuditReview) { a.Run.ProjectID = "prj_other" }},
		} {
			t.Run(test.name, func(t *testing.T) {
				invalid := audit
				test.edit(&invalid)
				invalid.Run.InvocationID = "refused-" + test.name
				if err := execution.(runner.NativeSecurityAuditExecution).RecordSecurityAudit(ctx, invalid); err == nil {
					t.Fatal("invalid audit acquired review authority")
				}
			})
		}
		foreignRequest := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "foreign-audit", LeaseID: activeLease.ID, FencingToken: activeLease.FencingToken}, ExpectedVersionID: version.ID, Decision: "approved", SecurityAudit: &audit}
		_, err = foreign.ReviewChange(ctx, item.WorkItemID, change.ID, version.ID, foreignRequest)
		if !errors.As(err, &refusal) || refusal.Status != http.StatusNotFound {
			t.Fatalf("foreign audit error=%v, want not found", err)
		}
		foreignRequest.IdempotencyKey = "stale-audit-fence"
		foreignRequest.FencingToken++
		_, err = native.ReviewChange(ctx, item.WorkItemID, change.ID, version.ID, foreignRequest)
		if !errors.As(err, &refusal) || refusal.Code != "stale_fencing_token" {
			t.Fatalf("stale audit fence error=%v", err)
		}
		detail, err = native.RuntimeChange(ctx, item.WorkItemID, change.ID)
		if err != nil || len(detail.Reviews) != 1 {
			t.Fatalf("refused audits changed review evidence: reviews=%d error=%v", len(detail.Reviews), err)
		}
	}
	if running {
		return
	}
	detailResponse := f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, detailResponse, &detail)
	if detail.Change.CurrentVersion != version.ID || detail.Change.Revision != 2 || len(detail.Versions) != 1 || detail.Versions[0].Policy.ID != descriptor.ID {
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
		if versions[0].ImplementingSessionID != 42 || versions[0].ImplementingMachineID != string(binding.MachineID) {
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
	nextInput := changeTestInput()
	nextInput.AttemptID, nextInput.RunID = start.Data.AttemptID, start.Data.RunID
	nextBundle := changeTestSource(t, &nextInput)
	nextRequest := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish-next-version", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, ExpectedVersionID: version.ID, ChangeVersionInput: nextInput, SourceBundle: nextBundle}
	nextResponse := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, nextRequest)
	requireNativeStatus(t, nextResponse, http.StatusOK)
	var nextVersion tracker.ChangeVersion
	decodeHubResponse(t, nextResponse, &nextVersion)
	if nextVersion.Number != 2 || nextVersion.ID == version.ID {
		t.Fatalf("next immutable version = %+v", nextVersion)
	}
	detailResponse = f.request(t, owner, http.MethodGet, changePath, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	decodeHubResponse(t, detailResponse, &detail)
	if err := service.populateChangeColumns(t.Context()); err != nil {
		t.Fatal(err)
	}
	approveNext := tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: "approve-next-version"}, ExpectedRevision: detail.Change.Revision, ExpectedVersionID: nextVersion.ID, Decision: "approved", Body: "Reviewed current source"}
	approveNextResponse := f.request(t, owner, http.MethodPost, changePath+"/versions/"+nextVersion.ID+"/reviews", approveNext)
	requireNativeStatus(t, approveNextResponse, http.StatusOK)
	if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
		if _, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "completed"); err != nil {
			return err
		}
		workItem, err := w.WorkItem(t.Context(), projectID, item.WorkItemID)
		if err != nil {
			return err
		}
		lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: string(binding.MachineID), SessionID: "landing-session", LeaseID: newNativeID("lease"), TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: orgID, LeaseID: lease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
			return err
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: orgID, ProjectID: projectID, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: descriptor.ID, BoundAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	landingStart := start
	landingStart.IdempotencyKey = "landing-run-start"
	landingStart.Data.AttemptID, landingStart.Data.RunID = newNativeID("attempt"), newNativeID("run")
	landingStart.Data.LeaseID, landingStart.Data.FencingToken = tracker.LeaseID(lease.LeaseID), tracker.FencingToken(lease.FencingToken)
	landingStart.Data.Identity = &tracker.NativeExecutionIdentity{Role: "merge", Backend: "codex", Model: "example"}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/events", workerToken, landingStart), http.StatusOK)
	mergeSHA := strings.Repeat("d", 40)
	landingRequest := tracker.LandChangeVersion{
		Mutation: tracker.Mutation{IdempotencyKey: "land-next-version", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, MergeSHA: mergeSHA, BaseRef: "main", Method: "squash",
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
	staleLandingResponse := performHubAPIRequest(t, service, http.MethodPost, landingPath, workerToken, staleLanding)
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
		if _, err := r.LandingReceipt(t.Context(), landingStart.Data.AttemptID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("stale landing retained receipt: %v", err)
		}
		if _, err := r.Receipt(t.Context(), workerID, "POST "+landingPath, staleLanding.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
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
	landingResult := make(chan *httptest.ResponseRecorder, 1)
	appendResult := make(chan error, 1)
	appendFinished := make(chan struct{})
	service.config.TenantRepository = barrier
	concurrentLanding := landingRequest
	concurrentLanding.IdempotencyKey = "land-concurrent-evidence-lock"
	go func() {
		landingResult <- performHubAPIRequest(t, service, http.MethodPost, landingPath, workerToken, concurrentLanding)
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
	case result := <-landingResult:
		landingJoined = true
		t.Fatalf("landing exited before the post-validation transaction: status=%d body=%s", result.Code, result.Body.String())
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
	var pgErr *pgconn.PgError
	if !errors.As(probeErr, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("Change row was not locked during concurrent evidence append: %v", probeErr)
	}
	releaseLanding()
	landingResponse := <-landingResult
	landingJoined = true
	if landingResponse.Code != http.StatusInternalServerError {
		t.Fatalf("injected concurrent landing status = %d, want %d", landingResponse.Code, http.StatusInternalServerError)
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
		if _, err := r.LandingReceipt(t.Context(), landingStart.Data.AttemptID); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("rolled-back concurrent landing receipt lookup = %v, want not found", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service.config.TenantRepository = failingChangeEventRepository{Repository: repository}
	failedLanding := performHubAPIRequest(t, service, http.MethodPost, landingPath, workerToken, landingRequest)
	requireNativeStatus(t, failedLanding, http.StatusInternalServerError)
	service.config.TenantRepository = repository
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		if _, err := r.LandingReceipt(t.Context(), landingStart.Data.AttemptID); !errors.Is(err, corestore.ErrNotFound) {
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
	eventsPath := f.base + "/work-items/" + string(item.WorkItemID) + "/events"
	landingEvent := func(key string, sequence int64, landing tracker.NativeLandingReceipt) tracker.NativeRunEvent {
		event := landingStart
		event.Type, event.IdempotencyKey, event.Data.Sequence = "run.observed", key, sequence
		event.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "merging", HeartbeatAt: service.config.now().UTC(), Landing: &landing}
		return event
	}
	requireStep := func(operation, key string, sequence int64, response *httptest.ResponseRecorder) {
		t.Helper()
		if response.Code != http.StatusOK {
			var refusal struct{ Code string }
			_ = json.Unmarshal(response.Body.Bytes(), &refusal)
			t.Fatalf("%s %s at sequence %d: status = %d, want 200, code=%s", operation, key, sequence, response.Code, refusal.Code)
		}
	}
	landedClaim := tracker.NativeLandingReceipt{Landed: true, ChangeID: change.ID, VersionID: nextVersion.ID, HeadSHA: nextVersion.HeadSHA, MergeSHA: mergeSHA, BaseRef: "main", Method: "squash"}
	foreignItemResponse := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-foreign-item"}, Title: "Foreign version issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, foreignItemResponse, http.StatusOK)
	var foreignItem tracker.NativeIssue
	decodeHubResponse(t, foreignItemResponse, &foreignItem)
	foreignChangeResponse := f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(foreignItem.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "create-foreign-change"}, Title: "Foreign change", Body: "Other source history"})
	requireNativeStatus(t, foreignChangeResponse, http.StatusOK)
	var foreignChange tracker.ChangeRequest
	decodeHubResponse(t, foreignChangeResponse, &foreignChange)
	waitingClaim := tracker.NativeLandingReceipt{Waiting: true, ChangeID: change.ID, VersionID: nextVersion.ID, HeadSHA: nextVersion.HeadSHA}
	for _, test := range []struct {
		name   string
		status int
		claim  tracker.NativeLandingReceipt
		edit   func(*tracker.NativeLandingReceipt)
	}{
		{"unknown Change", http.StatusNotFound, landedClaim, func(l *tracker.NativeLandingReceipt) { l.ChangeID = newNativeID("change") }},
		{"Change of another work item", http.StatusNotFound, landedClaim, func(l *tracker.NativeLandingReceipt) { l.ChangeID = foreignChange.ID }},
		{"unknown version", http.StatusNotFound, landedClaim, func(l *tracker.NativeLandingReceipt) { l.VersionID = newNativeID("version") }},
		{"waiting on another head", http.StatusUnprocessableEntity, waitingClaim, func(l *tracker.NativeLandingReceipt) { l.HeadSHA = strings.Repeat("e", 40) }},
		{"landed before the landing owner recorded it", http.StatusUnprocessableEntity, landedClaim, func(*tracker.NativeLandingReceipt) {}},
	} {
		claim := test.claim
		test.edit(&claim)
		if response := performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, landingEvent("unrecorded "+test.name, 2, claim)); response.Code != test.status {
			t.Fatalf("%s before landing: status=%d body=%s", test.name, response.Code, response.Body.String())
		}
	}
	requireStep("run event", "landing-waiting", 2, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, landingEvent("landing-waiting", 2, waitingClaim)))
	landedResponse := performHubAPIRequest(t, service, http.MethodPost, landingPath, workerToken, landingRequest)
	requireNativeStatus(t, landedResponse, http.StatusOK)
	landedReplay := performHubAPIRequest(t, service, http.MethodPost, landingPath, workerToken, landingRequest)
	requireNativeStatus(t, landedReplay, http.StatusOK)
	if landedReplay.Body.String() != landedResponse.Body.String() {
		t.Fatal("landing retry changed the archived response")
	}
	var originalReceipt eventstore.LandingReceipt
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		originalReceipt, err = r.LandingReceipt(t.Context(), landingStart.Data.AttemptID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if originalReceipt.VersionID != nextVersion.ID || originalReceipt.MergeSHA != mergeSHA {
		t.Fatalf("original landing receipt = %+v", originalReceipt)
	}
	for _, test := range []struct {
		name string
		edit func(*tracker.NativeLandingReceipt)
	}{
		{"method", func(l *tracker.NativeLandingReceipt) { l.Method = "merge" }},
		{"merge commit", func(l *tracker.NativeLandingReceipt) { l.MergeSHA = strings.Repeat("e", 40) }},
		{"base ref", func(l *tracker.NativeLandingReceipt) { l.BaseRef = "develop" }},
		{"version", func(l *tracker.NativeLandingReceipt) { l.VersionID, l.HeadSHA = version.ID, version.HeadSHA }},
	} {
		claim := landedClaim
		test.edit(&claim)
		if response := performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, landingEvent("recorded other "+test.name, 3, claim)); response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("landed claim with another %s: status=%d body=%s", test.name, response.Code, response.Body.String())
		}
	}
	for sequence := int64(3); sequence <= 4; sequence++ {
		requireStep("run event", fmt.Sprintf("landing-recorded-%d", sequence), sequence, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, landingEvent(fmt.Sprintf("landing-recorded-%d", sequence), sequence, landedClaim)))
	}
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		attempt, err := r.Attempt(t.Context(), landingStart.Data.AttemptID)
		if err == nil && attempt.Sequence != 4 {
			t.Fatalf("refused landing claims consumed the attempt sequence: %+v", attempt)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	landingRows := func() (rows [5]int64) {
		t.Helper()
		if err := admin.QueryRowContext(t.Context(), `SELECT
 (SELECT count(*) FROM collaboration_events WHERE organization_id=$1),
 (SELECT count(*) FROM native_attempt_events WHERE organization_id=$1 AND attempt_id=$2),
 (SELECT count(*) FROM runner_attempt_token_usage WHERE organization_id=$1 AND attempt_id=$2),
 (SELECT count(*) FROM cost_sources WHERE organization_id=$1),
 (SELECT sequence FROM native_attempts WHERE organization_id=$1 AND id=$2)`, string(orgID), landingStart.Data.AttemptID).Scan(&rows[0], &rows[1], &rows[2], &rows[3], &rows[4]); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	settled := landingRows()
	requireUnchanged := func(name string) {
		t.Helper()
		if rows := landingRows(); rows != settled {
			t.Fatalf("%s changed history, attempt receipts, token usage, costs or sequence: before=%v after=%v", name, settled, rows)
		}
	}
	otherMethod := landedClaim
	otherMethod.Method = "merge"
	validPrefix := landingEvent("landing-batch-prefix", 5, landedClaim)
	refusedBatch := landingEvent("landing-batch-other-method", 6, otherMethod)
	refusedBatch.PriorEvents = []tracker.NativeRunEvent{validPrefix}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, refusedBatch), http.StatusUnprocessableEntity)
	requireUnchanged("valid landed prefix with another method suffix")
	var canonical eventstore.Change
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		canonical, err = r.Change(t.Context(), change.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	details.mu.Lock()
	canonicalPayload := details.data[canonical.PayloadObjectKey]
	delete(details.data, canonical.PayloadObjectKey)
	details.mu.Unlock()
	if canonicalPayload == nil {
		t.Fatal("canonical Change payload is not in the fixture detail store")
	}
	missingResponse := performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, landingEvent("landing-missing-payload", 5, landedClaim))
	var missingFailure struct{ Code string }
	requireNativeStatus(t, missingResponse, http.StatusInternalServerError)
	decodeHubResponse(t, missingResponse, &missingFailure)
	if missingFailure.Code != "native_operation_failed" {
		t.Fatalf("landed claim without its canonical Change payload = %s", missingResponse.Body.String())
	}
	details.mu.Lock()
	details.data[canonical.PayloadObjectKey] = canonicalPayload
	details.mu.Unlock()
	requireUnchanged("missing canonical Change payload")
	moved := false
	details.afterStore = func(attachment.ArchiveReference) {
		if moved {
			return
		}
		moved = true
		content := eventstore.NewContent(details)
		current, err := content.Change(t.Context(), canonical)
		if err != nil {
			t.Error(err)
			return
		}
		current.Revision++
		current.UpdatedAt = current.UpdatedAt.Add(time.Microsecond)
		raw, err := json.Marshal(current)
		if err != nil {
			t.Error(err)
			return
		}
		record, err := content.PrepareChange(t.Context(), orgID, raw)
		if err != nil {
			t.Error(err)
			return
		}
		record.Revision = canonical.Revision
		if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.UpdateChange(t.Context(), record)
			return err
		}); err != nil {
			t.Error(err)
		}
	}
	movedClaim := landingEvent("landing-moved-payload", 5, landedClaim)
	movedResponse := performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, movedClaim)
	details.afterStore = nil
	if !moved {
		t.Fatal("canonical Change payload did not move during the landed claim")
	}
	var unavailable struct{ Code string }
	requireNativeStatus(t, movedResponse, http.StatusServiceUnavailable)
	decodeHubResponse(t, movedResponse, &unavailable)
	if unavailable.Code != "service_unavailable" {
		t.Fatalf("moved canonical Change payload refusal = %s", movedResponse.Body.String())
	}
	requireUnchanged("canonical Change payload moved after its proof was read")
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.Revision != canonical.Revision+1 || current.PayloadSHA256 == canonical.PayloadSHA256 || current.LandedMergeSHA == nil || *current.LandedMergeSHA != mergeSHA {
			t.Fatalf("canonical Change payload did not move with its landing intact: before=%+v after=%+v", canonical, current)
		}
		if _, err := r.Receipt(t.Context(), workerID, "POST "+eventsPath, movedClaim.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("refused landed claim retained a command receipt: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireStep("run event", movedClaim.IdempotencyKey, movedClaim.Data.Sequence, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, movedClaim))
	var echoes []tracker.NativeRunEvent
	for sequence := int64(6); sequence < 13; sequence++ {
		echoes = append(echoes, landingEvent(fmt.Sprintf("landing-full-batch-%d", sequence), sequence, landedClaim))
	}
	fullBatch := landingEvent("landing-full-batch", 13, landedClaim)
	fullBatch.PriorEvents = echoes
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		canonical, err = r.Change(t.Context(), change.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	canonicalReads := func(reset map[string]int) int {
		details.mu.Lock()
		defer details.mu.Unlock()
		count := details.keyReads[canonical.PayloadObjectKey]
		details.keyReads = reset
		return count
	}
	service.workerCancel()
	<-service.workerDone
	<-service.secretRewrapDone
	t.Cleanup(func() {
		details.mu.Lock()
		details.keyReads = nil
		details.mu.Unlock()
	})
	canonicalReads(map[string]int{})
	requireStep("run event batch", fullBatch.IdempotencyKey, fullBatch.Data.Sequence, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, fullBatch))
	if reads := canonicalReads(map[string]int{}); reads != 1 {
		t.Fatalf("eight landed echoes read the canonical Change payload %d times", reads)
	}
	settled = landingRows()
	if settled[4] != 13 {
		t.Fatalf("eight landed echoes left the attempt at sequence %d", settled[4])
	}
	echoes = nil
	for sequence := int64(14); sequence < 22; sequence++ {
		echoes = append(echoes, landingEvent(fmt.Sprintf("landing-overfull-batch-%d", sequence), sequence, landedClaim))
	}
	overfullBatch := landingEvent("landing-overfull-batch", 22, landedClaim)
	overfullBatch.PriorEvents = echoes
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, overfullBatch), http.StatusUnprocessableEntity)
	if reads := canonicalReads(nil); reads != 0 {
		t.Fatalf("nine landed echoes read the canonical Change payload %d times before refusal", reads)
	}
	requireUnchanged("nine landed echoes in one request")
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.LandingReceipt(t.Context(), landingStart.Data.AttemptID)
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
	stale := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, staleRequest)
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
	laterInput := changeTestInput()
	laterInput.AttemptID, laterInput.RunID = landingStart.Data.AttemptID, landingStart.Data.RunID
	laterBundle := changeTestSource(t, &laterInput)
	laterRequest := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish-after-landing", LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, ExpectedVersionID: nextVersion.ID, ChangeVersionInput: laterInput, SourceBundle: laterBundle}
	laterDiff := tracker.AttemptDiffRequest{Producer: tracker.DiffProducer{Kind: tracker.DiffSourceAttempt, ID: landingStart.Data.AttemptID, RunnerID: runnerID, LeaseID: tracker.LeaseID(lease.LeaseID), FencingToken: tracker.FencingToken(lease.FencingToken)}, Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 1}, BaseSHA: laterInput.BaseSHA, HeadSHA: laterInput.HeadSHA}
	if response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/attempts/"+landingStart.Data.AttemptID+"/diff", workerToken, laterDiff); response.Code != http.StatusAccepted {
		var refusal struct{ Code string }
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		t.Fatalf("attempt diff for the landing attempt: status = %d, want 202, code=%s", response.Code, refusal.Code)
	}
	laterResponse := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", workerToken, laterRequest)
	requireStep("publish version", laterRequest.IdempotencyKey, 0, laterResponse)
	var laterVersion tracker.ChangeVersion
	decodeHubResponse(t, laterResponse, &laterVersion)
	if laterVersion.ID == nextVersion.ID {
		t.Fatalf("later immutable version = %+v", laterVersion)
	}
	laterEcho := landingEvent("landing-after-later-version", 14, landedClaim)
	requireStep("run event", laterEcho.IdempotencyKey, laterEcho.Data.Sequence, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, laterEcho))
	if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Change(t.Context(), change.ID)
		if err != nil {
			return err
		}
		if current.CurrentVersionID == nil || *current.CurrentVersionID != laterVersion.ID || current.LandedVersionID == nil || *current.LandedVersionID != nextVersion.ID || current.LandedHeadSHA == nil || *current.LandedHeadSHA != nextVersion.HeadSHA || current.LandedMergeSHA == nil || *current.LandedMergeSHA != mergeSHA || current.LandedBaseRef == nil || *current.LandedBaseRef != "main" {
			t.Fatalf("later version changed the recorded landing: %+v", current)
		}
		stored, err := r.LandingReceipt(t.Context(), landingStart.Data.AttemptID)
		if err != nil {
			return err
		}
		if stored.VersionID != nextVersion.ID || stored.MergeSHA != mergeSHA || stored != originalReceipt {
			t.Fatalf("later version changed the landing receipt: before=%+v after=%+v", originalReceipt, stored)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	requireLanding := func(name, attemptID string, sequence int64) {
		t.Helper()
		if err := repository.ReadTenant(t.Context(), orgID, corestore.Serving, func(r tenantstore.Reader) error {
			attempt, err := r.Attempt(t.Context(), attemptID)
			if err != nil {
				return err
			}
			if attempt.Status != "cancelled" || attempt.Sequence != sequence {
				t.Fatalf("%s: attempt status=%s sequence=%d, want cancelled at %d", name, attempt.Status, attempt.Sequence, sequence)
			}
			current, err := r.Change(t.Context(), change.ID)
			if err != nil {
				return err
			}
			if current.CurrentVersionID == nil || *current.CurrentVersionID != laterVersion.ID || current.LandedVersionID == nil || *current.LandedVersionID != nextVersion.ID || current.LandedHeadSHA == nil || *current.LandedHeadSHA != nextVersion.HeadSHA || current.LandedMergeSHA == nil || *current.LandedMergeSHA != mergeSHA || current.LandedBaseRef == nil || *current.LandedBaseRef != "main" {
				t.Fatalf("%s changed the recorded landing or current version: %+v", name, current)
			}
			stored, err := r.LandingReceipt(t.Context(), landingStart.Data.AttemptID)
			if err != nil {
				return err
			}
			if stored.VersionID != nextVersion.ID || stored.MergeSHA != mergeSHA || stored != originalReceipt {
				t.Fatalf("%s changed the original landing receipt: before=%+v after=%+v", name, originalReceipt, stored)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/leases/"+lease.LeaseID+"/release", workerToken, tracker.NativeLeaseMutation{FencingToken: tracker.FencingToken(lease.FencingToken), Reason: "cancelled", CancelAttempt: true}); response.Code != http.StatusNoContent {
		var refusal struct{ Code string }
		_ = json.Unmarshal(response.Body.Bytes(), &refusal)
		t.Fatalf("release lease landing lease after landed echo at sequence 15: status = %d, want 204, code=%s", response.Code, refusal.Code)
	}
	requireLanding("worker release after landed echo", landingStart.Data.AttemptID, 15)
	var cancelLease leasestore.Lease
	var cancelRevision tracker.Revision
	if err := repository.WriteTenant(t.Context(), orgID, corestore.Serving, func(w tenantstore.Writer) error {
		workItem, err := w.WorkItem(t.Context(), projectID, item.WorkItemID)
		if err != nil {
			return err
		}
		cancelRevision = tracker.Revision(workItem.Revision)
		cancelLease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: workItem.ID, ExpectedRevision: workItem.Revision, MachineID: string(binding.MachineID), SessionID: "cancel-session", LeaseID: newNativeID("lease"), TTL: time.Hour})
		if err != nil {
			return err
		}
		if err := w.AssignRunnerLease(t.Context(), runnerstore.LeaseAssignment{OrganizationID: orgID, LeaseID: cancelLease.LeaseID, RunnerID: runnerID, AssignedAt: now}); err != nil {
			return err
		}
		return w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: orgID, ProjectID: projectID, LeaseID: tracker.LeaseID(cancelLease.LeaseID), PolicyID: descriptor.ID, BoundAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	cancelStart := landingStart
	cancelStart.IdempotencyKey = "cancel-run-start"
	cancelStart.Data.AttemptID, cancelStart.Data.RunID = newNativeID("attempt"), newNativeID("run")
	cancelStart.Data.LeaseID, cancelStart.Data.FencingToken = tracker.LeaseID(cancelLease.LeaseID), tracker.FencingToken(cancelLease.FencingToken)
	requireStep("run event", cancelStart.IdempotencyKey, 1, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, cancelStart))
	cancelEcho := cancelStart
	cancelEcho.Type, cancelEcho.IdempotencyKey, cancelEcho.Data.Sequence = "run.observed", "cancel-run-landed-echo", 2
	cancelEcho.Data.Runtime = &tracker.NativeRuntimeObservation{Phase: "merging", HeartbeatAt: service.config.now().UTC(), Landing: &landedClaim}
	requireStep("run event", cancelEcho.IdempotencyKey, 2, performHubAPIRequest(t, service, http.MethodPost, eventsPath, workerToken, cancelEcho))
	requireStep("workflow transition", "landed-echo-operator-cancel", 0, f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(item.WorkItemID)+"/workflow", tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "landed-echo-operator-cancel", LeaseID: tracker.LeaseID(cancelLease.LeaseID), FencingToken: tracker.FencingToken(cancelLease.FencingToken)}, ExpectedRevision: cancelRevision, State: "Cancelled", Reason: "user_requested"}))
	requireLanding("operator cancel after landed echo", cancelStart.Data.AttemptID, 3)
}
