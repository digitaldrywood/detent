package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeVersionAuthorityFixture struct {
	approval     ChangeVersionApproval
	prepareErr   error
	authorizeErr error
}

type failingChangeEventRepository struct {
	tenantstore.Repository
}

type failingChangeEventWriter struct {
	tenantstore.Writer
}

func (r failingChangeEventRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(failingChangeEventWriter{Writer: w})
	})
}

func (w failingChangeEventWriter) AppendEvent(context.Context, eventstore.Event) (eventstore.Event, error) {
	return eventstore.Event{}, errors.New("injected event write failure")
}

func (a changeVersionAuthorityFixture) Prepare(context.Context, nativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, time.Time) (ChangeVersionApproval, error) {
	return a.approval, a.prepareErr
}

func (a changeVersionAuthorityFixture) Authorize(context.Context, tenantstore.Writer, nativeScope, tracker.ChangeRequest, tracker.PublishChangeVersion, ChangeVersionApproval, time.Time) error {
	return a.authorizeErr
}

func TestChangeRecordBackendParity(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	for _, name := range []string{"First organization", "Second organization"} {
		t.Run(name, func(t *testing.T) {
			org, err := service.createNativeOrganizationFor(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			projectPath := "/api/v2/organizations/" + string(org.ID) + "/projects"
			response := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "create-project", "name": "Example project", "states": nativeFixtureStates()})
			requireNativeStatus(t, response, http.StatusOK)
			var project tracker.NativeProject
			decodeHubResponse(t, response, &project)
			itemsPath := projectPath + "/" + string(project.ID) + "/work-items"
			createItem := func(key string) tracker.NativeIssue {
				t.Helper()
				response := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example issue " + key, Body: workItemRecordBody, State: "Todo"})
				requireNativeStatus(t, response, http.StatusOK)
				var issue tracker.NativeIssue
				decodeHubResponse(t, response, &issue)
				return issue
			}
			primary, linked := createItem("primary"), createItem("linked")
			changePath := "/api/v2/organizations/" + string(org.ID) + "/projects/" + string(project.ID) + "/work-items/" + string(primary.WorkItemID) + "/changes"
			request := tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "create-change"}, Title: "Example Change", Body: "Immutable discussion", LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}}
			created := performHubAPIRequest(t, service, http.MethodPost, changePath, testHubAdminToken, request)
			requireNativeStatus(t, created, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, created, &change)
			if change.OrganizationID != org.ID || change.ProjectID != project.ID || change.WorkItemID != primary.WorkItemID || change.Body != request.Body || len(change.LinkedIssues) != 2 {
				t.Fatalf("created change=%+v", change)
			}
			replay := performHubAPIRequest(t, service, http.MethodPost, changePath, testHubAdminToken, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != created.Body.String() {
				t.Fatalf("create replay changed response: %s vs %s", replay.Body.String(), created.Body.String())
			}
			discussion := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: "discussion"}, Body: "Append-only discussion"}
			discussionPath := changePath + "/" + change.ID + "/discussion"
			posted := performHubAPIRequest(t, service, http.MethodPost, discussionPath, testHubAdminToken, discussion)
			requireNativeStatus(t, posted, http.StatusOK)
			discussionReplay := performHubAPIRequest(t, service, http.MethodPost, discussionPath, testHubAdminToken, discussion)
			requireNativeStatus(t, discussionReplay, http.StatusOK)
			if discussionReplay.Body.String() != posted.Body.String() {
				t.Fatal("discussion replay changed the archived response")
			}
			list := performHubAPIRequest(t, service, http.MethodGet, "/api/v2/organizations/"+string(org.ID)+"/projects/"+string(project.ID)+"/work-items/"+string(linked.WorkItemID)+"/changes", testHubAdminToken, nil)
			requireNativeStatus(t, list, http.StatusOK)
			var changes []tracker.ChangeRequest
			decodeHubResponse(t, list, &changes)
			if len(changes) != 1 || changes[0].ID != change.ID || len(changes[0].LinkedIssues) != 2 {
				t.Fatalf("linked issue changes=%+v", changes)
			}
			detailResponse := performHubAPIRequest(t, service, http.MethodGet, changePath+"/"+change.ID, testHubAdminToken, nil)
			requireNativeStatus(t, detailResponse, http.StatusOK)
			var detail tracker.ChangeDetail
			decodeHubResponse(t, detailResponse, &detail)
			if detail.Change.ID != change.ID || detail.Change.Body != request.Body || len(detail.Discussion) != 1 || detail.Discussion[0].Body != discussion.Body {
				t.Fatalf("change detail=%+v", detail.Change)
			}
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, changePath+"/"+change.ID+"/versions", testHubAdminToken, nil), http.StatusServiceUnavailable)
			requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, changePath+"/"+change.ID+"/versions/version_example/source", testHubAdminToken, nil), http.StatusNotFound)
			var localCount int
			if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_requests WHERE id=?", change.ID).Scan(&localCount); err != nil {
				t.Fatal(err)
			}
			if localCount != 0 {
				t.Fatal("selected change route wrote to SQLite")
			}
			if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
				links, err := r.Links(t.Context(), change.ID, "", 10)
				if err != nil {
					return err
				}
				if len(links) != 2 {
					t.Fatalf("stored links=%+v", links)
				}
				events, err := r.Events(t.Context(), project.ID, primary.WorkItemID, 0, 10)
				if err != nil {
					return err
				}
				found := false
				for _, event := range events {
					found = found || event.Type == "change.created"
				}
				if !found {
					t.Fatal("change creation event was not committed with the change")
				}
				events, err = r.Events(t.Context(), project.ID, primary.WorkItemID, 0, 10)
				if err != nil {
					return err
				}
				found = false
				for _, event := range events {
					found = found || event.Type == "change.discussion"
				}
				if !found {
					t.Fatal("discussion event was not committed with its evidence and receipt")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChangeRecordReceiptRejectsChangedRequest(t *testing.T) {
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	org, err := service.createNativeOrganizationFor(t.Context(), "Receipt organization")
	if err != nil {
		t.Fatal(err)
	}
	projectPath := "/api/v2/organizations/" + string(org.ID) + "/projects"
	response := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "create-project", "name": "Receipt project", "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	itemsPath := projectPath + "/" + string(project.ID) + "/work-items"
	itemResponse := performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "issue"}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, itemResponse, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, itemResponse, &issue)
	changePath := "/api/v2/organizations/" + string(org.ID) + "/projects/" + string(project.ID) + "/work-items/" + string(issue.WorkItemID) + "/changes"
	first := tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "same-key"}, Title: "Original title", Body: "Example body"}
	response = performHubAPIRequest(t, service, http.MethodPost, changePath, testHubAdminToken, first)
	requireNativeStatus(t, response, http.StatusOK)
	changed := first
	changed.Title = "Changed title"
	response = performHubAPIRequest(t, service, http.MethodPost, changePath, testHubAdminToken, changed)
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

func newChangePolicyPostgresFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser) {
	t.Helper()
	repository := projectRecordPostgres(t)
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
	return repository, f, owner
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
	repository := projectRecordPostgres(t)
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), RunDetails: &memoryRunDetails{}, TenantRepository: repository})
	org, err := service.createNativeOrganizationFor(t.Context(), "Version organization")
	if err != nil {
		t.Fatal(err)
	}
	projectPath := "/api/v2/organizations/" + string(org.ID) + "/projects"
	response := performHubAPIRequest(t, service, http.MethodPost, projectPath, testHubAdminToken, map[string]any{"idempotency_key": "create-project", "name": "Version project", "states": nativeFixtureStates()})
	requireNativeStatus(t, response, http.StatusOK)
	var project tracker.NativeProject
	decodeHubResponse(t, response, &project)
	itemsPath := projectPath + "/" + string(project.ID) + "/work-items"
	response = performHubAPIRequest(t, service, http.MethodPost, itemsPath, testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-item"}, Title: "Version issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, response, http.StatusOK)
	var item tracker.NativeIssue
	decodeHubResponse(t, response, &item)
	changesPath := itemsPath + "/" + string(item.WorkItemID) + "/changes"
	response = performHubAPIRequest(t, service, http.MethodPost, changesPath, testHubAdminToken, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "create-change"}, Title: "Version change", Body: "Source history"})
	requireNativeStatus(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, response, &change)
	changePath := changesPath + "/" + change.ID
	policy := hubTestPolicy()
	policyContent := eventstore.NewContent(service.runDetailStore())
	policyRevision, err := policyContent.PrepareProjectPolicy(t.Context(), org.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	policyRevision.ProjectID = project.ID
	policyRevision.ApprovedBy = "example-owner"
	policyRevision.ApprovedAt = service.config.now().UTC().Truncate(time.Microsecond)
	if err := repository.WriteTenant(t.Context(), org.ID, corestore.Serving, func(w tenantstore.Writer) error {
		return w.ApproveProjectPolicy(t.Context(), policyRevision, "")
	}); err != nil {
		t.Fatal(err)
	}
	reviewPolicy := tracker.ChangeReviewPolicy{PolicyID: policy.ID, RequireReview: true}
	reviewPolicy.ID = changerequest.PolicyID(reviewPolicy)
	service.config.ChangeVersionAuthority = changeVersionAuthorityFixture{approval: ChangeVersionApproval{Policy: policy, ReviewPolicy: reviewPolicy}}
	input := changeTestInput()
	input.RunID = artifact.NewID("run")
	input.AttemptID = artifact.NewID("attempt")
	now := service.config.now()
	input.Validation = &gate.CommandResult{HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("c", 40), ExitCode: 0, DurationNS: 1, Command: "go test ./...", EvidenceArtifact: &artifact.Reference{SchemaVersion: 1, Scope: artifact.Scope{OrganizationID: string(org.ID), ProjectID: string(project.ID), WorkItemID: string(item.WorkItemID), RunID: input.RunID, AttemptID: input.AttemptID}, ServiceID: artifact.NewID("service"), ArtifactID: artifact.NewID("artifact"), ManifestID: artifact.NewID("manifest"), Revision: 1, SHA256: strings.Repeat("d", 64), Kind: "log", State: "complete", Availability: "available", Bytes: 1, Objects: 1, ExpiresAt: now.Add(time.Hour), ObservedAt: now}}
	for _, test := range []struct {
		name   string
		mutate func(*artifact.Reference)
	}{
		{name: "wrong organization", mutate: func(ref *artifact.Reference) { ref.OrganizationID = artifact.NewID("org") }},
		{name: "wrong attempt", mutate: func(ref *artifact.Reference) { ref.AttemptID = artifact.NewID("attempt") }},
	} {
		bad := input
		validation := *input.Validation
		bad.Validation = &validation
		ref := *input.Validation.EvidenceArtifact
		test.mutate(&ref)
		bad.Validation.EvidenceArtifact = &ref
		response := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", testHubAdminToken, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "invalid-evidence-" + strings.ReplaceAll(test.name, " ", "-")}, ExpectedVersionID: "", ChangeVersionInput: bad})
		requireNativeStatus(t, response, http.StatusUnprocessableEntity)
	}
	bundle := changeTestSource(t, &input)
	input.Validation.HeadSHA = input.HeadSHA
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "publish-version"}, ExpectedVersionID: "", ChangeVersionInput: input, SourceBundle: bundle}
	service.config.TenantRepository = failingChangeEventRepository{Repository: repository}
	failed := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", testHubAdminToken, request)
	requireNativeStatus(t, failed, http.StatusInternalServerError)
	service.config.TenantRepository = repository
	if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
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
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	versionResponse := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", testHubAdminToken, request)
	requireNativeStatus(t, versionResponse, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, versionResponse, &version)
	if version.Source == nil || version.Source.BundleSHA256 != input.Source.BundleSHA256 || version.Policy.ID != policy.ID || version.ReviewPolicy.ID != reviewPolicy.ID || version.Number != 1 {
		t.Fatalf("published version=%+v", version)
	}
	replay := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", testHubAdminToken, request)
	requireNativeStatus(t, replay, http.StatusOK)
	if replay.Body.String() != versionResponse.Body.String() {
		t.Fatalf("version replay changed immutable payload: %s != %s", replay.Body.String(), versionResponse.Body.String())
	}
	changed := request
	changed.SourceBundle = append(bytes.Clone(bundle), 0)
	conflict := performHubAPIRequest(t, service, http.MethodPost, changePath+"/versions", testHubAdminToken, changed)
	requireNativeStatus(t, conflict, http.StatusConflict)
	sourceResponse := performHubAPIRequest(t, service, http.MethodGet, changePath+"/versions/"+version.ID+"/source", testHubAdminToken, nil)
	requireNativeStatus(t, sourceResponse, http.StatusOK)
	if !bytes.Equal(sourceResponse.Body.Bytes(), bundle) {
		t.Fatal("hydrated source differed from published immutable bundle")
	}
	detailResponse := performHubAPIRequest(t, service, http.MethodGet, changePath, testHubAdminToken, nil)
	requireNativeStatus(t, detailResponse, http.StatusOK)
	var detail tracker.ChangeDetail
	decodeHubResponse(t, detailResponse, &detail)
	if detail.Change.CurrentVersion != version.ID || detail.Change.Revision != 2 || len(detail.Versions) != 1 || detail.Versions[0].Policy.ID != policy.ID {
		t.Fatalf("published change detail=%+v", detail)
	}
	if err := repository.ReadTenant(t.Context(), org.ID, corestore.Serving, func(r tenantstore.Reader) error {
		versions, err := r.Versions(t.Context(), change.ID, 0, 10)
		if err != nil || len(versions) != 1 || versions[0].SourceObjectKey == "" {
			t.Fatalf("version rows=%+v err=%v", versions, err)
		}
		events, err := r.Events(t.Context(), project.ID, item.WorkItemID, 0, 10)
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
}
