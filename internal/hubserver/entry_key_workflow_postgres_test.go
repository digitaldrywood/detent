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

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
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
	states = append(states, tracker.NativeState{Name: "Triage", Transitions: []string{"Todo", "Cancelled"}})
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
			return w.ApproveProjectPolicy(t.Context(), prepared, "")
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
	for _, input := range []struct{ user, id, state string }{{"owner", "completed", "Done"}, {"owner", "admin", "Done"}, {"owner", "member", "Done"}, {"reporter", "reporter_own", "Triage"}} {
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
	issue := create("owner", "completed", "Done")
	var historic leasestore.Attempt
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
		_, err = w.Release(t.Context(), fence, "completed")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	args := move(issue, "rework", "Rework")
	changed := decode(call("owner", args))
	if changed.State != "Rework" || changed.Revision != issue.Revision+1 || changed.Title != issue.Title || changed.Body != issue.Body || changed.Type != issue.Type || !reflect.DeepEqual(changed.Labels, issue.Labels) || !reflect.DeepEqual(changed.Assignees, issue.Assignees) || !reflect.DeepEqual(changed.Priority, issue.Priority) || !reflect.DeepEqual(changed.Actor, issue.Actor) || !changed.CreatedAt.Equal(issue.CreatedAt) {
		t.Fatal("MoveItem lost workflow or content fidelity")
	}
	archiveBytes := func() int64 {
		t.Helper()
		var value int64
		if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error { var err error; value, err = r.ArchiveBytes(t.Context()); return err }); err != nil {
			t.Fatal(err)
		}
		return value
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
		attempt, err := r.Attempt(t.Context(), historic.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(attempt, historic) {
			return fmt.Errorf("historical attempt changed")
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
		_, err = r.Receipt(t.Context(), "credential_owner", "POST /api/v2/organizations/org_startup/projects/project_example/work-items/"+string(issue.WorkItemID)+"/workflow", "active_Cancelled")
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
