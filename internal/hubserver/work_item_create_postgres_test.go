package hubserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/labstack/echo/v4"
)

func TestPostgresCookieIssueLifecycle(t *testing.T) {
	repository, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 3)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project)
		if err != nil {
			return err
		}
		project.States = nativeTriageStates(append(humanWorkflowFixtureStates(), tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo", "Cancelled"}}))
		project.Settings.DuplicateChallenge = true
		_, err = w.UpdateProjectRecord(t.Context(), project)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	configureHumanWorkflowProject(t, f.service, "org_security", f.project)
	originalDB, originalReader, originalPostgres := f.service.database.db, f.service.database.reader, f.service.database.postgres
	f.service.database.db, f.service.database.reader = nil, nil
	f.service.database.postgres = repository.(*postgres.Repository)
	t.Cleanup(func() {
		f.service.database.db, f.service.database.reader, f.service.database.postgres = originalDB, originalReader, originalPostgres
	})
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		state, err := r.(searchstore.DuplicateSourceReader).DuplicateSourceState(t.Context(), f.project)
		if err == nil && !state.Ready {
			t.Fatal("cookie lifecycle must start with a ready duplicate index")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	items := f.base + "/work-items"
	create := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "cookie-lifecycle-create"}, Title: "Example cookie lifecycle", Body: workItemRecordBody, State: "Backlog", Type: "bug"}
	created := f.request(t, owner, http.MethodPost, items, create)
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	if issue.WorkItemID == "" || issue.State != "Backlog" || issue.Body != create.Body || issue.Actor.Kind != "human" {
		t.Fatal("cookie creation lost issue identity, body, state or actor")
	}
	replayed := f.request(t, owner, http.MethodPost, items, create)
	requireNativeStatus(t, replayed, http.StatusOK)
	if !bytes.Equal(created.Body.Bytes(), replayed.Body.Bytes()) {
		t.Fatal("cookie create replay changed the canonical response")
	}
	itemPath := items + "/" + string(issue.WorkItemID)
	read := f.request(t, owner, http.MethodGet, itemPath, nil)
	requireNativeStatus(t, read, http.StatusOK)
	var opened tracker.NativeIssue
	decodeHubResponse(t, read, &opened)
	if opened.WorkItemID != issue.WorkItemID || opened.Body != create.Body || opened.State != "Backlog" {
		t.Fatal("opening the cookie-created issue lost content or state")
	}
	comment := f.request(t, owner, http.MethodPost, itemPath+"/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "cookie-lifecycle-comment"}, Body: "Example lifecycle observation"})
	requireNativeStatus(t, comment, http.StatusOK)
	var observation tracker.NativeComment
	decodeHubResponse(t, comment, &observation)
	if observation.WorkItemID != issue.WorkItemID || observation.Body != "Example lifecycle observation" {
		t.Fatal("cookie comment lost its issue or body")
	}
	for index, target := range []string{"Todo", "Cancelled", "Todo"} {
		previous := issue
		transition := tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("cookie-lifecycle-transition-%d", index)}, ExpectedRevision: previous.Revision, State: target, Reason: "user_requested"}
		response := f.request(t, owner, http.MethodPost, itemPath+"/workflow", transition)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &issue)
		if issue.State != target || issue.Revision != previous.Revision+1 || issue.Body != create.Body || issue.WorkItemID != previous.WorkItemID {
			t.Fatalf("transition to %s lost canonical state, revision, body or identity", target)
		}
		replay := f.request(t, owner, http.MethodPost, itemPath+"/workflow", transition)
		requireNativeStatus(t, replay, http.StatusOK)
		if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
			t.Fatalf("transition to %s did not replay canonically", target)
		}
		read = f.request(t, owner, http.MethodGet, itemPath, nil)
		requireNativeStatus(t, read, http.StatusOK)
		decodeHubResponse(t, read, &opened)
		if opened.State != target || opened.Revision != issue.Revision {
			t.Fatalf("opening after %s returned stale state", target)
		}
	}
	patch := f.request(t, owner, http.MethodPatch, itemPath, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "cookie-unsupported-edit"}, ExpectedRevision: issue.Revision, Title: new("Example unsupported edit")})
	requireNativeStatus(t, patch, http.StatusServiceUnavailable)
}

func TestPostgresConfiguredCreateAndDuplicateChallenge(t *testing.T) {
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	if serving, _ := repository.WorkloadCapacities(); serving < 2 {
		t.Fatal("concurrent confirmation fixture requires two serving transactions")
	}
	now := time.Date(2026, 11, 4, 3, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_create_first", "org_create_second"} {
		t.Run(string(org), func(t *testing.T) {
			seedStartupOrganization(t, repository, org)
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',123) ON CONFLICT(organization_id,name) DO UPDATE SET value=excluded.value`, org); err != nil {
				t.Fatal(err)
			}
			accounting := func() [2]int64 {
				t.Helper()
				var values [2]int64
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					values[0], err = r.CollaborationBytes(t.Context())
					if err != nil {
						return err
					}
					values[1], err = r.ProjectEventSequence(t.Context(), tracker.ProjectID("project_"+string(org)))
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return values
			}
			details := &memoryRunDetails{}
			plans := pilotHostedPlans()
			provider := newHostedSecurityProvider()
			user := "example-owner-" + string(org)
			membership := auth.Membership{ID: "membership-" + string(org), UserID: user, OrganizationID: "provider_" + string(org), Status: "active"}
			membership.Role.Slug = "owner"
			provider.members[membership.ID] = membership
			service := &Service{echo: echo.New(), secretRewrapReady: make(chan struct{}), database: &database{postgres: repository, billingRepository: repository, billingAuditStore: details, hostedOrganization: org, now: func() time.Time { return now }, logger: discardLogger()}, config: Config{Hosted: &HostedConfig{OrganizationID: string(org), WorkOSOrganizationID: "provider_" + string(org), Provider: provider, PublicURL: "http://example.test", Plans: &plans}, Logger: discardLogger(), TenantRepository: repository, AccessRepository: repository, RunDetails: details, now: func() time.Time { return now }}}
			service.ready.Store(true)
			service.registerNativeRoutes(service.echo)
			service.operatorChat = chat.NewService(nil, nil, hostedOperatorExecutor{service}, chat.WithClock(service.config.now))
			service.echo.POST("/internal/v1/keys/catalog", service.entryKeyCatalog, service.hostedSharedEntry, service.operatorAuthority)
			service.echo.POST("/internal/v1/keys/call", service.entryKeyCall, service.hostedSharedEntry, service.operatorAuthority)
			if err := service.database.configureHostedPlans(t.Context(), &HostedConfig{OrganizationID: string(org), Plans: &plans}); err != nil {
				t.Fatal(err)
			}
			project := tracker.ProjectID("project_" + string(org))
			states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Verified"}}, {Name: "Verified", Terminal: true, Transitions: []string{"Todo"}}}
			descriptor := hubTestPolicy()
			descriptor.Workflow = &policy.Workflow{Source: "WORKFLOW.md", Revision: descriptor.SourceRevision, States: states}
			descriptor = descriptor.WithID()
			revision, err := eventstore.NewContent(details).PrepareProjectPolicy(t.Context(), org, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			revision.ProjectID, revision.ApprovedBy, revision.ApprovedAt = project, "example-owner", now
			token := "create-token-" + string(org)
			credential := "create-operator-" + string(org)
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: credential, Name: "Example operator", Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, KeyScope: "write", ProjectAccess: "selected", Kind: "personal", UserID: user, MembershipID: membership.ID}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: "owner@example.test", MembershipID: membership.ID, Role: "owner", Active: true, PrincipalID: credential, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
				record := projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native", CreatedAt: now, WorkflowSource: "WORKFLOW.md", WorkflowSourceRevision: descriptor.SourceRevision}, States: states}
				record.Settings.DuplicateChallenge = true
				if _, err := w.CreateProjectRecord(t.Context(), record); err != nil {
					return err
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: user, ProjectID: project, CanWrite: true}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential, ProjectID: project}); err != nil {
					return err
				}
				return w.ApproveProjectPolicy(t.Context(), revision, "")
			})
			if err != nil {
				t.Fatal(err)
			}
			refresh := func() {
				t.Helper()
				for {
					more, err := searchstore.NewContent(details).RefreshOne(t.Context(), repository, org)
					if err != nil {
						t.Fatal(err)
					}
					if !more {
						break
					}
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					page, err := r.(searchstore.DuplicateSourceReader).DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: project, Text: "readiness", Now: now})
					if err == nil && (!page.State.Ready || !page.Exhausted || page.NextID != "" || len(page.Sources) > 5) {
						t.Fatal("source fixture is not current and complete")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			call := func(request tracker.CreateIssue, status int) []byte {
				t.Helper()
				raw, err := json.Marshal(request)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v2/organizations/%s/projects/%s/work-items", org, project), bytes.NewReader(raw))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				service.echo.ServeHTTP(out, req)
				if out.Code != status {
					t.Fatalf("create status=%d want=%d body=%s", out.Code, status, out.Body.String())
				}
				return out.Body.Bytes()
			}
			refresh()
			request := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "create-first"}, Title: "Filesystem import loses attachment references", Body: workItemRecordBody, State: "Todo"}
			first := call(request, http.StatusOK)
			var issue tracker.NativeIssue
			if err := json.Unmarshal(first, &issue); err != nil {
				t.Fatal(err)
			}
			if issue.WorkItemID == "" || issue.Number != 1 || issue.Body != request.Body || issue.State != "Todo" {
				t.Fatal("configured create lost issue fields")
			}
			if !bytes.Equal(first, call(request, http.StatusOK)) {
				t.Fatal("create replay changed response")
			}
			refresh()
			mutateIssue := func(method, suffix string, payload any, result any) {
				t.Helper()
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(method, fmt.Sprintf("/api/v2/organizations/%s/projects/%s/work-items/%s%s", org, project, issue.WorkItemID, suffix), bytes.NewReader(raw))
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				service.echo.ServeHTTP(out, req)
				if out.Code != http.StatusOK {
					t.Fatalf("candidate mutation status=%d body=%s", out.Code, out.Body.String())
				}
				if result != nil {
					if err := json.Unmarshal(out.Body.Bytes(), result); err != nil {
						t.Fatal(err)
					}
				}
			}
			changeCandidate := func(archived bool) {
				t.Helper()
				previous := issue.Revision
				issue.Revision++
				issue.Archived = archived
				raw, err := json.Marshal(issue)
				if err != nil {
					t.Fatal(err)
				}
				version, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: int64(issue.Revision)}, raw)
				if err != nil {
					t.Fatal(err)
				}
				document, err := searchstore.NewContent(details).Prepare(t.Context(), searchstore.Document{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, ID: "issue:" + string(issue.WorkItemID), SourceID: string(issue.WorkItemID), Kind: "issue", Scope: "issue", Revision: int64(issue.Revision), UpdatedAt: now}, searchstore.Text{Title: issue.Title, Body: issue.Body})
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					row, err := w.WorkItem(t.Context(), project, issue.WorkItemID)
					if err != nil {
						return err
					}
					if row.Revision != int64(previous) {
						return corestore.ErrRevisionConflict
					}
					row.Archived = archived
					row.ActorPrincipalID = new(issue.Actor.PrincipalID)
					if _, err := w.UpdateWorkItem(t.Context(), row); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), version); err != nil {
						return err
					}
					return w.PutSearch(t.Context(), document, int64(previous))
				}); err != nil {
					t.Fatal(err)
				}
			}
			setRole := func(role string) {
				t.Helper()
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), user)
					if err != nil {
						return err
					}
					member.Role, member.UpdatedAt = role, now
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
					t.Fatal(err)
				}
			}
			for _, test := range []struct {
				name   string
				mutate func()
				status int
			}{
				{name: "candidate revision", status: http.StatusConflict, mutate: func() {
					changeCandidate(false)
				}},
				{name: "archived candidate", status: http.StatusConflict, mutate: func() {
					changeCandidate(true)
				}},
			} {
				t.Run(test.name, func(t *testing.T) {
					request.IdempotencyKey = "stale-" + test.name
					var challenge duplicateChallenge
					if err := json.Unmarshal(call(request, http.StatusConflict), &challenge); err != nil {
						t.Fatal(err)
					}
					test.mutate()
					confirmation := request
					confirmation.ChallengeToken, confirmation.NotDuplicateOf = challenge.ChallengeToken, []int{issue.Number}
					call(confirmation, test.status)
					if issue.Archived {
						changeCandidate(false)
					}
					refresh()
				})
			}
			request.IdempotencyKey = "hidden-candidate"
			var hiddenChallenge duplicateChallenge
			if err := json.Unmarshal(call(request, http.StatusConflict), &hiddenChallenge); err != nil {
				t.Fatal(err)
			}
			originalPrincipal := issue.Actor.PrincipalID
			issue.Actor.PrincipalID = "other-reporter"
			changeCandidate(false)
			setRole("reporter")
			hiddenConfirmation := request
			hiddenConfirmation.ChallengeToken, hiddenConfirmation.NotDuplicateOf = hiddenChallenge.ChallengeToken, []int{issue.Number}
			call(hiddenConfirmation, http.StatusNotFound)
			setRole("owner")
			issue.Actor.PrincipalID = originalPrincipal
			changeCandidate(false)
			refresh()
			request.IdempotencyKey = "stale-source-sequence"
			var sourceChallenge duplicateChallenge
			if err := json.Unmarshal(call(request, http.StatusConflict), &sourceChallenge); err != nil {
				t.Fatal(err)
			}
			mutateIssue(http.MethodPost, "/comments", tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "source-comment"}, Body: "Additional attachment reference evidence"}, nil)
			sourceConfirmation := request
			sourceConfirmation.ChallengeToken, sourceConfirmation.NotDuplicateOf = sourceChallenge.ChallengeToken, []int{issue.Number}
			call(sourceConfirmation, http.StatusConflict)
			var staleSourceReplay duplicateChallenge
			if err := json.Unmarshal(call(request, http.StatusConflict), &staleSourceReplay); err != nil || staleSourceReplay.ChallengeToken != "" {
				t.Fatalf("stale source replay exposed challenge: %v", err)
			}
			refresh()
			call(sourceConfirmation, http.StatusConflict)
			request.IdempotencyKey = "create-expired-challenge"
			var expiredChallenge duplicateChallenge
			if err := json.Unmarshal(call(request, http.StatusConflict), &expiredChallenge); err != nil {
				t.Fatal(err)
			}
			request.ChallengeToken, request.NotDuplicateOf = expiredChallenge.ChallengeToken, []int{issue.Number}
			now = now.Add(duplicateChallengeLifetime)
			call(request, http.StatusGone)
			request.ChallengeToken, request.NotDuplicateOf = "", nil
			request.IdempotencyKey = "create-confirmed"
			challenged := call(request, http.StatusConflict)
			var challenge duplicateChallenge
			if err := json.Unmarshal(challenged, &challenge); err != nil {
				t.Fatal(err)
			}
			if challenge.ChallengeToken == "" || len(challenge.PossibleDuplicates) != 1 || challenge.PossibleDuplicates[0].WorkItemID != issue.WorkItemID {
				t.Fatal("enabled duplicate policy did not challenge matching source")
			}
			request.ChallengeToken = challenge.ChallengeToken
			request.NotDuplicateOf = []int{issue.Number}
			changed := request
			changed.Title += " changed"
			call(changed, http.StatusGone)
			beforeRollback := accounting()
			rollback := errors.New("injected create rollback")
			service.config.TenantRepository = createProofRepository{Repository: repository, receiptError: rollback}
			call(request, http.StatusInternalServerError)
			service.config.TenantRepository = repository
			if accounting() != beforeRollback {
				t.Fatal("rollback changed collaboration bytes or project event sequence")
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				if _, err := r.WorkItemByNumber(t.Context(), project, 2); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("rollback left issue: %v", err)
				}
				operation := fmt.Sprintf("POST /api/v2/organizations/%s/projects/%s/work-items/duplicate-confirmation", org, project)
				if _, err := r.Receipt(t.Context(), credential, operation, request.IdempotencyKey); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("rollback left confirmation receipt: %v", err)
				}
				items, err := r.WorkItems(t.Context(), project, 0, 10)
				if err != nil {
					return err
				}
				if len(items) != 1 || items[0].NativeID != issue.WorkItemID {
					t.Fatal("rollback changed project issue population")
				}
				stored, err := r.(corestore.DuplicateChallengeReader).DuplicateChallenge(t.Context(), project, challenge.ChallengeToken)
				if err == nil && (stored.RequestID != request.IdempotencyKey || len(stored.Candidates) != 1) {
					t.Fatal("rollback changed challenge")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(challenged, call(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: request.IdempotencyKey}, Title: request.Title, Body: request.Body, State: request.State}, http.StatusConflict)) {
				t.Fatal("rollback changed original challenge replay")
			}
			arrived := make(chan struct{}, 2)
			release := make(chan struct{})
			var barrier sync.WaitGroup
			var arrivals atomic.Int32
			service.config.TenantRepository = createProofRepository{Repository: repository, beforeLock: func() {
				if arrivals.Add(1) <= 2 {
					arrived <- struct{}{}
				}
				<-release
			}}
			t.Cleanup(func() {
				select {
				case <-release:
				default:
					close(release)
				}
				barrier.Wait()
			})
			confirmRaw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			confirmations := make(chan *httptest.ResponseRecorder, 2)
			for range 2 {
				barrier.Add(1)
				go func() {
					defer barrier.Done()
					req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v2/organizations/%s/projects/%s/work-items", org, project), bytes.NewReader(confirmRaw))
					req.Header.Set("Authorization", "Bearer "+token)
					req.Header.Set("Content-Type", "application/json")
					response := httptest.NewRecorder()
					service.echo.ServeHTTP(response, req)
					confirmations <- response
				}()
			}
			for range 2 {
				select {
				case <-arrived:
				case result := <-confirmations:
					t.Fatalf("confirmation returned before collision barrier: status=%d body=%s", result.Code, result.Body.String())
				}
			}
			close(release)
			firstConfirmation, secondConfirmation := <-confirmations, <-confirmations
			barrier.Wait()
			service.config.TenantRepository = repository
			if firstConfirmation.Code != http.StatusOK || secondConfirmation.Code != http.StatusOK || !bytes.Equal(firstConfirmation.Body.Bytes(), secondConfirmation.Body.Bytes()) {
				t.Fatalf("concurrent confirmations status=%d/%d", firstConfirmation.Code, secondConfirmation.Code)
			}
			confirmed := firstConfirmation.Body.Bytes()
			var created tracker.NativeIssue
			if err := json.Unmarshal(confirmed, &created); err != nil {
				t.Fatal(err)
			}
			if created.Number != 2 || created.Revision != 2 || len(created.Related) != 1 || created.Related[0].ID != issue.WorkItemID {
				t.Fatalf("confirmed issue number=%d revision=%d", created.Number, created.Revision)
			}
			beforeReplay := accounting()
			if !bytes.Equal(confirmed, call(request, http.StatusOK)) {
				t.Fatal("consumed challenge did not replay successful receipt")
			}
			if accounting() != beforeReplay || beforeReplay[0] != beforeRollback[0] {
				t.Fatal("confirmation or replay charged collaboration bytes or duplicated events")
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				links, err := r.Relationships(t.Context(), project, created.WorkItemID, corestore.RelationshipCursor{}, 10)
				if err != nil {
					return err
				}
				if len(links.Items) != 1 || links.Items[0].RelatedWorkItemID != issue.WorkItemID {
					t.Fatal("confirmation lost related link")
				}
				items, err := r.WorkItems(t.Context(), project, 0, 10)
				if err != nil {
					return err
				}
				if len(items) != 2 {
					t.Fatal("confirmation replay created an additional issue")
				}
				events, err := r.Events(t.Context(), project, created.WorkItemID, 0, 10)
				if err != nil {
					return err
				}
				if len(events) != 2 || events[0].Type != "issue.created" || events[1].Type != "issue.edited" {
					t.Fatal("confirmation replay duplicated create or related events")
				}
				if _, err := r.(corestore.DuplicateChallengeReader).DuplicateChallenge(t.Context(), project, challenge.ChallengeToken); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("confirmation retained consumed challenge: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			refresh()
			racing := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "racing-no-match"}, Title: "Ultraviolet marmalade telescope", Body: "Copper hedgehog circuitry", State: "Verified"}
			var inserted sync.Once
			service.config.TenantRepository = createProofRepository{Repository: repository, beforeLock: func() {
				inserted.Do(func() {
					service.config.TenantRepository = repository
					matching := racing
					matching.IdempotencyKey = "new-matching-source"
					call(matching, http.StatusOK)
				})
			}}
			call(racing, http.StatusConflict)
			service.config.TenantRepository = repository
			refresh()
			var raceChallenge duplicateChallenge
			if err := json.Unmarshal(call(racing, http.StatusConflict), &raceChallenge); err != nil {
				t.Fatal(err)
			}
			if len(raceChallenge.PossibleDuplicates) != 1 || raceChallenge.PossibleDuplicates[0].Number != 3 {
				t.Fatal("concurrent matching source did not invalidate no-match")
			}
			racing.ChallengeToken, racing.NotDuplicateOf = raceChallenge.ChallengeToken, []int{3}
			var expire sync.Once
			service.config.TenantRepository = createProofRepository{Repository: repository, beforeLock: func() {
				expire.Do(func() { now = now.Add(duplicateChallengeLifetime) })
			}}
			call(racing, http.StatusGone)
			service.config.TenantRepository = repository
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				if _, err := r.WorkItemByNumber(t.Context(), project, 4); !errors.Is(err, corestore.ErrNotFound) {
					t.Fatalf("refused stale/expired create left issue: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			entrySigner := hostedSharedKey(7)
			service.config.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{entrySigner.Public().(ed25519.PublicKey)}, Generation: 1}
			if err := service.config.Hosted.SharedEntry.validate(string(org)); err != nil {
				t.Fatal(err)
			}
			service.config.Hosted.SharedEntry.verifier.Now = service.config.now
			entryCall := func(path string, payload any) *httptest.ResponseRecorder {
				t.Helper()
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
				id, err := cloudassert.NewID()
				if err != nil {
					t.Fatal(err)
				}
				claims := cloudassert.Claims{Issuer: "entry", Audience: string(org), Generation: 1, Kind: cloudassert.KindKey, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(raw), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id, Subject: user, ProviderOrganization: "provider_" + string(org), Role: "owner", Key: &apikey.KeyAuthority{ID: "entry-key-" + string(org), Kind: "personal", Permission: apikey.ScopeWrite, ProjectContext: apikey.ProjectContext{Access: "selected", Projects: []string{string(project)}}}}
				assertion, err := cloudassert.Sign(entrySigner, claims)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set(cloudassert.Header, assertion)
				req.Header.Set("Content-Type", "application/json")
				out := httptest.NewRecorder()
				service.echo.ServeHTTP(out, req)
				if out.Code != http.StatusOK {
					t.Fatalf("entry path=%s status=%d body=%s", path, out.Code, out.Body.String())
				}
				return out
			}
			catalog := entryCall("/internal/v1/keys/catalog", map[string]any{})
			var definitions []operatortool.Definition
			if err := json.Unmarshal(catalog.Body.Bytes(), &definitions); err != nil {
				t.Fatal(err)
			}
			found := false
			for _, definition := range definitions {
				found = found || definition.Name == operatortool.FileIssue
			}
			if !found {
				t.Fatal("file_issue catalog leaf missing")
			}
			entryCreated := entryCall("/internal/v1/keys/call", map[string]any{"name": operatortool.FileIssue, "connection_id": "example-connection", "arguments": map[string]any{"project_id": project, "title": "Zebra quartz envelope", "description": "Cobalt lantern velocity", "state": "Verified", "request_id": "catalog-create"}})
			var entryEnvelope struct {
				ResourceID tracker.NativeWorkItemID `json:"resource_id"`
				Data       tracker.NativeIssue      `json:"data"`
			}
			if err := json.Unmarshal(entryCreated.Body.Bytes(), &entryEnvelope); err != nil {
				t.Fatal(err)
			}
			entryIssue := entryEnvelope.Data
			if entryEnvelope.ResourceID != entryIssue.WorkItemID {
				t.Fatal("file_issue resource identity differs from issue")
			}
			if entryIssue.WorkItemID == "" || entryIssue.OrganizationID != org || entryIssue.ProjectID != project || entryIssue.Title != "Zebra quartz envelope" || entryIssue.Body != "Cobalt lantern velocity" || entryIssue.State != "Verified" {
				t.Fatal("file_issue response lost canonical issue fields")
			}
			var storedRow corestore.WorkItem
			var storedVersion eventstore.CollaborationVersion
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				storedRow, err = r.WorkItem(t.Context(), project, entryIssue.WorkItemID)
				if err != nil {
					return err
				}
				storedVersion, err = r.CollaborationVersion(t.Context(), string(storedRow.NativeID), storedRow.Revision)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			stored, err := service.hydrateWorkItemRecord(t.Context(), storedRow, storedVersion)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Title != entryIssue.Title || stored.Body != entryIssue.Body || stored.State != entryIssue.State || stored.OrganizationID != org || stored.ProjectID != project {
				t.Fatal("file_issue stored fields differ from response")
			}
			refresh()
			entryArguments := map[string]any{"project_id": project, "title": entryIssue.Title, "description": entryIssue.Body, "state": "Verified", "request_id": "catalog-confirm"}
			entryPayload := map[string]any{"name": operatortool.FileIssue, "connection_id": "example-connection", "arguments": entryArguments}
			entryChallenged := entryCall("/internal/v1/keys/call", entryPayload)
			var challengeEnvelope struct {
				Data duplicateChallenge `json:"data"`
			}
			if err := json.Unmarshal(entryChallenged.Body.Bytes(), &challengeEnvelope); err != nil {
				t.Fatal(err)
			}
			entryChallenge := challengeEnvelope.Data
			if entryChallenge.ChallengeToken == "" || len(entryChallenge.PossibleDuplicates) != 1 || entryChallenge.PossibleDuplicates[0].WorkItemID != entryIssue.WorkItemID {
				t.Fatal("signed file_issue lost duplicate challenge")
			}
			entryArguments["challenge_token"], entryArguments["not_duplicate_of"] = entryChallenge.ChallengeToken, []int{entryIssue.Number}
			entryConfirmed := entryCall("/internal/v1/keys/call", entryPayload)
			if err := json.Unmarshal(entryConfirmed.Body.Bytes(), &entryEnvelope); err != nil {
				t.Fatal(err)
			}
			if entryEnvelope.Data.Number != entryIssue.Number+1 || len(entryEnvelope.Data.Related) != 1 || entryEnvelope.Data.Related[0].ID != entryIssue.WorkItemID {
				t.Fatal("signed file_issue confirmation lost issue or related link")
			}
			beforeSignedReplay := accounting()
			entryReplayed := entryCall("/internal/v1/keys/call", entryPayload)
			var confirmationResponses [2]struct {
				ResourceID tracker.NativeWorkItemID `json:"resource_id"`
				Revision   tracker.Revision         `json:"revision,string"`
				URL        string                   `json:"url"`
				Data       json.RawMessage          `json:"data"`
			}
			for i, response := range []*httptest.ResponseRecorder{entryConfirmed, entryReplayed} {
				if err := json.Unmarshal(response.Body.Bytes(), &confirmationResponses[i]); err != nil {
					t.Fatal(err)
				}
			}
			original, replayed := confirmationResponses[0], confirmationResponses[1]
			if original.ResourceID != replayed.ResourceID || original.Revision != replayed.Revision || original.URL != replayed.URL || !bytes.Equal(original.Data, replayed.Data) {
				t.Fatal("signed file_issue confirmation replay changed canonical outcome")
			}
			if accounting() != beforeSignedReplay {
				t.Fatal("signed confirmation replay changed accounting or events")
			}
			t.Run("machine fingerprint occurrence and terminal history", func(t *testing.T) {
				refresh()
				machine := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "machine-first"}, Title: "Example machine finding", Body: issueorigin.Stamp("Repeated example backend failure", issueorigin.Origin{Kind: "doctor", Instance: "example-instance", Fingerprint: "example-machine-fingerprint"}), State: "Todo"}
				var original tracker.NativeIssue
				if err := json.Unmarshal(call(machine, http.StatusOK), &original); err != nil {
					t.Fatal(err)
				}
				refresh()
				machine.IdempotencyKey = "machine-occurrence"
				machine.Priority = new(1)
				machine.Provenance = &tracker.Provenance{Provider: "github", ExternalID: "example-machine-occurrence", AuthorID: "example-author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}
				var reused tracker.NativeIssue
				response := call(machine, http.StatusOK)
				if err := json.Unmarshal(response, &reused); err != nil {
					t.Fatal(err)
				}
				if reused.WorkItemID != original.WorkItemID || !reused.PublicationReused || reused.State != original.State || reused.Priority == nil || *reused.Priority != 1 {
					t.Fatal("machine fingerprint did not preserve current issue, priority and dispatch intent")
				}
				before := accounting()
				if !bytes.Equal(response, call(machine, http.StatusOK)) || accounting() != before {
					t.Fatal("machine occurrence replay created additional writes")
				}
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					comments, err := r.Comments(t.Context(), project, original.WorkItemID, 0, 10)
					if err == nil && len(comments) != 1 {
						t.Fatal("machine occurrence did not create exactly one comment")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				machine.Provenance = nil
				machine.Title, machine.State, machine.IdempotencyKey = "Example closed machine finding", "Verified", "machine-closed"
				machine.Body = issueorigin.Stamp("Closed example finding", issueorigin.Origin{Kind: "doctor", Instance: "example-instance", Fingerprint: "example-closed-fingerprint"})
				if err := json.Unmarshal(call(machine, http.StatusOK), &original); err != nil {
					t.Fatal(err)
				}
				refresh()
				machine.IdempotencyKey = "machine-closed-occurrence"
				before = accounting()
				if err := json.Unmarshal(call(machine, http.StatusOK), &reused); err != nil {
					t.Fatal(err)
				}
				if reused.WorkItemID != original.WorkItemID || !reused.PublicationReused || reused.State != "Verified" || accounting() != before {
					t.Fatal("machine duplicate reopened or rewrote closed history")
				}
			})
			for _, test := range []struct {
				name string
				page searchstore.DuplicateSourcePage
				err  error
				race bool
			}{
				{name: "unavailable index", page: searchstore.DuplicateSourcePage{Exhausted: true}},
				{name: "pending index", err: searchstore.ErrIndexPending},
				{name: "index becomes pending before write", page: searchstore.DuplicateSourcePage{State: searchstore.DuplicateSourceState{Ready: true}}, race: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					proof := createProofRepository{Repository: repository, duplicatePage: &test.page, duplicateError: test.err}
					if test.race {
						proof.sourceError = searchstore.ErrIndexPending
						proof.beforeWrite = func() { test.page.State.Ready = false }
					}
					service.config.TenantRepository = proof
					defer func() { service.config.TenantRepository = repository }()
					fallback := request
					fallback.ChallengeToken, fallback.NotDuplicateOf = "", nil
					fallback.IdempotencyKey = "fallback-create-" + test.name
					created := call(fallback, http.StatusOK)
					if !bytes.Equal(created, call(fallback, http.StatusOK)) {
						t.Fatal("unavailable-index filing replay changed response")
					}
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						current, err := r.ProjectRecord(t.Context(), project)
						if err == nil && !current.Settings.DuplicateChallenge {
							t.Fatal("unavailable-index filing disabled duplicate policy")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
			t.Run("actual pending index filing", func(t *testing.T) {
				if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					state, err := r.(searchstore.DuplicateSourceReader).DuplicateSourceState(t.Context(), project)
					if err == nil && state.Ready {
						t.Fatal("pending fixture unexpectedly has an available index")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				fallback := request
				fallback.ChallengeToken, fallback.NotDuplicateOf = "", nil
				fallback.IdempotencyKey = "actual-pending-create"
				response := call(fallback, http.StatusOK)
				before := accounting()
				if !bytes.Equal(response, call(fallback, http.StatusOK)) || accounting() != before {
					t.Fatal("pending-index replay added canonical writes")
				}
			})
			service.config.Hosted.SharedEntry = nil
			var currentProject projectstore.Record
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				currentProject, err = r.ProjectRecord(t.Context(), project)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			refresh()
			workflowReplay := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "workflow-replay"}, Title: request.Title, Body: request.Body, State: request.State}
			var workflowChallenge duplicateChallenge
			if err := json.Unmarshal(call(workflowReplay, http.StatusConflict), &workflowChallenge); err != nil || workflowChallenge.ChallengeToken == "" {
				t.Fatalf("workflow replay fixture did not challenge: %v", err)
			}
			mismatchedProject := currentProject
			mismatchedProject.Core.WorkflowSourceRevision = "unapproved-source"
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.UpdateProjectRecord(t.Context(), mismatchedProject)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			call(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "mismatched-workflow-source"}, Title: "Unapproved workflow source", Body: workItemRecordBody, State: "Todo"}, http.StatusConflict)
			var refusedReplay duplicateChallenge
			if err := json.Unmarshal(call(workflowReplay, http.StatusConflict), &refusedReplay); err != nil || refusedReplay.ChallengeToken != "" {
				t.Fatalf("mismatched workflow replay exposed challenge: %v", err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				current, err := w.ProjectRecord(t.Context(), project)
				if err != nil {
					return err
				}
				current.Core.WorkflowSourceRevision = currentProject.Core.WorkflowSourceRevision
				_, err = w.UpdateProjectRecord(t.Context(), current)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			ownerToken := token
			token = "read-token-" + string(org)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				readID := "read-key-" + string(org)
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: readID, Name: "Read only example", Hash: apikey.HashToken(token), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, KeyScope: "read", ProjectAccess: "selected", Kind: "personal", UserID: user, MembershipID: membership.ID}); err != nil {
					return err
				}
				return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: readID, ProjectID: project})
			}); err != nil {
				t.Fatal(err)
			}
			call(request, http.StatusForbidden)
			token = ownerToken
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.DeleteAccessTokenGrant(t.Context(), credential, project) }); err != nil {
				t.Fatal(err)
			}
			call(request, http.StatusForbidden)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential, ProjectID: project})
			}); err != nil {
				t.Fatal(err)
			}

			refresh()
			grantRace := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "grant-revoked-before-commit"}, Title: "Cobalt octopus conveyor", Body: "Unique filing for current project authorization", State: "Verified"}
			var revokeGrant sync.Once
			service.config.TenantRepository = createProofRepository{Repository: repository, beforeWrite: func() {
				revokeGrant.Do(func() {
					if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.DeleteAccessTokenGrant(t.Context(), credential, project)
					}); err != nil {
						t.Fatal(err)
					}
				})
			}}
			beforeGrantRace := accounting()
			call(grantRace, http.StatusForbidden)
			service.config.TenantRepository = repository
			if accounting() != beforeGrantRace {
				t.Fatal("revoked project selection changed durable accounting or events")
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: credential, ProjectID: project})
			}); err != nil {
				t.Fatal(err)
			}
			setRole("viewer")
			call(request, http.StatusForbidden)
			setRole("owner")
			refresh()
			revokedRequest := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "challenge-current-key"}, Title: request.Title, Body: request.Body, State: request.State}
			var revokedChallenge duplicateChallenge
			if err := json.Unmarshal(call(revokedRequest, http.StatusConflict), &revokedChallenge); err != nil {
				t.Fatal(err)
			}
			revokedRequest.ChallengeToken, revokedRequest.NotDuplicateOf = revokedChallenge.ChallengeToken, []int{issue.Number}
			now = now.Add(receiptstore.ReplayRetention)
			call(request, http.StatusGone)
			now = now.Add(time.Nanosecond)
			if err := service.expireNativeRecords(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			call(request, http.StatusGone)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(t.Context(), credential, now) }); err != nil {
				t.Fatal(err)
			}
			call(request, http.StatusUnauthorized)
			call(revokedRequest, http.StatusUnauthorized)
		})
	}
}

type createProofRepository struct {
	tenantstore.Repository
	beforeWrite    func()
	beforeLock     func()
	receiptError   error
	duplicatePage  *searchstore.DuplicateSourcePage
	duplicateError error
	sourceError    error
}

func (r createProofRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		if r.duplicatePage != nil {
			return read(createProofReader{Reader: reader, page: *r.duplicatePage, err: r.duplicateError})
		}
		return read(reader)
	})
}

type createProofReader struct {
	tenantstore.Reader
	page searchstore.DuplicateSourcePage
	err  error
}

func (r createProofReader) DuplicateSources(context.Context, searchstore.DuplicateSourceQuery) (searchstore.DuplicateSourcePage, error) {
	return r.page, r.err
}

func (r createProofReader) DuplicateSourceState(context.Context, tracker.ProjectID) (searchstore.DuplicateSourceState, error) {
	return r.page.State, r.err
}

func (r createProofRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	if r.beforeWrite != nil {
		r.beforeWrite()
	}
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		return write(createProofWriter{Writer: w, DuplicateChallengeWriter: w.(corestore.DuplicateChallengeWriter), DuplicateSourceWriter: w.(searchstore.DuplicateSourceWriter), beforeLock: r.beforeLock, receiptError: r.receiptError, sourceError: r.sourceError})
	})
}

type createProofWriter struct {
	tenantstore.Writer
	corestore.DuplicateChallengeWriter
	searchstore.DuplicateSourceWriter
	beforeLock   func()
	receiptError error
	sourceError  error
}

func (w createProofWriter) LockDuplicateSource(ctx context.Context, project tracker.ProjectID, sequence int64) error {
	if w.sourceError != nil {
		return w.sourceError
	}
	return w.DuplicateSourceWriter.LockDuplicateSource(ctx, project, sequence)
}

func (w createProofWriter) LockWorkItemProject(ctx context.Context, project tracker.ProjectID) error {
	if w.beforeLock != nil {
		w.beforeLock()
	}
	return w.Writer.LockWorkItemProject(ctx, project)
}

func (w createProofWriter) PutReceipt(ctx context.Context, receipt eventstore.Receipt) (eventstore.Receipt, error) {
	stored, err := w.Writer.PutReceipt(ctx, receipt)
	if err == nil && w.receiptError != nil {
		return stored, w.receiptError
	}
	return stored, err
}
