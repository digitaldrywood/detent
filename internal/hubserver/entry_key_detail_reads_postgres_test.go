package hubserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresEntryKeyDetailReads(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	key := hostedSharedKey(38)
	cfg := startupConfig(connection)
	plans := pilotHostedPlans()
	cfg.Hosted.Plans = &plans
	cfg.SecretKeys = secretTestKeys(t, "1", "1")
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	currentOrganization := "org_startup"
	selected := false
	selectedProjects := []string{"hidden_project"}
	entryRevoked := false
	currentRole := "owner"
	currentPermission := apikey.ScopeRead
	var currentBlocked []string
	cfg.Hosted.SharedEntry.ResolveKey = func(_ context.Context, origin entryauthority.Origin) (cloudassert.Claims, error) {
		if entryRevoked {
			return cloudassert.Claims{}, operatortool.ErrAccessDenied
		}
		selection := apikey.ProjectContext{Access: "all"}
		if selected {
			selection = apikey.ProjectContext{Access: "selected", Projects: selectedProjects}
		}
		return cloudassert.Claims{Issuer: origin.Issuer, Generation: origin.Generation, Audience: currentOrganization, ProviderOrganization: "provider_" + currentOrganization, Subject: origin.Subject, Role: currentRole, Kind: cloudassert.KindKey, ExpiresAt: time.Now().Add(time.Minute), Key: &apikey.KeyAuthority{ID: origin.KeyID, CredentialBinding: origin.CredentialBinding, Kind: "personal", Permission: currentPermission, BlockedProjects: currentBlocked, ProjectContext: selection}}, nil
	}
	store, err := openDatabase(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		seedStartupOrganization(t, store.postgres, org)
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			envelope, err := cfg.SecretKeys.Seal(bytes.Repeat([]byte{0x35}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
				return err
			}
			for _, user := range []string{"first", "second"} {
				id := "credential_" + user
				if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: id, Name: user, Hash: apikey.HashToken(string(org) + user), Scope: "operator", NativeOnly: true, CreatedAt: now, UpdatedAt: now, UserID: user, MembershipID: "membership_" + user, KeyScope: "admin", ProjectAccess: "all", Kind: "personal"}); err != nil {
					return err
				}
				if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: user, Email: user + "@example.test", MembershipID: "membership_" + user, Role: "owner", Active: true, PrincipalID: id, CreatedAt: now, UpdatedAt: now}); err != nil {
					return err
				}
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "hidden_project", Name: "Private example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			project := tracker.ProjectID("project_" + string(org))
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: project, Name: "Example", Slug: "example", Profile: "native"}, States: nativeFixtureStates()}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "credential_first", ProjectID: project}); err != nil {
				return err
			}
			return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "first", ProjectID: project, CanWrite: true, ManageRunner: false})
		}); err != nil {
			t.Fatal(err)
		}
	}

	content := eventstore.NewContent(cfg.RunDetails)
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		project := tracker.ProjectID("project_" + string(org))
		descriptor := hubTestPolicy()
		descriptor.SourceDigest = policy.Digest([]byte(org))
		descriptor = descriptor.WithID()
		prepared, err := content.PrepareProjectPolicy(t.Context(), org, descriptor)
		if err != nil {
			t.Fatal(err)
		}
		prepared.ProjectID, prepared.ApprovedBy, prepared.ApprovedAt = project, "credential_first", now
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.ApproveProjectPolicy(t.Context(), prepared, "")
		}); err != nil {
			t.Fatal(err)
		}
		for index, item := range []string{"wi_own", "wi_foreign"} {
			actor := "credential_first"
			if index == 1 {
				actor = "credential_second"
			}
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: org, ProjectID: project, WorkItemID: tracker.NativeWorkItemID(item), Profile: "native", Revision: 1, Number: index + 1}, Title: "Example issue", State: "Done", Archived: index == 1, CreatedAt: now, UpdatedAt: now, Actor: tracker.Actor{Kind: "human", PrincipalID: actor}}
			raw, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			version, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, RecordID: item, Revision: 1}, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: org, ProjectID: project, NativeID: issue.WorkItemID, Number: int64(index + 1), Revision: 1, EventSequence: 3, NativeCreatedAt: now, NativeUpdatedAt: now, Title: issue.Title, WorkflowStateName: &issue.State, Archived: issue.Archived, ActorKind: &issue.Actor.Kind, ActorPrincipalID: &actor}); err != nil {
					return err
				}
				_, err := w.CreateCollaborationVersion(t.Context(), version)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for n := 1; n <= 3; n++ {
				comment := tracker.NativeComment{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, ID: fmt.Sprintf("c_%s_%d", item, n), Revision: 1, Sequence: int64(n), Body: fmt.Sprintf("%s comment %d", org, n), Actor: issue.Actor, CreatedAt: now, UpdatedAt: now}
				raw, err := json.Marshal(comment)
				if err != nil {
					t.Fatal(err)
				}
				cv, err := content.PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, RecordID: comment.ID, Revision: 1}, raw)
				if err != nil {
					t.Fatal(err)
				}
				change := tracker.ChangeRequest{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, ID: fmt.Sprintf("change_%s_%d", item, n), Revision: 1, Title: "Example Change", Body: string(org) + " preserved Change", CreatedAt: now, UpdatedAt: now}
				cr, err := json.Marshal(change)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareChange(t.Context(), org, cr)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.CreateComment(t.Context(), corestore.Comment{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, ID: comment.ID, Revision: 1, Sequence: comment.Sequence, BodyObjectKey: cv.PayloadObjectKey, ActorKind: issue.Actor.Kind, ActorPrincipalID: actor, CreatedAt: now, UpdatedAt: now}); err != nil {
						return err
					}
					if _, err := w.CreateCollaborationVersion(t.Context(), cv); err != nil {
						return err
					}
					if _, err := w.CreateChange(t.Context(), row); err != nil {
						return err
					}
					return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: project, WorkItemID: issue.WorkItemID, ChangeID: change.ID, CreatedAt: now})
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: org, ProjectID: tracker.ProjectID("project_" + string(org)), WorkItemID: "wi_foreign", ChangeID: "change_wi_own_1", CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
		for _, item := range []string{"wi_own", "wi_foreign"} {
			for _, id := range []string{item, "c_" + item + "_1", "c_" + item + "_2", "c_" + item + "_3"} {
				kind := "comment"
				if id == item {
					kind = "issue"
				}
				document, err := searchstore.NewContent(cfg.RunDetails).Prepare(t.Context(), searchstore.Document{OrganizationID: org, ProjectID: tracker.ProjectID("project_" + string(org)), WorkItemID: tracker.NativeWorkItemID(item), ID: kind + ":" + id, SourceID: id, Kind: kind, Scope: "issue", Revision: 1, UpdatedAt: now}, searchstore.Text{Title: "Example issue"})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.postgres.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.PutSearch(t.Context(), document, 0) }); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	firstService := service
	t.Cleanup(func() {
		if err := firstService.Close(); err != nil {
			t.Error(err)
		}
	})
	request := func(path, user, connection string, arguments any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		t.Helper()
		body, err := json.Marshal(arguments)
		if err != nil {
			t.Fatal(err)
		}
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		claims := cloudassert.Claims{Issuer: "entry", Audience: currentOrganization, ProviderOrganization: "provider_" + currentOrganization, Subject: user, Role: "owner", Generation: 1, Kind: cloudassert.KindKey, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: time.Now(), ExpiresAt: time.Now().Add(20 * time.Second), ID: id, Key: &apikey.KeyAuthority{ID: "key_" + user, CredentialBinding: strings.Repeat("a", 64), Kind: "personal", Permission: currentPermission, ProjectContext: apikey.ProjectContext{Access: "all"}}}
		if mutate != nil {
			mutate(&claims)
		}
		assertion, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(cloudassert.Header, assertion)
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, req)
		return response
	}
	call := func(name string, args any, mutate func(*cloudassert.Claims)) *httptest.ResponseRecorder {
		return request("/internal/v1/keys/call", "first", "detail_read", map[string]any{"name": name, "arguments": args, "connection_id": "detail_read"}, mutate)
	}

	otherConfig := cfg
	otherHosted := *cfg.Hosted
	otherEntry := *cfg.Hosted.SharedEntry
	otherHosted.SharedEntry = &otherEntry
	otherConfig.Hosted = &otherHosted
	otherConfig.Hosted.OrganizationID = "org_other"
	otherConfig.Hosted.WorkOSOrganizationID = "provider_org_other"
	otherService, err := Open(t.Context(), otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := otherService.Close(); err != nil {
			t.Error(err)
		}
	})
	snapshot := func() []byte {
		t.Helper()
		var rows []any
		for _, org := range []tracker.OrganizationID{"org_startup", "org_other"} {
			if err := service.config.TenantRepository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				for _, id := range []tracker.NativeWorkItemID{"wi_own", "wi_foreign"} {
					project := tracker.ProjectID("project_" + string(org))
					work, err := r.WorkItem(t.Context(), project, id)
					if err != nil {
						return err
					}
					comments, err := r.Comments(t.Context(), project, id, 0, 200)
					if err != nil {
						return err
					}
					changes, err := r.Changes(t.Context(), project, id, "", 200)
					if err != nil {
						return err
					}
					rows = append(rows, work, comments, changes)
					for _, comment := range comments {
						version, err := r.CollaborationVersion(t.Context(), comment.ID, comment.Revision)
						if err != nil {
							return err
						}
						rows = append(rows, version)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := json.Marshal(rows)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	before := snapshot()
	for _, target := range []struct {
		org     string
		service *Service
	}{{"org_startup", firstService}, {"org_other", otherService}} {
		service, currentOrganization = target.service, target.org
		for _, name := range []string{operatortool.HealthFindings, operatortool.WorkConfig, operatortool.WorkComments, operatortool.WorkHistory, operatortool.WorkVersion, operatortool.WorkRelationships, operatortool.WorkRuns, operatortool.WorkReferences, operatortool.WorkExport, operatortool.WorkBulkExport, operatortool.BoardActivity} {
			t.Run(target.org+"/"+name, func(t *testing.T) {
				args := map[string]any{"project_id": "project_" + currentOrganization}
				if name != operatortool.HealthFindings && name != operatortool.WorkConfig && name != operatortool.WorkBulkExport {
					args["reference"] = "wi_own"
				}
				if name == operatortool.WorkVersion {
					args["revision"] = 1
				}
				catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
				requireNativeStatus(t, catalog, http.StatusOK)
				var definitions []operatortool.Definition
				decodeHubResponse(t, catalog, &definitions)
				if !entryKeyHasDefinition(definitions, name) {
					t.Errorf("%s missing from signed catalog", name)
				}
				response := call(name, args, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("call status=%d body=%s", response.Code, response.Body)
				}
				other := "org_other"
				if currentOrganization == other {
					other = "org_startup"
				}
				if strings.Contains(response.Body.String(), other+" comment") {
					t.Fatal("read leaked another tenant's content")
				}
				args["project_id"] = "project_" + other
				if response := call(name, args, nil); response.Code == http.StatusOK {
					t.Fatal("cross tenant read accepted")
				}
			})
		}
		t.Run(target.org+"/get_project_policy", func(t *testing.T) {
			args := map[string]any{"project_id": "project_" + currentOrganization}
			for _, role := range []string{"owner", "admin"} {
				currentRole, currentPermission = role, apikey.ScopeAdmin
				catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
				requireNativeStatus(t, catalog, http.StatusOK)
				var definitions []operatortool.Definition
				decodeHubResponse(t, catalog, &definitions)
				definition, found := entryKeyDefinition(definitions, "get_project_policy")
				if !found || !definition.Annotations.ReadOnly {
					t.Errorf("%s policy read missing from signed catalog", role)
				}
				response := call("get_project_policy", args, nil)
				if response.Code != http.StatusOK {
					t.Errorf("%s policy call status=%d body=%s", role, response.Code, response.Body.String())
					continue
				}
				var result struct {
					OrganizationID string            `json:"organization_id"`
					ProjectID      string            `json:"project_id"`
					Data           projectPolicyRead `json:"data"`
				}
				decodeHubResponse(t, response, &result)
				if result.OrganizationID != currentOrganization || result.ProjectID != args["project_id"] || result.Data.Policy == nil || result.Data.Policy.SourceDigest != policy.Digest([]byte(currentOrganization)) || result.Data.ApprovedBy != "credential_first" {
					t.Fatalf("policy identity or tenant content incorrect: %+v", result)
				}
			}
			currentRole, currentPermission = "owner", apikey.ScopeRead
			for _, scenario := range []struct {
				name  string
				setup func()
				reset func()
			}{
				{"selected", func() { selected = true }, func() { selected = false }},
				{"blocked", func() { currentBlocked = []string{"project_" + currentOrganization} }, func() { currentBlocked = nil }},
				{"revoked", func() { entryRevoked = true }, func() { entryRevoked = false }},
				{"invalid_role", func() { currentRole = "invalid" }, func() { currentRole = "owner" }},
			} {
				t.Run(scenario.name, func(t *testing.T) {
					scenario.setup()
					defer scenario.reset()
					response := call("get_project_policy", args, nil)
					if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "source_digest") {
						t.Fatalf("policy access denial failed: %d %s", response.Code, response.Body.String())
					}
				})
			}
			for _, project := range []string{"hidden_project", "project_org_other", "project_org_startup"} {
				if project == "project_"+currentOrganization {
					continue
				}
				response := call("get_project_policy", map[string]any{"project_id": project}, nil)
				if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "source_digest") {
					t.Fatalf("policy crossed project boundary: %d %s", response.Code, response.Body.String())
				}
			}
		})
		for _, name := range []string{operatortool.ListComments, operatortool.ListChanges} {
			t.Run(target.org+"/"+name, func(t *testing.T) {
				args := map[string]any{"project_id": "project_" + currentOrganization, "limit": 1}
				field := "identifier"
				if name == operatortool.ListChanges {
					field = "work_item_id"
				}
				args[field] = "wi_own"
				response := call(name, args, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("call status=%d body=%s", response.Code, response.Body.String())
				}
				catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
				requireNativeStatus(t, catalog, http.StatusOK)
				var definitions []operatortool.Definition
				decodeHubResponse(t, catalog, &definitions)
				found := false
				for _, d := range definitions {
					if d.Name == name {
						found = d.Annotations.ReadOnly
					}
					for _, forbidden := range []string{operatortool.CreateChange, operatortool.GetChange, operatortool.ReviewChange, operatortool.PublishChangeVersion} {
						if d.Name == forbidden {
							t.Errorf("advertised %s", forbidden)
						}
					}
				}
				if !found {
					t.Fatal("read missing from signed catalog")
				}
				for _, role := range []string{"owner", "admin", "member", "reporter", "viewer"} {
					currentRole = role
					response := call(name, args, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("role %s status=%d body=%s", role, response.Code, response.Body.String())
					}
					if name == operatortool.ListChanges {
						var result operatortool.ChangeResult
						decodeHubResponse(t, response, &result)
						expected := 2
						if role == "reporter" {
							expected = 1
						}
						if len(result.Changes) != 1 || len(result.Changes[0].LinkedIssues) != expected {
							t.Fatalf("role %s linked visibility = %+v", role, result.Changes)
						}
					}

				}
				currentRole = "owner"
				if name == operatortool.ListComments {
					seen := []string{}
					for n := 1; n <= 3; n++ {
						response := call(name, args, nil)
						requireNativeStatus(t, response, http.StatusOK)
						var result struct {
							Data tracker.Page[tracker.NativeComment] `json:"data"`
						}
						decodeHubResponse(t, response, &result)
						if len(result.Data.Items) != 1 || result.Data.Items[0].Body != fmt.Sprintf("%s comment %d", currentOrganization, n) {
							t.Fatalf("comment page %d = %+v", n, result)
						}
						seen = append(seen, result.Data.Items[0].ID)
						if n == 1 {
							args["cursor"] = result.Data.NextCursor
							args[field] = "wi_foreign"
							if response := call(name, args, nil); response.Code == http.StatusOK {
								t.Fatal("cursor crossed resource boundary")
							}
							args[field] = "wi_own"
						}
						if (result.Data.NextCursor != "") != (n < 3) {
							t.Fatal("comment continuation incorrect")
						}
						args["cursor"] = result.Data.NextCursor
					}
					if seen[0] == seen[1] || seen[1] == seen[2] {
						t.Fatal("duplicate comment page")
					}
					args["cursor"] = "invalid"
					if response := call(name, args, nil); response.Code == http.StatusOK {
						t.Fatal("invalid cursor accepted")
					}
					delete(args, "cursor")
				} else {
					for offset := 0; offset <= 3; offset++ {
						args["offset"] = offset
						response := call(name, args, nil)
						requireNativeStatus(t, response, http.StatusOK)
						var result operatortool.ChangeResult
						decodeHubResponse(t, response, &result)
						if offset == 3 {
							if len(result.Changes) != 0 || result.NextOffset != nil {
								t.Fatal("terminal Change page incorrect")
							}
							continue
						}
						if len(result.Changes) != 1 || result.Changes[0].ID != fmt.Sprintf("change_wi_own_%d", offset+1) || result.Changes[0].Body != currentOrganization+" preserved Change" {
							t.Fatalf("Change page %d = %+v", offset, result)
						}
						if (result.NextOffset != nil) != (offset < 2) || result.NextOffset != nil && *result.NextOffset != offset+1 {
							t.Fatal("Change continuation incorrect")
						}
					}
					delete(args, "offset")
					args["limit"] = 200
					requireNativeStatus(t, call(name, args, nil), http.StatusOK)
					args["limit"] = 1
				}

				selected = true
				selectedProjects = []string{"project_" + currentOrganization}
				requireNativeStatus(t, call(name, args, nil), http.StatusOK)
				selected = false
				selectedProjects = []string{"hidden_project"}
				args["limit"] = 201
				if response := call(name, args, nil); response.Code != http.StatusUnprocessableEntity {
					t.Fatalf("oversized limit status=%d body=%s", response.Code, response.Body.String())
				}
				args["limit"] = 1
				for _, scenario := range []struct {
					name  string
					setup func()
					reset func()
				}{
					{"selected", func() { selected = true }, func() { selected = false }},
					{"blocked", func() { currentBlocked = []string{"project_" + currentOrganization} }, func() { currentBlocked = nil }},
					{"key_revoked", func() { entryRevoked = true }, func() { entryRevoked = false }},
				} {
					scenario.setup()
					response := call(name, args, nil)
					scenario.reset()
					if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `"code":"access_denied"`) || strings.Contains(response.Body.String(), "preserved Change") || strings.Contains(response.Body.String(), "comment 1") {
						t.Fatalf("%s leaked read: %d %s", scenario.name, response.Code, response.Body.String())
					}
				}
				currentRole = "reporter"
				args[field] = "wi_foreign"
				if response := call(name, args, nil); response.Code == http.StatusOK {
					t.Fatal("reporter read another author's item")
				}
				currentRole = "owner"
				args[field] = "wi_own"
				args["project_id"] = "project_org_other"
				if currentOrganization == "org_other" {
					args["project_id"] = "project_org_startup"
				}
				if response := call(name, args, nil); response.Code == http.StatusOK {
					t.Fatal("cross organization read accepted")
				}
				args["project_id"] = "hidden_project"
				if response := call(name, args, nil); response.Code == http.StatusOK {
					t.Fatal("ungranted project read accepted")
				}
				args["project_id"] = "project_" + currentOrganization
				args[field] = "missing_item"
				if response := call(name, args, nil); response.Code == http.StatusOK {
					t.Fatal("missing item read accepted")
				}
			})
		}
	}

	service, currentOrganization = firstService, "org_startup"
	for _, name := range []string{operatortool.ListComments, operatortool.ListChanges} {
		t.Run(name+"_current_payload_integrity", func(t *testing.T) {
			if name == operatortool.ListComments {
				if err := service.stopGitHubWebhookMaintenance(); err != nil {
					t.Fatal(err)
				}
				repository := service.config.TenantRepository
				service.config.TenantRepository = pgmcpcatPendingCommentsRepository{Repository: repository}
				t.Cleanup(func() { service.config.TenantRepository = repository })
			}
			var key string
			if err := service.config.TenantRepository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
				if name == operatortool.ListComments {
					row, err := r.Comment(t.Context(), "project_org_startup", "wi_own", "c_wi_own_1")
					if err != nil {
						return err
					}
					version, err := r.CollaborationVersion(t.Context(), row.ID, row.Revision)
					key = version.PayloadObjectKey
					return err
				}
				row, err := r.Change(t.Context(), "change_wi_own_1")
				key = row.PayloadObjectKey
				return err
			}); err != nil {
				t.Fatal(err)
			}
			details := cfg.RunDetails.(*memoryRunDetails)
			details.mu.Lock()
			original := bytes.Clone(details.data[key])
			details.data[key] = []byte("corrupt payload")
			details.mu.Unlock()
			defer func() { details.mu.Lock(); details.data[key] = original; details.mu.Unlock() }()
			args := map[string]any{"project_id": "project_org_startup", "identifier": "wi_own", "limit": 1}
			if name == operatortool.ListChanges {
				delete(args, "identifier")
				args["work_item_id"] = "wi_own"
			}
			response := call(name, args, nil)
			if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"native_operation_failed"`) || strings.Contains(response.Body.String(), "corrupt payload") || strings.Contains(response.Body.String(), "preserved Change") {
				t.Fatalf("corrupt object accepted or leaked: %d %s", response.Code, response.Body.String())
			}
		})
	}
	if after := snapshot(); !bytes.Equal(before, after) {
		t.Fatal("API reads mutated canonical work, comments or Changes")
	}
	for _, target := range []struct {
		org     string
		service *Service
	}{{"org_startup", firstService}, {"org_other", otherService}} {
		service, currentOrganization = target.service, target.org
		currentPermission = apikey.ScopeWrite
		for _, name := range []string{operatortool.AddComment, operatortool.EditItem, operatortool.EditComment, operatortool.RestoreItem} {
			t.Run(target.org+"/"+name, func(t *testing.T) {
				args := map[string]any{"project_id": "project_" + currentOrganization, "identifier": "wi_own", "request_id": name, "expected_revision": "1"}
				switch name {
				case operatortool.AddComment:
					delete(args, "expected_revision")
					args["body"] = currentOrganization + " added comment"
				case operatortool.EditItem:
					args["title"] = currentOrganization + " updated title"
				case operatortool.EditComment:
					args["comment_id"], args["body"] = "c_wi_own_1", currentOrganization+" edited comment"
				case operatortool.RestoreItem:
					args["identifier"] = "wi_foreign"
				}
				catalog := request("/internal/v1/keys/catalog", "first", "", map[string]any{}, nil)
				requireNativeStatus(t, catalog, http.StatusOK)
				var definitions []operatortool.Definition
				decodeHubResponse(t, catalog, &definitions)
				if !entryKeyHasDefinition(definitions, name) {
					t.Errorf("%s missing from signed catalog", name)
				}
				currentPermission = apikey.ScopeRead
				requireNativeStatus(t, call(name, args, nil), http.StatusForbidden)
				currentPermission = apikey.ScopeWrite
				response := call(name, args, nil)
				if response.Code != http.StatusOK {
					t.Fatalf("call status=%d body=%s", response.Code, response.Body)
				}
				replay := call(name, args, nil)
				requireNativeStatus(t, replay, http.StatusOK)
				var originalResult, replayResult struct{ Data json.RawMessage }
				decodeHubResponse(t, response, &originalResult)
				decodeHubResponse(t, replay, &replayResult)
				if !bytes.Equal(originalResult.Data, replayResult.Data) {
					t.Fatal("mutation replay changed response")
				}
			})
		}
		currentPermission = apikey.ScopeRead
	}
	service, currentOrganization = firstService, "org_startup"
	for _, scenario := range []string{"grant", "membership", "credential"} {
		t.Run("revoke_"+scenario, func(t *testing.T) {
			for _, name := range []string{operatortool.ListComments, operatortool.ListChanges} {
				args := map[string]any{"project_id": "project_org_startup", "identifier": "wi_own"}
				if name == operatortool.ListChanges {
					delete(args, "identifier")
					args["work_item_id"] = "wi_own"
				}
				requireNativeStatus(t, call(name, args, nil), http.StatusOK)
			}
			if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
				switch scenario {
				case "grant":
					return w.DeleteAccessProjectGrant(t.Context(), "first", "project_org_startup")
				case "membership":
					return w.RevokeAccessMember(t.Context(), "first", time.Now())
				default:
					return w.RevokeAccessCredential(t.Context(), "credential_first", time.Now())
				}
			}); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{operatortool.ListComments, operatortool.ListChanges} {
				args := map[string]any{"project_id": "project_org_startup", "identifier": "wi_own"}
				if name == operatortool.ListChanges {
					delete(args, "identifier")
					args["work_item_id"] = "wi_own"
				}
				if response := call(name, args, nil); response.Code == http.StatusOK {
					t.Fatalf("%s survived %s revocation", name, scenario)
				}
			}
			if scenario == "membership" {
				if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					member, err := w.AccessMember(t.Context(), "first")
					if err != nil {
						return err
					}
					member.Active = true
					if err := w.UpdateAccessMember(t.Context(), member); err != nil {
						return err
					}
					if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_first", ProjectID: "project_org_startup"}); err != nil {
						return err
					}
					return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "first", ProjectID: "project_org_startup", CanWrite: true})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "grant" {
				if err := service.config.TenantRepository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
					return w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "first", ProjectID: "project_org_startup", CanWrite: true})
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
