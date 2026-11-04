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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeyMoveItem(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	key := hostedSharedKey(64)
	clock := time.Now().UTC().Truncate(time.Microsecond)
	initialClock := clock
	states := humanWorkflowFixtureStates()
	states = append(states, tracker.NativeState{Name: "Triage", Transitions: []string{"Todo", "Cancelled"}}, tracker.NativeState{Name: "Review", Transitions: []string{"Merging", "Rework", "Cancelled"}}, tracker.NativeState{Name: "Merging", Dispatchable: true, Transitions: []string{"Done", "Rework"}})
	descriptor := hubTestPolicy()
	descriptor.Workflow = &policy.Workflow{Source: "example-workflow.yaml", Revision: descriptor.SourceRevision, States: states}
	descriptor = descriptor.WithID()
	permissions := map[string]apikey.Scope{"owner": apikey.ScopeAdmin, "admin": apikey.ScopeAdmin, "member": apikey.ScopeWrite, "reporter": apikey.ScopeWrite, "reader": apikey.ScopeRead}
	roles := map[string]string{"owner": "owner", "admin": "admin", "member": "member", "reporter": "reporter", "reader": "owner"}
	revoked := false
	blocked := []string(nil)
	projectAccess := "all"
	cfg := startupConfig(connection)
	cfg.now = func() time.Time { return clock }
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	plans := capacityHostedPlans()
	cfg.Hosted.Plans = &plans
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	resolver := func(organization string) entryauthority.Resolver {
		return func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
			if revoked {
				return cloudassert.Claims{}, operatortool.ErrAccessDenied
			}
			return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: organization, ProviderOrganization: "provider_" + organization, Subject: origin.Subject, Role: roles[origin.Subject], Kind: cloudassert.KindKey, ExpiresAt: clock.Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: permissions[origin.Subject], BlockedProjects: blocked, ProjectContext: apikey.ProjectContext{Access: projectAccess, Projects: moveItemSelectedProjects(projectAccess)}}}, nil
		}
	}
	cfg.Hosted.SharedEntry.ResolveKey = resolver("org_startup")
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_other"} {
		seedStartupOrganization(t, store.postgres, organization)
		prepared, err := eventstore.NewContent(cfg.RunDetails).PrepareProjectPolicy(t.Context(), organization, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		prepared.ProjectID, prepared.ApprovedBy, prepared.ApprovedAt = "project_example", "credential_owner", clock
		rules := defaultChangeReviewPolicy(descriptor)
		rules.RequireReview = true
		rules.ID = changerequest.PolicyID(rules)
		reviewPolicy, err := eventstore.NewContent(cfg.RunDetails).PrepareReviewPolicy(t.Context(), eventstore.ReviewPolicy{OrganizationID: organization, ProjectID: "project_example", PolicyRevisionID: descriptor.ID, Revision: 1, UpdatedBy: "credential_owner", UpdatedAt: clock}, rules)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			cursor, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x36}, 32), accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: cursor, UpdatedAt: clock}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursor.Version, RecordedAt: clock}); err != nil {
				return err
			}
			for _, user := range []string{"owner", "admin", "member", "reporter", "reader"} {
				id := "credential_" + user
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: id, Name: "Example " + user, Hash: apikey.HashToken(string(organization) + user), Scope: "operator", NativeOnly: true, CreatedAt: clock, UpdatedAt: clock, UserID: user, MembershipID: "membership_" + user, KeyScope: string(permissions[user]), ProjectAccess: "all", Kind: "personal"}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: roles[user], Active: true, PrincipalID: id, CreatedAt: clock, UpdatedAt: clock}); err != nil {
					return err
				}
			}
			for _, id := range []tracker.ProjectID{"project_example", "project_foreign"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: "Example " + string(id), Slug: strings.ReplaceAll(string(id), "_", "-"), Profile: "native"}, States: states}); err != nil {
					return err
				}
			}
			for _, user := range []string{"owner", "admin", "member", "reporter", "reader"} {
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "credential_" + user, ProjectID: "project_example"}); err != nil {
					return err
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: user, ProjectID: "project_example", CanWrite: true}); err != nil {
					return err
				}
			}
			if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: organization, ID: "machine_example", Hostname: "example", Capacity: 2, Version: "test"}); err != nil {
				return err
			}
			if err := w.ApproveProjectPolicy(t.Context(), prepared, ""); err != nil {
				return err
			}
			return w.SetReviewPolicy(t.Context(), reviewPolicy, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	if service.database.reader != nil || service.database.db != nil {
		t.Fatal("fixture must be PostgreSQL only")
	}
	service.config.Hosted.SharedEntry.verifier.Now = cfg.now
	repository := service.config.TenantRepository
	base := "/api/v2/organizations/org_startup/projects/project_example/work-items"
	requestFor := func(target *Service, user, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		claims := cloudassert.Claims{Issuer: "entry", Audience: target.config.Hosted.OrganizationID, ProviderOrganization: "provider_" + target.config.Hosted.OrganizationID, Subject: user, Role: roles[user], Generation: 1, Kind: cloudassert.KindKey, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(raw), IssuedAt: clock, ExpiresAt: clock.Add(20 * time.Second), ID: id, Key: &apikey.KeyAuthority{ID: "key_" + user, CredentialBinding: strings.Repeat("a", 64), Kind: "personal", Permission: permissions[user], BlockedProjects: blocked, ProjectContext: apikey.ProjectContext{Access: projectAccess, Projects: moveItemSelectedProjects(projectAccess)}}}
		assertion, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(cloudassert.Header, assertion)
		response := httptest.NewRecorder()
		target.Handler().ServeHTTP(response, req)
		return response
	}
	request := func(user, path string, body any) *httptest.ResponseRecorder {
		return requestFor(service, user, path, body)
	}
	create := func(user, id, state string) tracker.NativeIssue {
		t.Helper()
		body := workItemRecordBody
		if user == "reporter" {
			body = reporterBugSections
		}
		response := request(user, base, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: id}, Title: "Example " + id, Body: body, State: state, Type: "bug", Labels: []string{"example-label"}, Assignees: []string{"example-user"}, Priority: new(2)})
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return issue
	}
	call := func(user string, args operatortool.MoveItemArguments) *httptest.ResponseRecorder {
		return request(user, "/internal/v1/keys/call", map[string]any{"name": operatortool.MoveItem, "arguments": args, "connection_id": "move_" + user})
	}
	move := func(issue tracker.NativeIssue, key, target string) operatortool.MoveItemArguments {
		return operatortool.MoveItemArguments{ProjectID: "project_example", Identifier: string(issue.WorkItemID), ExpectedRevision: int64(issue.Revision), TargetState: target, RequestID: key, ReasonDetail: "Example human request"}
	}
	decode := func(response *httptest.ResponseRecorder) tracker.NativeIssue {
		t.Helper()
		requireNativeStatus(t, response, http.StatusOK)
		var result struct {
			Data tracker.NativeIssue `json:"data"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Data
	}
	cached := map[string]tracker.NativeIssue{}
	for _, input := range []struct{ user, id, state string }{{"owner", "completed", "Done"}, {"owner", "admin", "Done"}, {"owner", "member", "Done"}, {"reporter", "reporter_own", "Triage"}, {"reporter", "reporter_fresh_race", "Triage"}, {"reporter", "reporter_replay_race", "Triage"}} {
		cached[input.id] = create(input.user, input.id, input.state)
	}
	create = func(user, id, state string) tracker.NativeIssue {
		t.Helper()
		issue, ok := cached[id]
		if !ok || issue.State != state {
			t.Fatal("missing prepared issue")
		}
		return issue
	}
	configure := func(organization tracker.OrganizationID) {
		t.Helper()
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(t.Context(), "project_example")
			if err != nil {
				return err
			}
			project.Core.WorkflowSource, project.Core.WorkflowSourceRevision = descriptor.Workflow.Source, descriptor.Workflow.Revision
			_, err = w.UpdateProjectRecord(t.Context(), project)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	configure("org_startup")
	for _, user := range []string{"owner", "admin", "member", "reporter", "reader"} {
		t.Run("catalog_"+user, func(t *testing.T) {
			response := request(user, "/internal/v1/keys/catalog", map[string]any{})
			requireNativeStatus(t, response, http.StatusOK)
			var definitions []operatortool.Definition
			decodeHubResponse(t, response, &definitions)
			found := false
			for _, definition := range definitions {
				if definition.Name == operatortool.MoveItem {
					found = true
					if definition.Annotations.ReadOnly {
						t.Error("MoveItem is a mutation")
					}
				}
				if definition.Name == operatortool.StopRun {
					t.Error("StopRun must remain unavailable")
				}
				if user == "reader" && !definition.Annotations.ReadOnly {
					t.Errorf("reader gained mutation %s", definition.Name)
				}
			}
			if found != (user != "reader") {
				t.Errorf("MoveItem availability=%v", found)
			}
		})
	}
	if t.Failed() {
		return
	}

	otherConfig := cfg
	otherHosted := *cfg.Hosted
	otherHosted.OrganizationID = "org_other"
	otherHosted.WorkOSOrganizationID = "provider_org_other"
	otherEntry := *cfg.Hosted.SharedEntry
	otherEntry.ResolveKey = resolver("org_other")
	otherHosted.SharedEntry = &otherEntry
	otherConfig.Hosted = &otherHosted
	other, err := Open(t.Context(), otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := other.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
	})
	other.config.Hosted.SharedEntry.verifier.Now = cfg.now
	otherBase := "/api/v2/organizations/org_other/projects/project_example/work-items"
	otherCreated := requestFor(other, "owner", otherBase, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "other_issue"}, Title: "Other tenant issue", Body: workItemRecordBody, State: "Done", Type: "bug"})
	requireNativeStatus(t, otherCreated, http.StatusOK)
	var otherIssue tracker.NativeIssue
	decodeHubResponse(t, otherCreated, &otherIssue)
	configure("org_other")
	otherArgs := move(otherIssue, "other_move", "Rework")
	otherMoved := decode(requestFor(other, "owner", "/internal/v1/keys/call", map[string]any{"name": operatortool.MoveItem, "arguments": otherArgs, "connection_id": "other_move"}))
	if otherMoved.OrganizationID != "org_other" || otherMoved.State != "Rework" || otherMoved.Revision != otherIssue.Revision+1 {
		t.Fatal("second tenant is not independently usable")
	}
	requireNativeStatus(t, call("owner", otherArgs), http.StatusForbidden)
	t.Run("signed Change creation preserves tenant authority and HTTP replay", func(t *testing.T) {
		for _, target := range []struct {
			service *Service
			issue   tracker.NativeIssue
		}{{service, cached["completed"]}, {other, otherMoved}} {
			for _, user := range []string{"owner", "admin", "member", "reporter", "reader"} {
				catalog := requestFor(target.service, user, "/internal/v1/keys/catalog", map[string]any{})
				requireNativeStatus(t, catalog, http.StatusOK)
				var definitions []operatortool.Definition
				decodeHubResponse(t, catalog, &definitions)
				if entryKeyHasDefinition(definitions, operatortool.CreateChange) != (user != "reader" && user != "reporter") {
					t.Fatalf("create_change catalog authorization incorrect for %s", user)
				}
			}
			linkedPath := "/api/v2/organizations/" + string(target.issue.OrganizationID) + "/projects/project_example/work-items"
			linkedResponse := requestFor(target.service, "owner", linkedPath, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "change-linked-item"}, Title: "Example linked issue", Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, linkedResponse, http.StatusOK)
			var linked tracker.NativeIssue
			decodeHubResponse(t, linkedResponse, &linked)
			args := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(target.issue.WorkItemID), LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}, RequestID: "example-change", Title: "Example Change", Body: "Example discussion"}
			invoke := func(user string, input operatortool.ChangeArguments) *httptest.ResponseRecorder {
				return requestFor(target.service, user, "/internal/v1/keys/call", map[string]any{"name": operatortool.CreateChange, "arguments": input, "connection_id": "change_" + user})
			}
			response := invoke("owner", args)
			requireNativeStatus(t, response, http.StatusOK)
			var created operatortool.ChangeResult
			decodeHubResponse(t, response, &created)
			if created.ChangeID == "" || created.ProjectID != "project_example" || created.WorkItemID != string(target.issue.WorkItemID) || created.OrganizationID != string(target.issue.OrganizationID) {
				t.Fatal("Change creation lost canonical identity or organization")
			}
			listed := requestFor(target.service, "owner", "/internal/v1/keys/call", map[string]any{"name": operatortool.ListChanges, "connection_id": "change_list", "arguments": map[string]any{"project_id": "project_example", "work_item_id": target.issue.WorkItemID}})
			requireNativeStatus(t, listed, http.StatusOK)
			var list operatortool.ChangeResult
			decodeHubResponse(t, listed, &list)
			if len(list.Changes) != 1 || list.Changes[0].ID != created.ChangeID || len(list.Changes[0].LinkedIssues) != 2 {
				t.Fatal("typed Change list lost the newly created Change")
			}
			app := hubChangeApplication{service: target.service}
			for _, name := range []string{operatortool.GetChange, operatortool.GetChangeVersion, operatortool.GetChangeReviewPolicy, operatortool.ArtifactReferences} {
				if value, err := app.ReadChange(t.Context(), name, args); !errors.Is(err, operatortool.ErrServiceUnavailable) || value.Receipt != nil {
					t.Fatalf("unsupported PostgreSQL read %s entered a legacy owner: %v", name, err)
				}
			}
			replay := invoke("owner", args)
			requireNativeStatus(t, replay, http.StatusOK)
			var repeated operatortool.ChangeResult
			decodeHubResponse(t, replay, &repeated)
			if repeated.ChangeID != created.ChangeID || !bytes.Equal(repeated.Receipt, created.Receipt) {
				t.Fatal("Change replay changed the canonical receipt")
			}
			path := "/api/v2/organizations/" + string(target.issue.OrganizationID) + "/projects/project_example/work-items/" + string(target.issue.WorkItemID) + "/changes"
			httpReplay := requestFor(target.service, "owner", path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: args.RequestID}, Title: args.Title, Body: args.Body, LinkedIssues: args.LinkedIssues})
			requireNativeStatus(t, httpReplay, http.StatusOK)
			var fromHTTP tracker.ChangeRequest
			decodeHubResponse(t, httpReplay, &fromHTTP)
			if fromHTTP.ID != created.ChangeID || fromHTTP.OrganizationID != target.issue.OrganizationID {
				t.Fatal("HTTP replay created a second Change or crossed tenants")
			}
			t.Run("publish and review "+string(target.issue.OrganizationID), func(t *testing.T) {
				target := target
				issueResponse := requestFor(target.service, "owner", linkedPath, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "workflow-review-issue"}, Title: "Example review workflow", Body: workItemRecordBody, State: "Review"})
				requireNativeStatus(t, issueResponse, http.StatusOK)
				decodeHubResponse(t, issueResponse, &target.issue)
				path := linkedPath + "/" + string(target.issue.WorkItemID) + "/changes"
				changeResponse := requestFor(target.service, "owner", "/internal/v1/keys/call", map[string]any{"name": operatortool.CreateChange, "connection_id": "workflow_change", "arguments": operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(target.issue.WorkItemID), LinkedIssues: []tracker.NativeWorkItemID{linked.WorkItemID}, RequestID: "workflow_change", Title: "Example workflow Change"}})
				requireNativeStatus(t, changeResponse, http.StatusOK)
				var created operatortool.ChangeResult
				decodeHubResponse(t, changeResponse, &created)
				var history leasestore.Attempt
				if err := repository.WriteTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					row, err := w.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
					if err != nil {
						return err
					}
					lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: "machine_example", SessionID: "workflow_history", LeaseID: "workflow_history", TTL: time.Minute})
					if err != nil {
						return err
					}
					fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
					if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "workflow_history", RunID: "workflow_history", Fence: fence, Sequence: 1, Status: "running", StartedAt: clock, PayloadObjectKey: "example/workflow-start"}); err != nil {
						return err
					}
					history, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "workflow_history", RunID: "workflow_history", Fence: fence, Sequence: 2, Status: "succeeded", PayloadObjectKey: "example/workflow-done"})
					if err != nil {
						return err
					}
					_, err = w.Release(t.Context(), fence, "released")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				input := changeTestInput()
				input.PolicyID = descriptor.ID
				empty := ""
				publish := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(target.issue.WorkItemID), ChangeID: created.ChangeID, ExpectedVersionID: &empty, RequestID: "example-version", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, MergeBaseSHA: input.MergeBaseSHA, Repository: input.Repository, Code: &input.Code, Artifacts: input.Artifacts, PolicyID: input.PolicyID}
				changeCall := func(user, name string, args operatortool.ChangeArguments) *httptest.ResponseRecorder {
					return requestFor(target.service, user, "/internal/v1/keys/call", map[string]any{"name": name, "arguments": args, "connection_id": "version_" + user})
				}
				drainReviewProjection := func(t *testing.T) {
					t.Helper()
					owner := repository.(interface {
						BackfillCurrentChangeReviews(context.Context, tracker.OrganizationID, *eventstore.Content, int) (int, error)
					})
					for range 32 {
						n, err := owner.BackfillCurrentChangeReviews(t.Context(), target.issue.OrganizationID, eventstore.NewContent(target.service.runDetailStore()), corestore.MaxPageSize)
						if err != nil {
							t.Fatal(err)
						}
						if n == 0 {
							return
						}
					}
					t.Fatal("fixture review projections did not complete")
				}
				response := changeCall("owner", operatortool.PublishChangeVersion, publish)
				requireNativeStatus(t, response, http.StatusOK)
				var published operatortool.ChangeResult
				decodeHubResponse(t, response, &published)
				if published.Version == nil || published.Version.ID == "" || published.Version.ChangeID != created.ChangeID || published.Version.Actor.Kind != "human" || published.Version.PolicyID != descriptor.ID {
					t.Fatal("version lost source, policy or human identity")
				}
				replay := changeCall("owner", operatortool.PublishChangeVersion, publish)
				requireNativeStatus(t, replay, http.StatusOK)
				var repeated operatortool.ChangeResult
				decodeHubResponse(t, replay, &repeated)
				if !bytes.Equal(published.Receipt, repeated.Receipt) {
					t.Fatal("version replay changed the canonical receipt")
				}
				versionPath := path + "/" + created.ChangeID + "/versions"
				httpReplay := requestFor(target.service, "owner", versionPath, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: publish.RequestID}, ChangeVersionInput: input})
				requireNativeStatus(t, httpReplay, http.StatusOK)
				var httpVersion tracker.ChangeVersion
				decodeHubResponse(t, httpReplay, &httpVersion)
				if httpVersion.ID != published.Version.ID {
					t.Fatal("HTTP version replay created a second version")
				}

				newReviewCase := func(t *testing.T, key, state string) (tracker.NativeIssue, operatortool.ChangeResult, operatortool.ChangeResult) {
					t.Helper()
					target := target

					response := requestFor(target.service, "owner", linkedPath, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key + "-issue"}, Title: "Example reviewer coordination", Body: workItemRecordBody, State: state})
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &target.issue)
					argsCreate := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(target.issue.WorkItemID), RequestID: key + "-change", Title: "Example reviewer Change"}
					response = changeCall("owner", operatortool.CreateChange, argsCreate)
					requireNativeStatus(t, response, http.StatusOK)
					var created operatortool.ChangeResult
					decodeHubResponse(t, response, &created)
					argsPublish := publish
					argsPublish.ItemID, argsPublish.ChangeID, argsPublish.RequestID = string(target.issue.WorkItemID), created.ChangeID, key+"-version"
					response = changeCall("owner", operatortool.PublishChangeVersion, argsPublish)
					requireNativeStatus(t, response, http.StatusOK)
					var published operatortool.ChangeResult
					decodeHubResponse(t, response, &published)
					return target.issue, created, published
				}
				t.Run("publication and review recheck current authority", func(t *testing.T) {
					issue, created, published := newReviewCase(t, "authority", "Review")
					publishedArgs := publish
					publishedArgs.ItemID, publishedArgs.ChangeID, publishedArgs.RequestID = string(issue.WorkItemID), created.ChangeID, "authority-version"
					reviewArgs := operatortool.ChangeArguments{ProjectID: string(issue.ProjectID), ItemID: string(issue.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: 2, RequestID: "authority-review", Decision: "commented", Body: "Example authority review"}
					requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, reviewArgs), http.StatusOK)
					readSource := func(t *testing.T) eventstore.Change {
						t.Helper()
						var row eventstore.Change
						if err := repository.ReadTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							row, err = r.Change(t.Context(), created.ChangeID)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						return row
					}
					changeAuthority := func(kind string, allowed bool) error {
						return repository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
							if kind == "key" {
								hash := apikey.HashToken(string(issue.OrganizationID) + "owner")
								if !allowed {
									hash = apikey.HashToken("example changed credential")
								}
								return w.RotateAccessCredential(t.Context(), "credential_owner", hash, tokenFingerprint(hash), clock)
							}
							member, err := w.AccessMember(t.Context(), "owner")
							if err != nil {
								return err
							}
							member.Active = allowed
							return w.UpdateAccessMember(t.Context(), member)
						})
					}
					for _, test := range []struct {
						name string
						args operatortool.ChangeArguments
						path string
					}{
						{name: operatortool.PublishChangeVersion, args: publishedArgs, path: linkedPath + "/" + string(issue.WorkItemID) + "/changes/" + created.ChangeID + "/versions"},
						{name: operatortool.ReviewChange, args: reviewArgs, path: linkedPath + "/" + string(issue.WorkItemID) + "/changes/" + created.ChangeID + "/versions/" + published.Version.ID + "/reviews"},
					} {
						for _, kind := range []string{"membership", "key"} {
							for _, replay := range []bool{false, true} {
								t.Run(fmt.Sprintf("%s/%s/writer/replay=%t", test.name, kind, replay), func(t *testing.T) {
									before := readSource(t)
									args := test.args
									if !replay {
										args.RequestID += "-fresh-" + kind
										if test.name == operatortool.PublishChangeVersion {
											args.ExpectedVersionID = &published.Version.ID
										}
									}
									target.service.config.TenantRepository = &projectUpdateBeforeWriteRepository{Repository: repository, before: func() error { return changeAuthority(kind, false) }}
									defer func() {
										target.service.config.TenantRepository = repository
										if err := changeAuthority(kind, true); err != nil {
											t.Error(err)
										}
									}()
									requireNativeStatus(t, changeCall("owner", test.name, args), http.StatusForbidden)
									if after := readSource(t); !reflect.DeepEqual(before, after) {
										t.Fatal("revoked publication or review changed immutable source")
									}
								})
							}
							t.Run(test.name+"/"+kind+"/archive replay", func(t *testing.T) {
								before := readSource(t)
								var receipt eventstore.Receipt
								if err := repository.ReadTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
									var err error
									receipt, err = r.Receipt(t.Context(), "credential_owner", http.MethodPost+" "+test.path, test.args.RequestID)
									return err
								}); err != nil {
									t.Fatal(err)
								}
								original := target.service.config.RunDetails
								probe := &changeReceiptReadProbe{DetailStore: original, key: receipt.PayloadObjectKey, before: func(context.Context) error { return changeAuthority(kind, false) }}
								target.service.config.RunDetails = probe
								defer func() {
									target.service.config.RunDetails = original
									if err := changeAuthority(kind, true); err != nil {
										t.Error(err)
									}
								}()
								response := changeCall("owner", test.name, test.args)
								requireNativeStatus(t, response, http.StatusForbidden)
								if !probe.called || strings.Contains(response.Body.String(), created.ChangeID) {
									t.Fatal("revoked authority exposed a receipt or missed archive interleaving")
								}
								if after := readSource(t); !reflect.DeepEqual(before, after) {
									t.Fatal("revoked receipt replay changed immutable source")
								}
							})
						}
					}
				})
				t.Run("visual content retains only current scoped attachments", func(t *testing.T) {
					issue, created, published := newReviewCase(t, "visual-content", "Review")
					setVisual := func(enabled bool) error {
						return repository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
							project, err := w.ProjectRecord(t.Context(), issue.ProjectID)
							if err != nil {
								return err
							}
							return w.UpdateProjectContent(t.Context(), issue.ProjectID, project.Core.ContentSettingsRevision, enabled)
						})
					}
					if err := setVisual(true); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := setVisual(false); err != nil {
							t.Error(err)
						}
					}()
					ids := make([]string, 6)
					for index := range ids {
						ids[index] = fmt.Sprintf("att_%032x", index+1)
					}
					if err := repository.WriteTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						for index, id := range ids {
							project := string(issue.ProjectID)
							if index == 1 {
								project = "project_foreign"
							}
							metadata := attachment.Metadata{ID: id, ProjectID: project, Uploader: "credential_owner", Name: "example.png", ContentType: "image/png", Size: 10, Width: 1, Height: 1, SHA256: strings.Repeat("a", 64), CreatedAt: clock}
							if index == 2 {
								metadata.DeletedAt = &clock
							}
							if err := w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: issue.OrganizationID, Metadata: metadata, Kind: "attachment", Retention: "ephemeral", ObjectKey: "example/" + id, UncompressedSize: 10, Codec: "identity", SchemaVersion: 1}); err != nil {
								return err
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if _, err := admin.ExecContext(t.Context(), `UPDATE attachments SET pending_content=$3 WHERE organization_id=$1 AND id=$2`, issue.OrganizationID, ids[3], []byte("pending")); err != nil {
						t.Fatal(err)
					}
					otherOrganization, foreignID := tracker.OrganizationID("org_other"), "att_77777777777777777777777777777777"
					if issue.OrganizationID == "org_other" {
						otherOrganization, foreignID = "org_startup", "att_88888888888888888888888888888888"
					}
					if err := repository.WriteTenant(t.Context(), otherOrganization, corestore.Serving, func(w tenantstore.Writer) error {
						metadata := attachment.Metadata{ID: foreignID, ProjectID: string(issue.ProjectID), Uploader: "credential_owner", Name: "example.png", ContentType: "image/png", Size: 10, Width: 1, Height: 1, SHA256: strings.Repeat("a", 64), CreatedAt: clock}
						return w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: otherOrganization, Metadata: metadata, Kind: "attachment", ObjectKey: "example/" + foreignID, UncompressedSize: 10, Codec: "identity", SchemaVersion: 1})
					}); err != nil {
						t.Fatal(err)
					}
					for _, test := range []struct {
						name, body string
						status     int
					}{
						{name: "live", body: "![Example](attachment:" + ids[0] + ")", status: http.StatusOK},
						{name: "foreign project", body: "![Example](attachment:" + ids[1] + ")", status: http.StatusUnprocessableEntity},
						{name: "foreign tenant", body: "![Example](attachment:" + foreignID + ")", status: http.StatusUnprocessableEntity},
						{name: "deleted", body: "![Example](attachment:" + ids[2] + ")", status: http.StatusUnprocessableEntity},
						{name: "pending", body: "![Example](attachment:" + ids[3] + ")", status: http.StatusUnprocessableEntity},
						{name: "missing", body: "![Example](attachment:att_ffffffffffffffffffffffffffffffff)", status: http.StatusUnprocessableEntity},
						{name: "external", body: "![Example](https://example.test/image.png)", status: http.StatusUnprocessableEntity},
					} {
						t.Run(test.name, func(t *testing.T) {
							args := operatortool.ChangeArguments{ProjectID: string(issue.ProjectID), ItemID: string(issue.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: 2, RequestID: "visual-" + strings.ReplaceAll(test.name, " ", "-"), Decision: "commented", Body: test.body}
							requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, args), test.status)
						})
					}
					artifactArgs := publish
					artifactArgs.ItemID, artifactArgs.ChangeID, artifactArgs.RequestID, artifactArgs.ExpectedVersionID = string(issue.WorkItemID), created.ChangeID, "visual-artifact", &published.Version.ID
					artifactArgs.Artifacts = []tracker.ChangeArtifact{{Kind: "artifact", URI: "attachment:" + ids[4], SHA256: strings.Repeat("a", 64), Availability: "available"}}
					for _, test := range []struct{ name, id, digest string }{
						{name: "foreign-project", id: ids[1], digest: strings.Repeat("a", 64)},
						{name: "foreign-tenant", id: foreignID, digest: strings.Repeat("a", 64)},
						{name: "digest-mismatch", id: ids[4], digest: strings.Repeat("b", 64)},
					} {
						args := artifactArgs
						args.RequestID += "-" + test.name
						args.Artifacts = []tracker.ChangeArtifact{{Kind: "artifact", URI: "attachment:" + test.id, SHA256: test.digest, Availability: "available"}}
						requireNativeStatus(t, changeCall("owner", operatortool.PublishChangeVersion, args), http.StatusUnprocessableEntity)
					}
					requireNativeStatus(t, changeCall("owner", operatortool.PublishChangeVersion, artifactArgs), http.StatusOK)
					for _, id := range []string{ids[0], ids[4]} {
						if err := repository.ReadTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							refs, err := r.AttachmentReferences(t.Context(), issue.ProjectID, id, 10)
							if err == nil && (len(refs) != 1 || refs[0].WorkItemID != issue.WorkItemID || !refs[0].RetainWithWorkItem) {
								return fmt.Errorf("attachment %s lost primary-item retention: %+v", id, refs)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					original := target.service.config.RunDetails
					var beforeReceipts int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_commands WHERE organization_id=$1`, issue.OrganizationID).Scan(&beforeReceipts); err != nil {
						t.Fatal(err)
					}
					deleted := false
					target.service.config.RunDetails = &changeContentStoreProbe{DetailStore: original, before: func(ctx context.Context) error {
						if deleted {
							return nil
						}
						deleted = true
						_, err := admin.ExecContext(ctx, `UPDATE attachments SET deleted_at=$3 WHERE organization_id=$1 AND id=$2`, issue.OrganizationID, ids[5], clock)
						return err
					}}
					defer func() { target.service.config.RunDetails = original }()
					args := operatortool.ChangeArguments{ProjectID: string(issue.ProjectID), ItemID: string(issue.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: 3, RequestID: "visual-delete-race", Decision: "commented", Body: "![Example](attachment:" + ids[5] + ")"}
					requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, args), http.StatusUnprocessableEntity)
					if !deleted {
						t.Fatal("attachment deletion did not interleave after preflight")
					}
					var rejectedWrites int
					if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_commands WHERE organization_id=$1`, issue.OrganizationID).Scan(&rejectedWrites); err != nil || rejectedWrites != beforeReceipts {
						t.Fatalf("rejected attachment race committed receipt: count=%d want=%d error=%v", rejectedWrites, beforeReceipts, err)
					}
				})
				t.Run("existing review projection completes through its owner", func(t *testing.T) {
					issue, created, published := newReviewCase(t, "existing-review", "Review")
					args := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(issue.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: 2, RequestID: "existing-review-comment", Decision: "commented", Body: "Example existing review"}
					requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, args), http.StatusOK)
					if _, err := admin.ExecContext(t.Context(), `UPDATE change_requests SET review_summary_sequence=0 WHERE organization_id=$1 AND id=$2`, issue.OrganizationID, created.ChangeID); err != nil {
						t.Fatal(err)
					}
					args.RequestID, args.Decision = "existing-review-approval", "approved"
					requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, args), http.StatusConflict)
					drainReviewProjection(t)
					requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, args), http.StatusOK)
					if err := repository.ReadTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.WorkItem(t.Context(), issue.ProjectID, issue.WorkItemID)
						if err == nil && (stringValue(row.WorkflowStateName) != "Merging" || row.Revision != 2) {
							return errors.New("existing review did not promote after canonical projection recovery")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
				t.Run("one approval preserves another reviewers request", func(t *testing.T) {
					target := target
					issue, created, published := newReviewCase(t, "other-reviewer", "Review")
					target.issue = issue
					content := eventstore.NewContent(cfg.RunDetails)
					for _, principal := range []string{"credential_admin", "credential_owner"} {
						raw, err := json.Marshal(tracker.ChangeReview{ID: payloadNativeID(t.Context(), "review"), VersionID: published.Version.ID, Decision: "changes_requested", Body: "Example outstanding review", Actor: tracker.Actor{Kind: "human", PrincipalID: principal}, CreatedAt: clock})
						if err != nil {
							t.Fatal(err)
						}
						row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: target.issue.OrganizationID, ChangeID: created.ChangeID, VersionID: new(published.Version.ID), Kind: "review", CreatedAt: clock}, raw)
						if err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
							t.Fatal(err)
						}
					}
					args := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(target.issue.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: 2, RequestID: "partial-human-approval", Decision: "approved", Body: "Example approval"}
					drainReviewProjection(t)
					requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, args), http.StatusOK)
					if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
						if err == nil && (stringValue(row.WorkflowStateName) != "Review" || row.Revision != 1) {
							return fmt.Errorf("outstanding other review was bypassed: %s/%d", stringValue(row.WorkflowStateName), row.Revision)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
					args.RequestID, args.Decision = "clear-other-review", "commented"
					requireNativeStatus(t, changeCall("admin", operatortool.ReviewChange, args), http.StatusOK)
					args.RequestID, args.Decision = "approve-other-review", "approved"
					drainReviewProjection(t)
					requireNativeStatus(t, changeCall("admin", operatortool.ReviewChange, args), http.StatusOK)
					if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
						if err == nil && (stringValue(row.WorkflowStateName) != "Merging" || row.Revision != 2) {
							return fmt.Errorf("all current human approvals did not promote: %s/%d", stringValue(row.WorkflowStateName), row.Revision)
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				})
				t.Run("concurrent move invalidates skipped rework", func(t *testing.T) {
					target := target
					issue, created, published := newReviewCase(t, "concurrent-lane", "Todo")
					target.issue = issue
					var version eventstore.CollaborationVersion
					if err := repository.ReadTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						version, err = r.CollaborationVersion(t.Context(), string(issue.WorkItemID), 1)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					original := target.service.config.RunDetails
					moved := false
					probe := &changeReceiptReadProbe{DetailStore: original, key: version.PayloadObjectKey, before: func(ctx context.Context) error {
						if moved {
							return nil
						}
						moved = true
						changed := issue
						changed.State, changed.Revision = "Review", 2
						raw, err := json.Marshal(changed)
						if err != nil {
							return err
						}
						snapshot, err := eventstore.NewContent(original).PrepareCollaborationVersion(ctx, eventstore.CollaborationVersion{OrganizationID: issue.OrganizationID, ProjectID: issue.ProjectID, WorkItemID: issue.WorkItemID, RecordID: string(issue.WorkItemID), Revision: 2}, raw)
						if err != nil {
							return err
						}
						return repository.WriteTenant(ctx, issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
							row, err := w.LockWorkItem(ctx, issue.ProjectID, issue.WorkItemID)
							if err != nil {
								return err
							}
							row.WorkflowStateName = new("Review")
							if _, err := w.UpdateWorkItem(ctx, row); err != nil {
								return err
							}
							_, err = w.CreateCollaborationVersion(ctx, snapshot)
							return err
						})
					}}
					target.service.config.RunDetails = probe
					defer func() { target.service.config.RunDetails = original }()
					args := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(issue.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: 2, RequestID: "concurrent-lane-review", Decision: "changes_requested", Body: "Example requested changes"}
					response := changeCall("owner", operatortool.ReviewChange, args)
					target.service.config.RunDetails = original
					requireNativeStatus(t, response, http.StatusConflict)
					if !moved || !probe.called {
						t.Fatal("concurrent lane mutation did not reach the hydration boundary")
					}
					if err := repository.ReadTenant(t.Context(), issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.Change(t.Context(), created.ChangeID)
						if err != nil {
							return err
						}
						if row.EvidenceSequence != 0 {
							return errors.New("stale skipped-rework decision published review evidence")
						}
						comments, err := r.Comments(t.Context(), issue.ProjectID, issue.WorkItemID, 0, 10)
						if err != nil {
							return err
						}
						if len(comments) != 0 {
							return errors.New("stale skipped-rework decision published instructions")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
				for _, decision := range []string{"commented", "approved", "changes_requested"} {
					drainReviewProjection(t)
					var current eventstore.Change
					if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						var err error
						current, err = r.Change(t.Context(), created.ChangeID)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					review := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(target.issue.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: current.Revision, RequestID: "example-review-" + decision, Decision: decision, Body: "Example human review"}

					if decision == "approved" {
						var lease leasestore.Lease
						if err := repository.WriteTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
							row, err := w.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
							if err != nil {
								return err
							}
							lease, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: "machine_example", SessionID: "workflow_active", LeaseID: "workflow_active", TTL: time.Minute})
							return err
						}); err != nil {
							t.Fatal(err)
						}
						active := review
						active.RequestID += "-live-lease"
						requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, active), http.StatusConflict)
						if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							row, err := r.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
							if err != nil {
								return err
							}
							if stringValue(row.WorkflowStateName) != "Review" || row.Revision != 1 {
								return errors.New("live-lease refusal mutated workflow")
							}
							stored, err := r.Change(t.Context(), created.ChangeID)
							if err != nil {
								return err
							}
							if stored.EvidenceSequence != current.EvidenceSequence {
								return errors.New("live-lease refusal published review evidence")
							}
							if _, err := r.Receipt(t.Context(), "credential_owner", http.MethodPost+" "+versionPath+"/"+published.Version.ID+"/reviews", active.RequestID); !errors.Is(err, corestore.ErrNotFound) {
								return fmt.Errorf("live-lease refusal wrote receipt: %v", err)
							}
							currentLease, err := r.Lease(t.Context(), lease.LeaseID)
							if err != nil {
								return err
							}
							if currentLease.ReleasedAt != nil || currentLease.FencingToken != lease.FencingToken {
								return errors.New("live-lease refusal released or replaced current lease")
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
							_, err := w.Release(t.Context(), leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, "released")
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					response := changeCall("owner", operatortool.ReviewChange, review)
					requireNativeStatus(t, response, http.StatusOK)
					var result operatortool.ChangeResult
					decodeHubResponse(t, response, &result)
					var value tracker.ChangeReview
					if err := json.Unmarshal(result.Receipt, &value); err != nil || value.ID == "" || value.VersionID != published.Version.ID || value.Actor.Kind != "human" || value.Decision != decision {
						t.Fatalf("human review lost canonical identity: %v", err)
					}
					replay := changeCall("owner", operatortool.ReviewChange, review)
					requireNativeStatus(t, replay, http.StatusOK)
					decodeHubResponse(t, replay, &repeated)
					if !bytes.Equal(result.Receipt, repeated.Receipt) {
						t.Fatal("review replay changed canonical evidence")
					}
					httpReplay := requestFor(target.service, "owner", versionPath+"/"+published.Version.ID+"/reviews", tracker.ReviewChange{Mutation: tracker.Mutation{IdempotencyKey: review.RequestID}, ExpectedRevision: tracker.Revision(review.ExpectedRevision), ExpectedVersionID: review.VersionID, Decision: decision, Body: review.Body})
					requireNativeStatus(t, httpReplay, http.StatusOK)
					var httpReview tracker.ChangeReview
					decodeHubResponse(t, httpReplay, &httpReview)
					if httpReview.ID != value.ID {
						t.Fatal("HTTP review replay created duplicate evidence")
					}
					if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
						if err != nil {
							return err
						}
						state, revision := "Review", int64(1)
						if decision == "approved" {
							state, revision = "Merging", 2
						}
						if decision == "changes_requested" {
							state, revision = "Rework", 3
						}
						if stringValue(row.WorkflowStateName) != state || row.Revision != revision {
							return fmt.Errorf("review workflow state=%s revision=%d, want %s/%d", stringValue(row.WorkflowStateName), row.Revision, state, revision)
						}
						if decision != "commented" && (row.DispatchGeneration <= history.DispatchGeneration || row.DispatchRequestedAt == nil) {
							return errors.New("review transition omitted fresh dispatch intent")
						}
						current, err := r.Attempt(t.Context(), history.ID)
						if err != nil {
							return err
						}
						if !reflect.DeepEqual(current, history) {
							return errors.New("human review modified historical successful attempt")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if decision == "changes_requested" && !target.issue.Terminal {
						var rows []corestore.Comment
						var snapshot eventstore.CollaborationVersion
						if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							rows, err = r.Comments(t.Context(), "project_example", target.issue.WorkItemID, 0, 10)
							if err != nil {
								return err
							}
							if len(rows) != 1 {
								return fmt.Errorf("review instructions comments=%d, want1", len(rows))
							}
							snapshot, err = r.CollaborationVersion(t.Context(), rows[0].ID, rows[0].Revision)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						comment, err := target.service.hydrateCommentRecord(t.Context(), rows[0], snapshot)
						if err != nil || comment.Sequence == 0 || comment.Sequence != rows[0].Sequence || !strings.Contains(comment.Body, review.Body) {
							t.Fatalf("persisted review instructions are unreadable or lost sequence/body: %v", err)
						}
					}
					for _, user := range []string{"member", "reporter", "reader"} {
						requireNativeStatus(t, changeCall(user, operatortool.ReviewChange, review), http.StatusForbidden)
					}
				}
				var current eventstore.Change
				if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					current, err = r.Change(t.Context(), created.ChangeID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				secondary := operatortool.ChangeArguments{ProjectID: "project_example", ItemID: string(linked.WorkItemID), ChangeID: created.ChangeID, VersionID: published.Version.ID, ExpectedRevision: current.Revision, RequestID: "secondary-review", Decision: "changes_requested", Body: "Example wrong-target instructions"}
				requireNativeStatus(t, changeCall("owner", operatortool.ReviewChange, secondary), http.StatusForbidden)
			})
			changeRole := func(role string) error {
				return repository.WriteTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), "owner")
					if err != nil {
						return err
					}
					member.Role = role
					return w.UpdateAccessMember(t.Context(), member)
				})
			}
			for _, replay := range []bool{false, true} {
				input := args
				if !replay {
					input.RequestID = "revoked-new-change"
				}
				var before corestore.WorkItem
				if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					before, err = r.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				func() {
					target.service.config.TenantRepository = &projectUpdateBeforeWriteRepository{Repository: repository, before: func() error { return changeRole("reporter") }}
					defer func() {
						target.service.config.TenantRepository = repository
						if err := changeRole("owner"); err != nil {
							t.Error(err)
						}
					}()
					requireNativeStatus(t, invoke("owner", input), http.StatusForbidden)
				}()
				if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					item, err := r.WorkItem(t.Context(), "project_example", target.issue.WorkItemID)
					if err != nil {
						return err
					}
					if item.EventSequence != before.EventSequence {
						return errors.New("revoked Change mutation appended an event")
					}
					if !replay {
						_, err := r.Receipt(t.Context(), "credential_owner", http.MethodPost+" "+path, input.RequestID)
						if !errors.Is(err, corestore.ErrNotFound) {
							return fmt.Errorf("revoked creation left a receipt: %v", err)
						}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			var receipt eventstore.Receipt
			if err := repository.ReadTenant(t.Context(), target.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				receipt, err = r.Receipt(t.Context(), "credential_owner", http.MethodPost+" "+path, args.RequestID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			func() {
				baseStore := target.service.config.RunDetails
				read := &changeReceiptReadProbe{DetailStore: baseStore, key: receipt.PayloadObjectKey, before: func(ctx context.Context) error { return changeRole("reporter") }}
				target.service.config.RunDetails = read
				defer func() {
					target.service.config.RunDetails = baseStore
					if err := changeRole("owner"); err != nil {
						t.Error(err)
					}
				}()
				response := invoke("owner", args)
				requireNativeStatus(t, response, http.StatusForbidden)
				if !read.called || strings.Contains(response.Body.String(), created.ChangeID) {
					t.Fatal("revoked receipt hydration leaked a Change or missed the interleaving")
				}
			}()
			args.Title = "Different content"
			requireNativeStatus(t, invoke("owner", args), http.StatusConflict)
			for _, user := range []string{"reporter", "reader"} {
				requireNativeStatus(t, invoke(user, args), http.StatusForbidden)
			}
			for _, user := range []string{"admin", "member"} {
				input := args
				input.RequestID = "change_" + user
				response := invoke(user, input)
				requireNativeStatus(t, response, http.StatusOK)
				var value operatortool.ChangeResult
				decodeHubResponse(t, response, &value)
				if value.ChangeID == "" || value.ChangeID == created.ChangeID {
					t.Fatal("authorized member did not create a distinct Change")
				}
			}
			foreign := args
			foreign.RequestID = "foreign-linked-change"
			foreign.LinkedIssues = []tracker.NativeWorkItemID{otherMoved.WorkItemID}
			if target.issue.OrganizationID == "org_other" {
				foreign.LinkedIssues = []tracker.NativeWorkItemID{cached["completed"].WorkItemID}
			}
			requireNativeStatus(t, invoke("owner", foreign), http.StatusForbidden)
			missing := args
			missing.RequestID = "missing-linked-change"
			missing.LinkedIssues = []tracker.NativeWorkItemID{"wi_missing"}
			requireNativeStatus(t, invoke("owner", missing), http.StatusForbidden)
			probed := 0
			func() {
				target.service.config.TenantRepository = &changeCreateProbeRepository{Repository: repository, before: func(ctx context.Context) error {
					for _, id := range []tracker.NativeWorkItemID{target.issue.WorkItemID, linked.WorkItemID} {
						tx, err := admin.BeginTx(ctx, nil)
						if err != nil {
							return err
						}
						var row int64
						err = tx.QueryRowContext(ctx, "SELECT id FROM issues WHERE organization_id=$1 AND project_id=$2 AND native_id=$3 FOR NO KEY UPDATE NOWAIT", target.issue.OrganizationID, "project_example", id).Scan(&row)
						rollbackErr := tx.Rollback()
						var locked *pq.Error
						if !errors.As(err, &locked) || locked.Code != "55P03" {
							return fmt.Errorf("linked item was not locked before Change creation: %v", err)
						}
						if rollbackErr != nil {
							return rollbackErr
						}
						probed++
					}
					return nil
				}}
				defer func() { target.service.config.TenantRepository = repository }()
				input := args
				input.RequestID = "locked-linked-change"
				requireNativeStatus(t, invoke("owner", input), http.StatusOK)
			}()
			if probed != 2 {
				t.Fatal("Change creation did not probe both linked row locks")
			}
			args.ProjectID = "project_foreign"
			requireNativeStatus(t, invoke("owner", args), http.StatusForbidden)
		}
	})
	issue := create("owner", "completed", "Done")
	var historic leasestore.Attempt
	var historicLease leasestore.Lease
	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), "project_example", issue.WorkItemID)
		if err != nil {
			return err
		}
		lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: "machine_example", SessionID: "completed_session", LeaseID: "completed_lease", TTL: time.Minute})
		if err != nil {
			return err
		}
		fence := leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}
		if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "completed_attempt", RunID: "completed_run", Fence: fence, Sequence: 1, Status: "running", StartedAt: clock, PayloadObjectKey: "example/start"}); err != nil {
			return err
		}
		historic, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "completed_attempt", RunID: "completed_run", Fence: fence, Sequence: 2, Status: "succeeded", PayloadObjectKey: "example/done"})
		if err != nil {
			return err
		}
		historicLease, err = w.Release(t.Context(), fence, "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	readIntent := func() tenantstore.DispatchState {
		t.Helper()
		var state tenantstore.DispatchState
		if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			state, err = r.(tenantstore.DispatchReader).IssueDispatchState(t.Context(), "project_example", issue.WorkItemID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return state
	}
	t.Run("human intent rollback and required CAS", func(t *testing.T) {
		before := readIntent()
		requireNativeStatus(t, call("owner", move(issue, "", "Rework")), http.StatusUnprocessableEntity)
		withoutRevision := move(issue, "missing-revision", "Rework")
		withoutRevision.ExpectedRevision = 0
		requireNativeStatus(t, call("owner", withoutRevision), http.StatusUnprocessableEntity)
		original := service.config.ConversationDispatch
		called := false
		service.config.ConversationDispatch = conversationDispatchRequestFunc(func(ctx context.Context, w tenantstore.Writer, project tracker.ProjectID, item string, fence leasestore.Fence, at time.Time) error {
			if err := original.RequestDispatch(ctx, w, project, item, fence, at); err != nil {
				return err
			}
			called = true
			return workItemStorageUnavailable()
		})
		defer func() { service.config.ConversationDispatch = original }()
		refused := call("owner", move(issue, "rollback-intent", "Rework"))
		service.config.ConversationDispatch = original
		requireNativeStatus(t, refused, http.StatusServiceUnavailable)
		if !called || !reflect.DeepEqual(before, readIntent()) {
			t.Fatal("failed transition committed or skipped the dispatch intent")
		}
		if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
			row, err := r.WorkItem(t.Context(), "project_example", issue.WorkItemID)
			if err != nil {
				return err
			}
			if row.Revision != int64(issue.Revision) || stringValue(row.WorkflowStateName) != issue.State {
				return fmt.Errorf("failed dispatch intent changed issue state")
			}
			bound, err := (mutation.Metadata{PrincipalID: "credential_owner", OrganizationID: "org_startup", ProjectID: "project_example", Action: operatortool.MoveItem}).Bind("rollback-intent", nil)
			if err != nil {
				return err
			}
			operation := "POST /api/v2/organizations/org_startup/projects/project_example/work-items/" + string(issue.WorkItemID) + "/workflow"
			_, err = r.Receipt(t.Context(), "credential_owner", operation, bound.RetryIdentity)
			if !errors.Is(err, corestore.ErrNotFound) {
				return fmt.Errorf("failed intent receipt: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	})
	args := move(issue, "rework", "Rework")
	originalDispatch := service.config.ConversationDispatch
	defer func() { service.config.ConversationDispatch = originalDispatch }()
	resolved := 0
	service.config.ConversationDispatch = conversationDispatchLockProbe{delegate: originalDispatch, probe: func() error {
		resolved++
		probe, err := admin.BeginTx(t.Context(), nil)
		if err != nil {
			return err
		}
		var id int64
		lockErr := probe.QueryRowContext(t.Context(), `SELECT id FROM issues WHERE organization_id=$1 AND native_id=$2 FOR NO KEY UPDATE NOWAIT`, "org_startup", issue.WorkItemID).Scan(&id)
		if err := probe.Rollback(); err != nil {
			return err
		}
		var refused *pq.Error
		if !errors.As(lockErr, &refused) || refused.Code != "55P03" {
			return fmt.Errorf("human issue was not locked during dispatch fence resolution: %w", lockErr)
		}
		return nil
	}}
	changed := decode(call("owner", args))
	service.config.ConversationDispatch = originalDispatch
	if resolved != 1 {
		t.Fatalf("human intent fence resolutions=%d want 1", resolved)
	}
	if changed.State != "Rework" || changed.Revision != issue.Revision+1 || changed.Title != issue.Title || changed.Body != issue.Body || changed.Type != issue.Type || !reflect.DeepEqual(changed.Labels, issue.Labels) || !reflect.DeepEqual(changed.Assignees, issue.Assignees) || !reflect.DeepEqual(changed.Priority, issue.Priority) || !reflect.DeepEqual(changed.Actor, issue.Actor) || !changed.CreatedAt.Equal(issue.CreatedAt) {
		t.Fatal("MoveItem lost workflow or content fidelity")
	}
	intent := readIntent()
	if intent.Revision != int64(changed.Revision) || intent.Revision <= historic.WorkItemRevision || intent.Generation <= historic.DispatchGeneration || intent.RequestedAt == nil {
		t.Fatalf("human Rework did not supersede the consumed success: revision=%d generation=%d requested=%t", intent.Revision, intent.Generation, intent.RequestedAt != nil)
	}
	archiveBytes := func() int64 {
		t.Helper()
		var value int64
		if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error { var err error; value, err = r.ArchiveBytes(t.Context()); return err }); err != nil {
			t.Fatal(err)
		}
		return value
	}
	comment := func(target *Service, user, identifier, requestID string) *httptest.ResponseRecorder {
		return requestFor(target, user, "/internal/v1/keys/call", map[string]any{"name": operatortool.AddComment, "connection_id": "comment_" + user, "arguments": map[string]any{"project_id": "project_example", "identifier": identifier, "request_id": requestID, "body": "Example comment"}})
	}
	t.Run("signed comments preserve tenant authority and replay", func(t *testing.T) {
		for _, test := range []struct {
			service *Service
			user    string
			issue   tracker.NativeIssue
		}{{service, "owner", changed}, {service, "reporter", cached["reporter_own"]}, {other, "owner", otherMoved}} {
			identifier := fmt.Sprint(test.issue.Number)
			response := comment(test.service, test.user, identifier, "example-comment-"+test.user)
			requireNativeStatus(t, response, http.StatusOK)
			var result struct {
				Data tracker.NativeComment `json:"data"`
			}
			decodeHubResponse(t, response, &result)
			if result.Data.ID == "" || result.Data.WorkItemID != test.issue.WorkItemID || result.Data.OrganizationID != test.issue.OrganizationID || result.Data.Body != "Example comment" {
				t.Fatal("comment lost canonical identity, tenant or body")
			}
			commentArchiveBytes := func() int64 {
				t.Helper()
				var value int64
				if err := test.service.config.TenantRepository.ReadTenant(t.Context(), test.issue.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					value, err = r.ArchiveBytes(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return value
			}
			before := commentArchiveBytes()
			replay := comment(test.service, test.user, identifier, "example-comment-"+test.user)
			requireNativeStatus(t, replay, http.StatusOK)
			var repeated struct {
				Data tracker.NativeComment `json:"data"`
			}
			decodeHubResponse(t, replay, &repeated)
			if !reflect.DeepEqual(result.Data, repeated.Data) || commentArchiveBytes() != before {
				t.Fatal("comment replay changed the canonical result or archive accounting")
			}
		}
		requireNativeStatus(t, comment(service, "reader", string(changed.WorkItemID), "read-key-comment"), http.StatusForbidden)
		requireNativeStatus(t, comment(service, "reporter", string(changed.WorkItemID), "foreign-reporter-comment"), http.StatusForbidden)
		requireNativeStatus(t, comment(service, "owner", string(otherMoved.WorkItemID), "foreign-tenant-comment"), http.StatusForbidden)
	})
	for _, replay := range []bool{false, true} {
		t.Run(fmt.Sprintf("comment ownership changes before write replay=%v", replay), func(t *testing.T) {
			item := cached["reporter_fresh_race"]
			if replay {
				item = cached["reporter_replay_race"]
				requireNativeStatus(t, comment(service, "reporter", string(item.WorkItemID), "ownership-race"), http.StatusOK)
			}
			var before corestore.WorkItem
			if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				before, err = r.WorkItem(t.Context(), "project_example", item.WorkItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			intercepted := &projectUpdateBeforeWriteRepository{Repository: repository}
			intercepted.before = func() error {
				return repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockWorkItemProject(t.Context(), "project_example"); err != nil {
						return err
					}
					row, err := w.WorkItem(t.Context(), "project_example", item.WorkItemID)
					if err != nil {
						return err
					}
					row.ActorPrincipalID = new("credential_owner")
					_, err = w.UpdateWorkItem(t.Context(), row)
					return err
				})
			}
			service.config.TenantRepository = intercepted
			defer func() { service.config.TenantRepository = repository }()
			denied := comment(service, "reporter", string(item.WorkItemID), "ownership-race")
			requireNativeStatus(t, denied, http.StatusForbidden)
			if strings.Contains(denied.Body.String(), "Example comment") {
				t.Fatal("former owner received private comment data")
			}
			if intercepted.before != nil {
				t.Fatal("ownership did not change before the write")
			}
			if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.WorkItem(t.Context(), "project_example", item.WorkItemID)
				if err == nil && row.EventSequence != before.EventSequence {
					t.Fatal("denied comment appended an event")
				}
				if err != nil {
					return err
				}
				if !replay {
					operation := "POST /api/v2/organizations/org_startup/projects/project_example/work-items/" + string(item.WorkItemID) + "/comments"
					bound, err := (mutation.Metadata{PrincipalID: "credential_reporter", OrganizationID: "org_startup", ProjectID: "project_example", Action: operatortool.AddComment}).Bind("ownership-race", nil)
					if err != nil {
						return err
					}
					_, err = r.Receipt(t.Context(), "credential_reporter", operation, bound.RetryIdentity)
					if !errors.Is(err, corestore.ErrNotFound) {
						return fmt.Errorf("denied comment receipt: %v", err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
	beforeReplay := archiveBytes()
	replayed := decode(call("owner", args))
	if archiveBytes() != beforeReplay {
		t.Fatal("receipt replay charged archive bytes again")
	}
	conflicting := args
	conflicting.TargetState = "Todo"
	requireNativeStatus(t, call("owner", conflicting), http.StatusConflict)
	if !reflect.DeepEqual(changed, replayed) {
		t.Fatal("MoveItem did not replay the canonical result")
	}
	if !reflect.DeepEqual(intent, readIntent()) {
		t.Fatal("replay created another dispatch intent")
	}
	if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.WorkItem(t.Context(), "project_example", issue.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(changed.Revision) || stringValue(row.WorkflowStateName) != "Rework" {
			return fmt.Errorf("current state differs")
		}
		version, err := r.CollaborationVersion(t.Context(), string(issue.WorkItemID), row.Revision)
		if err != nil {
			return err
		}
		raw, err := eventstore.NewContent(cfg.RunDetails).CollaborationVersion(t.Context(), version)
		if err != nil {
			return err
		}
		var stored tracker.NativeIssue
		if err := json.Unmarshal(raw, &stored); err != nil {
			return err
		}
		if stored.State != "Rework" || stored.Revision != changed.Revision || stored.Body != issue.Body || stored.Type != issue.Type || !reflect.DeepEqual(stored.Labels, issue.Labels) || !reflect.DeepEqual(stored.Assignees, issue.Assignees) || !reflect.DeepEqual(stored.Priority, issue.Priority) {
			return fmt.Errorf("collaboration version differs")
		}
		events, err := r.Events(t.Context(), "project_example", issue.WorkItemID, 0, 100)
		if err != nil {
			return err
		}
		count := 0
		for _, event := range events {
			if event.Type == "workflow.transitioned" && event.EventRevision == row.Revision && event.FromState == "Done" && event.ToState == "Rework" && event.ActorKind == "human" && event.ActorPrincipalID == "credential_owner" {
				count++
			}
		}
		if count != 1 {
			return fmt.Errorf("transition events=%d", count)
		}
		attempt, err := r.LatestAttempt(t.Context(), row.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(attempt, historic) {
			return fmt.Errorf("historical attempt changed")
		}
		lease, err := r.Lease(t.Context(), historicLease.LeaseID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(lease, historicLease) {
			return fmt.Errorf("historical lease changed")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"admin", "member"} {
		t.Run("execute_"+user, func(t *testing.T) {
			item := create("owner", user, "Done")
			got := decode(call(user, move(item, "rework_"+user, "Rework")))
			if got.State != "Rework" || got.Revision != item.Revision+1 {
				t.Fatal("authorized role did not move issue")
			}
		})
	}
	for _, test := range []struct {
		name, user, target string
		revision           int64
		status             int
	}{
		{"read_only", "reader", "Todo", int64(changed.Revision), http.StatusForbidden},
		{"stale", "owner", "Todo", int64(issue.Revision), http.StatusConflict},
		{"done_refused", "owner", "Done", int64(changed.Revision), http.StatusUnprocessableEntity},
		{"blocked_refused", "owner", "Blocked", int64(changed.Revision), http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := move(changed, test.name, test.target)
			input.ExpectedRevision = test.revision
			requireNativeStatus(t, call(test.user, input), test.status)
		})
	}
	if !reflect.DeepEqual(intent, readIntent()) {
		t.Fatal("refused mutation changed the current dispatch intent")
	}
	own := create("reporter", "reporter_own", "Triage")
	requireNativeStatus(t, call("reporter", move(own, "reporter_not_cancel", "Todo")), http.StatusForbidden)
	cancel := move(own, "reporter_cancel", "Cancelled")
	cancelled := decode(call("reporter", cancel))
	if cancelled.State != "Cancelled" {
		t.Fatal("reporter own cancellation failed")
	}
	decode(call("reporter", cancel))
	requireNativeStatus(t, call("reporter", move(changed, "reporter_foreign", "Cancelled")), http.StatusForbidden)
	for _, test := range []string{"key", "member", "downgrade", "reporter_target", "grant", "blocked_project", "selected_project"} {
		t.Run("revoked_"+test, func(t *testing.T) {
			switch test {
			case "key":
				revoked = true
				defer func() { revoked = false }()
			case "blocked_project":
				blocked = []string{"project_example"}
				defer func() { blocked = nil }()
			case "selected_project":
				projectAccess = "selected"
				defer func() { projectAccess = "all" }()
			default:
				if test == "reporter_target" {
					roles["owner"] = "reporter"
					defer func() { roles["owner"] = "owner" }()
				}
				if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), "owner")
					if err != nil {
						return err
					}
					if test == "member" {
						member.Active = false
					}
					if test == "grant" {
						return w.DeleteAccessProjectGrant(t.Context(), "owner", "project_example")
					}
					if test == "reporter_target" {
						member.Role = "reporter"
					}
					if test == "downgrade" {
						member.Role = "viewer"
					}
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
						member, err := w.AccessMember(t.Context(), "owner")
						if err != nil {
							return err
						}
						member.Active, member.Role = true, "owner"
						if test == "grant" {
							return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "owner", ProjectID: "project_example", CanWrite: true})
						}
						return w.UpdateAccessMember(t.Context(), member)
					}); err != nil {
						t.Error(err)
					}
				}()
			}
			input := args
			if test == "selected_project" {
				input.ProjectID = "project_foreign"
			}
			response := call("owner", input)
			if response.Code != http.StatusForbidden && response.Code != http.StatusUnauthorized {
				t.Fatalf("revocation status=%d", response.Code)
			}
			input.RequestID = "fresh_" + test
			input.ExpectedRevision = int64(changed.Revision)
			input.TargetState = "Todo"
			fresh := call("owner", input)
			if fresh.Code != http.StatusForbidden && fresh.Code != http.StatusUnauthorized {
				t.Fatalf("fresh revocation status=%d", fresh.Code)
			}
			if test != "reporter_target" {
				commentArguments := map[string]any{"project_id": input.ProjectID, "identifier": string(changed.WorkItemID), "request_id": "revoked-comment-" + test, "body": "Example denied comment"}
				denied := request("owner", "/internal/v1/keys/call", map[string]any{"name": operatortool.AddComment, "connection_id": "comment_owner", "arguments": commentArguments})
				if denied.Code != http.StatusForbidden && denied.Code != http.StatusUnauthorized {
					t.Fatalf("revoked comment status=%d", denied.Code)
				}
			}

		})
	}
	var active leasestore.Lease
	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		row, err := w.WorkItem(t.Context(), "project_example", issue.WorkItemID)
		if err != nil {
			return err
		}
		active, err = w.Claim(t.Context(), leasestore.Claim{IssueID: row.ID, ExpectedRevision: row.Revision, MachineID: "machine_example", SessionID: "new_session", LeaseID: "new_lease", TTL: time.Minute})
		if err != nil {
			return err
		}
		if active.WorkItemRevision != int64(changed.Revision) || active.FencingToken <= historic.FencingToken {
			return fmt.Errorf("new claim did not use current revision and fence")
		}
		_, err = w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: "new_attempt", RunID: "new_run", Fence: leasestore.Fence{LeaseID: active.LeaseID, FencingToken: active.FencingToken}, Sequence: 1, Status: "running", StartedAt: clock, PayloadObjectKey: "example/new"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		target string
		status int
	}{{"Todo", http.StatusConflict}, {"Cancelled", http.StatusForbidden}} {
		requireNativeStatus(t, call("owner", move(changed, "active_"+test.target, test.target)), test.status)
	}
	if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(t.Context(), active.LeaseID)
		if err == nil && lease.ReleasedAt != nil {
			return fmt.Errorf("refusal released active lease")
		}
		if err != nil {
			return err
		}
		attempt, err := r.LatestAttempt(t.Context(), lease.IssueID)
		if err != nil {
			return err
		}
		if attempt.Status != "running" || attempt.Sequence != 1 {
			return fmt.Errorf("missing run evidence cancellation changed the active attempt")
		}
		row, err := r.WorkItem(t.Context(), "project_example", changed.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(changed.Revision) || stringValue(row.WorkflowStateName) != changed.State {
			return fmt.Errorf("refused transition changed the issue")
		}
		bound, err := (mutation.Metadata{PrincipalID: "credential_owner", OrganizationID: "org_startup", ProjectID: "project_example", Action: operatortool.MoveItem}).Bind("active_Cancelled", nil)
		if err != nil {
			return err
		}
		_, err = r.Receipt(t.Context(), "credential_owner", "POST /api/v2/organizations/org_startup/projects/project_example/work-items/"+string(issue.WorkItemID)+"/workflow", bound.RetryIdentity)
		if !errors.Is(err, corestore.ErrNotFound) {
			return fmt.Errorf("missing run evidence cancellation receipt: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(t.Context(), `UPDATE leases SET acquired_at=acquired_at-interval '3 minutes',renewed_at=renewed_at-interval '2 minutes',expires_at=expires_at-interval '2 minutes' WHERE organization_id=$1 AND lease_id=$2`, "org_startup", active.LeaseID); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Minute)
	newer := decode(call("owner", move(changed, "expired_lease_cancel", "Cancelled")))
	if newer.State != "Cancelled" || newer.Revision != changed.Revision+1 {
		t.Fatal("expired lease cancellation failed")
	}
	if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		lease, err := r.Lease(t.Context(), active.LeaseID)
		if err != nil {
			return err
		}
		attempt, err := r.Attempt(t.Context(), "new_attempt")
		if err != nil {
			return err
		}
		if lease.ReleasedAt == nil || lease.ReleaseReason != "expired" || attempt.Status != "cancelled" {
			return fmt.Errorf("expired cancellation did not use the fenced owner")
		}
		old, err := r.Attempt(t.Context(), historic.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(old, historic) {
			return fmt.Errorf("cancellation changed historical attempt")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var receipt eventstore.Receipt
	bound, err := (mutation.Metadata{PrincipalID: "credential_owner", OrganizationID: "org_startup", ProjectID: "project_example", Action: operatortool.MoveItem}).Bind(args.RequestID, args)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(t.Context(), "credential_owner", "POST /api/v2/organizations/org_startup/projects/project_example/work-items/"+string(issue.WorkItemID)+"/workflow", bound.RetryIdentity)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !receipt.CreatedAt.Equal(initialClock) || receipt.ResponseReplayUntilNs != receipt.CreatedAt.Add(receiptstore.ReplayRetention).UnixNano() {
		t.Fatal("receipt retention not canonical")
	}
	clock = time.Unix(0, receipt.ResponseReplayUntilNs-1).UTC()
	decode(call("owner", args))
	clock = clock.Add(time.Nanosecond)
	beforeExpiry := archiveBytes()
	response := call("owner", args)
	requireNativeStatus(t, response, http.StatusGone)
	if archiveBytes() > beforeExpiry {
		t.Fatal("expiry increased archive charge")
	}
	requireNativeStatus(t, call("owner", args), http.StatusGone)
	if !strings.Contains(response.Body.String(), "idempotency_expired") {
		t.Fatal("expired receipt lost canonical error")
	}
	if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.Receipt(t.Context(), receipt.ActorID, receipt.Operation, receipt.CommandKey)
		if err != nil {
			return err
		}
		if !current.ResponseExpired || current.RequestHash != receipt.RequestHash || current.PayloadSHA256 != receipt.PayloadSHA256 {
			return fmt.Errorf("expired receipt was not tombstoned")
		}
		row, err := r.WorkItem(t.Context(), "project_example", issue.WorkItemID)
		if err != nil {
			return err
		}
		if row.Revision != int64(newer.Revision) || stringValue(row.WorkflowStateName) != "Cancelled" {
			return fmt.Errorf("expired command changed newer state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func moveItemSelectedProjects(access string) []string {
	if access == "selected" {
		return []string{"project_example"}
	}
	return nil
}

func TestWorkflowSafeError(t *testing.T) {
	for _, test := range []struct {
		name        string
		input, want error
	}{
		{"provider unavailable", &auth.HostedIdentityError{Reason: auth.HostedReasonProviderUnavailable}, operatortool.ErrProviderUnavailable},
		{"membership denied", auth.ErrHostedIdentity, operatortool.ErrAccessDenied},
		{"receipt expired", commandResponseExpired(), receiptstore.ErrExpired},
		{"storage failure", errors.New("example storage unavailable"), operatortool.ErrServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := workflowSafeError(test.input); !errors.Is(got, test.want) {
				t.Fatalf("classification=%v, want %v", got, test.want)
			}
		})
	}
}

type changeReceiptReadProbe struct {
	attachment.DetailStore
	key    string
	before func(context.Context) error
	called bool
}

func (p *changeReceiptReadProbe) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Key == p.key {
		p.called = true
		if err := p.before(ctx); err != nil {
			return nil, err
		}
	}
	return p.DetailStore.ReadDetail(ctx, organization, ref)
}

type changeCreateProbeRepository struct {
	tenantstore.Repository
	before func(context.Context) error
}

type changeContentStoreProbe struct {
	attachment.DetailStore
	before func(context.Context) error
}

func (p *changeContentStoreProbe) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	if err := p.before(ctx); err != nil {
		return attachment.ArchiveReference{}, err
	}
	return p.DetailStore.StoreDetail(ctx, organization, raw)
}

func (r *changeCreateProbeRepository) WriteTenant(ctx context.Context, org tracker.OrganizationID, workload corestore.Workload, write func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, org, workload, func(w tenantstore.Writer) error { return write(changeCreateProbeWriter{Writer: w, before: r.before}) })
}

type changeCreateProbeWriter struct {
	tenantstore.Writer
	before func(context.Context) error
}

func (w changeCreateProbeWriter) CreateChange(ctx context.Context, value eventstore.Change) (eventstore.Change, error) {
	if err := w.before(ctx); err != nil {
		return eventstore.Change{}, err
	}
	return w.Writer.CreateChange(ctx, value)
}
