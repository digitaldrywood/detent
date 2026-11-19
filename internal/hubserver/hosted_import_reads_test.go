package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHostedImportTypedReads(t *testing.T) {
	t.Parallel()
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	stamp, expiry := now.Add(-time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339)
	objects := &memoryRunDetails{}
	newSource := func(organization tracker.OrganizationID) *neutralimporttest.Fixture {
		t.Helper()
		fixture := newCoreNeutralFixture(t, organization, "organizations", "hosted_tenant", "api_tokens", "hosted_members", "hosted_release_preferences", "daily_summary_preferences", "daily_summary_receipts", "hosted_project_grants", "token_grants", "hosted_sessions", "hosted_transactions", "hosted_invitations", "hosted_member_reservations", "hosted_binding_migrations", "organization_secrets", "organization_secret_audit", "project_secrets", "project_secret_audit", "hosted_audit", "hosted_audit_times")
		fixture.Add("organizations", map[string]any{"id": organization, "name": "Example organization", "slug": organization, "local": false, "created_at": stamp})
		fixture.Add("hosted_tenant", map[string]any{"singleton": "1", "provider_id": "provider_" + string(organization), "bootstrap_subject": "target", "deployment": "example", "public_url": "https://example.test", "allocation_generation": "1"})
		return fixture
	}
	addSession := func(fixture *neutralimporttest.Fixture, key, payload string, subject any) {
		t.Helper()
		raw, err := json.Marshal(auth.HostedIdentity{Subject: payload, OrganizationID: "provider_" + string(fixture.Organization), SessionID: key, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		fixture.Add("hosted_sessions", map[string]any{"token_hash": key, "email": "user@example.test", "identity_json": string(raw), "identity_subject": subject, "created_at": stamp, "expires_at": expiry})
	}
	t.Run("conflicting imported session identity is refused", func(t *testing.T) {
		for _, entry := range []struct{ key, payload, subject string }{{"typed-target", "other", "target"}, {"typed-other", "target", "other"}} {
			t.Run(entry.key, func(t *testing.T) {
				organization := tracker.OrganizationID("org_conflict_" + entry.subject)
				fixture := newSource(organization)
				addSession(fixture, entry.key, entry.payload, entry.subject)
				if _, err := repository.ImportAccessCopy(t.Context(), postgres.AccessCopyImportSource{BundlePath: fixture.Write(), OrganizationID: organization, Catalog: catalog, Objects: objects}); !errors.Is(err, postgres.ErrAccessImport) {
					t.Fatalf("conflicting session identity import: %v", err)
				}
				var count int
				if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM hosted_sessions WHERE organization_id=$1", organization).Scan(&count); err != nil || count != 0 {
					t.Fatalf("conflicting session identity persisted: count=%d error=%v", count, err)
				}
			})
		}
	})
	sessions := []struct {
		key, payload string
		subject      any
		want         bool
	}{
		{"typed-target", "target", "target", true},
		{"typed-other", "other", "other", false},
		{"legacy-target", "target", nil, true},
		{"legacy-other", "other", nil, false},
	}
	receipts := []struct {
		retry          string
		kind, identity any
		lookup         string
	}{
		{"payload-identity", "account_administration", "typed-identity", "typed-identity"},
		{"legacy-identity", nil, nil, "legacy-identity"},
		{"partial-identity", "account_administration", nil, "partial-identity"},
	}
	organizations := []tracker.OrganizationID{"org_import_first", "org_import_second"}
	for _, organization := range organizations {
		fixture := newSource(organization)
		for _, subject := range []string{"target", "other", "inactive"} {
			fixture.Add("api_tokens", map[string]any{"id": "hosted_" + apikey.HashToken(subject)[:32], "name": subject, "token_hash": apikey.HashToken(string(organization) + subject), "token_fingerprint": hubapi.TokenFingerprint(apikey.HashToken(string(organization) + subject)), "scope": "operator", "native_only": true, "created_at": stamp, "updated_at": stamp, "operator_project_access": "selected", "key_kind": "personal", "hosted_user_id": subject, "hosted_membership_id": "membership_" + subject, "operator_key_scope": "write"})
			fixture.Add("hosted_members", map[string]any{"user_id": subject, "email": subject + "@example.test", "membership_id": "membership_" + subject, "role": "member", "active": subject != "inactive", "principal_id": "hosted_" + apikey.HashToken(subject)[:32], "created_at": stamp, "updated_at": stamp})
		}
		for _, entry := range sessions {
			addSession(fixture, entry.key, entry.payload, entry.subject)
		}
		addSession(fixture, "legacy-inactive", "inactive", nil)
		for index, entry := range receipts {
			raw, err := json.Marshal(accountReceipt{Kind: "account_administration", Retry: entry.retry, Hash: string(organization), Outcome: "pending"})
			if err != nil {
				t.Fatal(err)
			}
			fixture.Add("hosted_audit", map[string]any{"id": index + 1, "session_id": "session", "actual_actor": "actor", "effective_user": "actor", "reason": "", "event": string(raw), "route": "mcp/account", "project_id": "", "status": 0, "started_at": stamp, "expires_at": expiry, "recorded_at": stamp, "recorded_at_ns": now.Add(-time.Hour).UnixNano(), "account_kind": entry.kind, "account_retry": entry.identity, "mutation_json": `{}`})
		}
		if _, err := repository.ImportAccessCopy(t.Context(), postgres.AccessCopyImportSource{BundlePath: fixture.Write(), OrganizationID: organization, Catalog: catalog, Objects: objects}); err != nil {
			t.Fatal(err)
		}
	}
	for _, organization := range organizations {
		t.Run(string(organization), func(t *testing.T) {
			clock := &leaseTestClock{value: now}
			service := &Service{config: WithDeps(Config{AccessRepository: repository, TenantRepository: repository, RunDetails: objects, Clock: clock.Now, Hosted: &hostedplans.HostedConfig{OrganizationID: string(organization)}}, hubdeps.Deps{GenerateToken: apikey.GenerateToken})}
			authorize := func(org tracker.OrganizationID, entry string, subject string) error {
				return repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.AuthorizeAccess(t.Context(), accessstore.Authority{CredentialID: "hosted_" + apikey.HashToken(subject)[:32], Hash: apikey.HashToken(string(org) + subject), UserID: subject, MembershipID: "membership_" + subject, SessionHash: entry}, clock.Now())
					return err
				})
			}
			t.Run("session revocation honors typed subjects and evaluates unknowns", func(t *testing.T) {
				for _, entry := range sessions {
					if err := authorize(organization, entry.key, entry.payload); err != nil {
						t.Fatalf("imported session authority %s: %v", entry.key, err)
					}
				}
				cutoff := clock.Now()
				if err := service.revokeHostedMemberLocally(t.Context(), "target"); err != nil {
					t.Fatal(err)
				}
				for _, entry := range sessions {
					err := authorize(organization, entry.key, entry.payload)
					if entry.want && !errors.Is(err, accessstore.ErrDenied) || !entry.want && err != nil {
						t.Fatalf("current session authority %s: %v", entry.key, err)
					}
				}
				if organization == organizations[0] {
					for _, entry := range sessions {
						if err := authorize(organizations[1], entry.key, entry.payload); err != nil {
							t.Fatalf("member revocation crossed tenants %s: %v", entry.key, err)
						}
					}
				}
				readCutoff := func(subject string) *time.Time {
					t.Helper()
					var boundary *time.Time
					if err := admin.QueryRowContext(t.Context(), "SELECT sessions_revoked_before FROM hosted_members WHERE organization_id=$1 AND user_id=$2", organization, subject).Scan(&boundary); err != nil {
						t.Fatal(err)
					}
					return boundary
				}
				if got := readCutoff("target"); got == nil || !got.Equal(cutoff) {
					t.Fatalf("member session cutoff = %v, want %s", got, cutoff)
				}
				if got := readCutoff("other"); got != nil {
					t.Fatalf("active member acquired a session cutoff: %v", got)
				}
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					return w.RevokeAccessMember(t.Context(), "target", cutoff.Add(-time.Second))
				}); err != nil {
					t.Fatal(err)
				}
				if got := readCutoff("target"); got == nil || !got.Equal(cutoff) {
					t.Fatalf("older revocation moved session cutoff backwards: %v", got)
				}
				clock.Advance(time.Second)
				reactivate := func(subject string) {
					t.Helper()
					identity := auth.Identity{Subject: subject, Email: subject + "@example.test", EmailVerified: true, Hosted: &auth.HostedIdentity{Subject: subject, OrganizationID: "provider_" + string(organization), SessionID: "reactivated_" + subject, CreatedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Hour)}}
					membership := auth.Membership{ID: "membership_" + subject, UserID: subject, OrganizationID: identity.Hosted.OrganizationID, Status: "active"}
					membership.Role.Slug = "member"
					if err := service.addHostedMember(t.Context(), identity, membership); err != nil {
						t.Fatal(err)
					}
				}
				reactivate("target")
				for _, entry := range sessions {
					err := authorize(organization, entry.key, entry.payload)
					if entry.want && !errors.Is(err, accessstore.ErrDenied) || !entry.want && err != nil {
						t.Fatalf("reactivation restored an old session %s: %v", entry.key, err)
					}
					_, err = service.storedWebSession(t.Context(), entry.key, clock.Now())
					if entry.want && !errors.Is(err, auth.ErrInvalidSession) || !entry.want && err != nil {
						t.Fatalf("stored session after reactivation %s: %v", entry.key, err)
					}
				}
				if got := readCutoff("target"); got == nil || !got.Equal(cutoff) {
					t.Fatalf("reactivation changed the durable session cutoff: %v", got)
				}
				for _, entry := range []struct {
					name           string
					at             time.Time
					want, existing bool
				}{
					{"late old shared binding", cutoff.Add(-time.Microsecond), false, false},
					{"shared binding at revocation", cutoff, false, false},
					{"fresh shared binding", cutoff.Add(time.Microsecond), true, false},
					{"existing late old shared binding", cutoff.Add(-time.Microsecond), false, true},
				} {
					t.Run(entry.name, func(t *testing.T) {
						claims := cloudassert.Claims{Kind: cloudassert.KindBrowser, Audience: string(organization), Subject: "target", Email: "target@example.test", ProviderOrganization: "provider_" + string(organization), ProviderSession: entry.name, SessionCreatedAt: entry.at, SessionExpiresAt: clock.Now().Add(time.Hour), AccessExpiresAt: clock.Now().Add(time.Hour), Role: "member", Binding: entry.name}
						attempts := 1
						if entry.existing {
							identity := &auth.HostedIdentity{Subject: claims.Subject, OrganizationID: claims.ProviderOrganization, SessionID: claims.ProviderSession, CreatedAt: claims.SessionCreatedAt, ExpiresAt: claims.SessionExpiresAt}
							if err := service.CreateWebSession(t.Context(), auth.SessionRecord{TokenHash: claims.Binding, Email: claims.Email, Identity: identity, CreatedAt: clock.Now(), ExpiresAt: claims.SessionExpiresAt}); err != nil {
								t.Fatal(err)
							}
							attempts = 2
						}
						for range attempts {
							c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
							_, _, err := service.sharedAccessSession(t.Context(), c, claims)
							if entry.want && err != nil || !entry.want && !errors.Is(err, auth.ErrInvalidSession) {
								t.Fatalf("late-materialized shared session accepted=%v error=%v", entry.want, err)
							}
						}
						if err := authorize(organization, entry.name, "target"); entry.want && err != nil || !entry.want && !errors.Is(err, accessstore.ErrDenied) {
							t.Fatalf("late-materialized session authority accepted=%v error=%v", entry.want, err)
						}
						var persistedIdentity, persistedCreation time.Time
						err := admin.QueryRowContext(t.Context(), "SELECT identity_created_at,created_at FROM hosted_sessions WHERE organization_id=$1 AND token_hash=$2", organization, entry.name).Scan(&persistedIdentity, &persistedCreation)
						if entry.want || entry.existing {
							if err != nil || !persistedIdentity.Equal(entry.at) || !persistedCreation.After(cutoff) {
								t.Fatalf("late binding lost immutable provider time: identity=%s local=%s error=%v", persistedIdentity, persistedCreation, err)
							}
						} else if !errors.Is(err, sql.ErrNoRows) {
							t.Fatalf("denied lazy binding escaped transaction rollback: %v", err)
						}
					})
				}
				freshIdentity := &auth.HostedIdentity{Subject: "target", OrganizationID: "provider_" + string(organization), SessionID: "fresh-dedicated", CreatedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Hour)}
				if err := service.CreateWebSession(t.Context(), auth.SessionRecord{TokenHash: "fresh-dedicated", Email: "target@example.test", Identity: freshIdentity, CreatedAt: clock.Now(), ExpiresAt: freshIdentity.ExpiresAt}); err != nil {
					t.Fatal(err)
				}
				if _, err := service.storedWebSession(t.Context(), "fresh-dedicated", clock.Now()); err != nil {
					t.Fatalf("fresh dedicated session refused: %v", err)
				}
				if err := authorize(organization, "fresh-dedicated", "target"); err != nil {
					t.Fatalf("fresh dedicated session authority refused: %v", err)
				}
				inactiveAt := now.Add(-time.Hour)
				if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
					principal := "hosted_" + apikey.HashToken("current_inactive")[:32]
					hash := apikey.HashToken(string(organization) + "current_inactive")
					if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Name: "Current inactive member", Hash: hash, Fingerprint: hubapi.TokenFingerprint(hash), Scope: "operator", NativeOnly: true, Kind: "personal", UserID: "current_inactive", MembershipID: "membership_current_inactive", KeyScope: "write", ProjectAccess: "selected", CreatedAt: inactiveAt, UpdatedAt: inactiveAt}); err != nil {
						return err
					}
					if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "current_inactive", Email: "current_inactive@example.test", MembershipID: "membership_current_inactive", Role: "member", Active: false, PrincipalID: principal, CreatedAt: inactiveAt, UpdatedAt: inactiveAt}); err != nil {
						return err
					}
					return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: organization, Hash: "current-inactive", Email: "current_inactive@example.test", Subject: "current_inactive", ProviderOrganizationID: "provider_" + string(organization), SessionID: "current-inactive", IdentityCreatedAt: inactiveAt, IdentityExpiresAt: clock.Now().Add(time.Hour), CreatedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Hour)})
				}); err != nil {
					t.Fatal(err)
				}
				if got := readCutoff("current_inactive"); got == nil || !got.Equal(inactiveAt) {
					t.Fatalf("current inactive member lost cutoff: %v", got)
				}
				if _, err := service.storedWebSession(t.Context(), "current-inactive", clock.Now()); !errors.Is(err, auth.ErrInvalidSession) {
					t.Fatalf("current inactive session accepted: %v", err)
				}
				reactivate("current_inactive")
				if _, err := service.storedWebSession(t.Context(), "current-inactive", clock.Now()); !errors.Is(err, auth.ErrInvalidSession) {
					t.Fatalf("reactivation restored current inactive session: %v", err)
				}
				if got := readCutoff("inactive"); got == nil || !got.Equal(inactiveAt) {
					t.Fatalf("inactive import lost revocation cutoff: %v", got)
				}
				if _, err := service.storedWebSession(t.Context(), "legacy-inactive", clock.Now()); !errors.Is(err, auth.ErrInvalidSession) {
					t.Fatalf("inactive imported session accepted: %v", err)
				}
				result, err := admin.ExecContext(t.Context(), "UPDATE hosted_members SET sessions_revoked_before=NULL WHERE organization_id=$1 AND user_id='inactive' AND NOT active", organization)
				if err != nil {
					t.Fatal(err)
				}
				if affected, err := result.RowsAffected(); err != nil || affected != 1 {
					t.Fatalf("existing inactive member fixture affected=%d error=%v", affected, err)
				}
				if _, err := service.storedWebSession(t.Context(), "legacy-inactive", clock.Now()); !errors.Is(err, auth.ErrInvalidSession) {
					t.Fatalf("existing inactive session bypassed fallback cutoff: %v", err)
				}
				reactivate("inactive")
				if got := readCutoff("inactive"); got == nil || !got.Equal(inactiveAt) {
					t.Fatalf("reactivation lost existing inactive cutoff: %v", got)
				}
				if _, err := service.storedWebSession(t.Context(), "legacy-inactive", clock.Now()); !errors.Is(err, auth.ErrInvalidSession) {
					t.Fatalf("reactivation restored existing inactive session: %v", err)
				}
				for _, subject := range []string{"inactive", "current_inactive"} {
					identity := &auth.HostedIdentity{Subject: subject, OrganizationID: "provider_" + string(organization), SessionID: "fresh_" + subject, CreatedAt: clock.Now(), ExpiresAt: clock.Now().Add(time.Hour)}
					if err := service.CreateWebSession(t.Context(), auth.SessionRecord{TokenHash: identity.SessionID, Email: subject + "@example.test", Identity: identity, CreatedAt: clock.Now(), ExpiresAt: identity.ExpiresAt}); err != nil {
						t.Fatal(err)
					}
					if _, err := service.storedWebSession(t.Context(), identity.SessionID, clock.Now()); err != nil {
						t.Fatalf("fresh session after inactive reactivation %s: %v", subject, err)
					}
					if err := authorize(organization, identity.SessionID, subject); err != nil {
						t.Fatalf("fresh inactive member authority %s: %v", subject, err)
					}
				}
			})
			t.Run("account retry metadata is authoritative and unknown receipts remain visible", func(t *testing.T) {
				for _, entry := range receipts {
					raw, err := json.Marshal(accountReceipt{Kind: "account_administration", Retry: entry.retry, Hash: string(organization), Outcome: "pending"})
					if err != nil {
						t.Fatal(err)
					}
					got, err := service.readAccessAccountReceipt(t.Context(), "actor", entry.lookup)
					if err != nil || got != string(raw) {
						t.Fatalf("retry=%s receipt=%q error=%v", entry.lookup, got, err)
					}
				}
				if _, err := service.readAccessAccountReceipt(t.Context(), "foreign-actor", "legacy-identity"); !errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("foreign actor receipt: %v", err)
				}
			})
		})
	}
	t.Run("duplicate detection uses typed provenance and refuses stale authorization", func(t *testing.T) {
		f := newBatchFixture(t)
		if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		scope := f.scope(t)
		credential := scope.Credential
		batch := &tracker.GitHubBatch{ID: "typed-identity"}
		for index := range 2 {
			snapshot := batchSnapshot(index + 100)
			var issue tracker.NativeIssue
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", testHubAdminToken, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("source-%d", index)}, Title: fmt.Sprintf("source-%d", index), Body: "Preserved", State: "Backlog", Provenance: &snapshot.Provenance})
			requireNativeStatus(t, response, http.StatusOK)
			decodeHubResponse(t, response, &issue)
			preview := previewIssue(index+100, false)
			id, owned, err := f.service.existingGitHubBatchItem(t.Context(), scope, batch, preview)
			if err != nil || id != issue.WorkItemID || owned {
				t.Fatalf("typed duplicate=%s error=%v", id, err)
			}
		}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessCredential(t.Context(), credential.ID, f.service.config.Clock())
		}); err != nil {
			t.Fatal(err)
		}
		_, _, lookupErr := f.service.existingGitHubBatchItem(t.Context(), scope, batch, previewIssue(100, false))
		if !errors.Is(lookupErr, accessstore.ErrDenied) {
			t.Fatalf("stale authorization evaluated identity: %v", lookupErr)
		}
	})
}

func TestHostedImportOutboxFields(t *testing.T) {
	t.Parallel()
	objects := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: objects})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	items := []tracker.NativeIssue{f.create(t, "first outbox owner"), f.create(t, "second outbox owner")}
	cases := []struct {
		name, raw       string
		number, summary any
		want            string
		claim           int
		queued          bool
		payload         outboxFields
	}{
		{"typed keep", `{"issue_number":99,"summary":false}`, 77, true, "pending", 77, true, outboxFields{77, true}},
		{"typed number refusal", `{"issue_number":99,"summary":true}`, 0, true, "superseded", 0, false, outboxFields{0, true}},
		{"typed summary refusal", `{"issue_number":99,"summary":true}`, 99, false, "superseded", 0, true, outboxFields{99, false}},
		{"unknown keep", `{"issue_number":88,"summary":true}`, nil, nil, "pending", 88, true, outboxFields{88, true}},
		{"unknown summary refusal", `{"issue_number":99}`, nil, nil, "superseded", 0, true, outboxFields{99, false}},
		{"partial refusal", `{"issue_number":99,"summary":true}`, 0, nil, "superseded", 0, false, outboxFields{0, true}},
	}
	source := newCoreNeutralFixture(t, f.project.OrganizationID, "repositories", "projects", "issues", "github_outbox")
	source.Add("repositories", map[string]any{"id": 1, "github_node_id": "outbox-repository", "github_owner": "example-org", "github_name": "example-project", "created_at": testTimestamp, "updated_at": testTimestamp})
	source.Add("projects", map[string]any{"id": f.project.ID, "name": "Example project", "slug": "example-project", "profile": "native", "github_projection": "disabled", "created_at": testTimestamp})
	for index, item := range items {
		source.Add("issues", map[string]any{"id": index + 1, "native_id": item.WorkItemID, "project_id": f.project.ID, "repository_id": 1})
	}
	for index, entry := range cases {
		source.Add("github_outbox", map[string]any{"id": index + 1, "idempotency_key": entry.name, "repository_id": 1, "issue_id": index%2 + 1, "mutation_kind": "workpad", "desired_json": entry.raw, "status": "pending", "created_at": testTimestamp, "updated_at": testTimestamp, "desired_issue_number": entry.number, "desired_summary": entry.summary})
	}
	lifecycles := []string{"pending", "processing", "retrying"}
	for index, status := range lifecycles {
		values := map[string]any{"id": index + 7, "idempotency_key": "attempted-" + status, "repository_id": 1, "issue_id": index%2 + 1, "mutation_kind": "workpad", "desired_json": `{"issue_number":"shadowed","summary":"shadowed","phase":"legacy","body":"Retained source body","extra":"retained"}`, "status": status, "attempts": 3, "coalesce_key": "historical-" + status, "last_attempt_at": testTimestamp, "next_retry_at": testTimestamp, "created_at": testTimestamp, "updated_at": testTimestamp, "desired_issue_number": index + 91, "desired_summary": false}
		if status == "processing" {
			values["processing_started_at"] = testTimestamp
		}
		source.Add("github_outbox", values)
	}
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	input := TenantCoreImportSource{BundlePath: source.Write(), OrganizationID: f.project.OrganizationID, Catalog: catalog, Objects: objects}
	if count, err := ImportCoreGitHubOutbox(t.Context(), f.service.database.postgres, input); err != nil || count != 9 {
		t.Fatalf("historical outbox import=%d error=%v", count, err)
	}
	if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		project, err := w.ProjectRecord(t.Context(), f.project.ID)
		if err != nil {
			return err
		}
		project.Settings.Projection = "disabled"
		if _, err := w.UpdateProjectRecord(t.Context(), project); err != nil {
			return err
		}
		return w.(projectstore.GitHubOutboxWriter).SupersedeGitHubCompatibilityWrites(t.Context(), f.project.ID, project.Settings.RepositoryEnabled, f.service.config.Clock())
	}); err != nil {
		t.Fatal(err)
	}
	var claimed []projectstore.GitHubOutbound
	for index, entry := range cases {
		var row projectstore.GitHubOutbound
		var receipt eventstore.Receipt
		err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			row, err = r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), entry.name)
			if errors.Is(err, corestore.ErrNotFound) && !entry.queued {
				receipt, err = r.Receipt(t.Context(), "production-import", "core-github-outbox", fmt.Sprint(index+1))
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		status := row.Status
		var normalized json.RawMessage
		if !entry.queued {
			raw, err := eventstore.NewContent(objects).Receipt(t.Context(), receipt)
			if err != nil {
				t.Fatal(err)
			}
			var proof struct{ Value coreGitHubOutboxImportValue }
			if err := json.Unmarshal(raw, &proof); err != nil {
				t.Fatal(err)
			}
			if proof.Value.Queued || proof.Value.Record.IssueNumber != 0 || !bytes.Equal(proof.Value.Source.Values["desired_json"].Data, []byte(entry.raw)) || proof.Value.Source.Values["desired_issue_number"].Null != (entry.number == nil) || proof.Value.Source.Values["desired_summary"].Null != (entry.summary == nil) {
				t.Fatal("retired outbox receipt changed typed source identity or nullability")
			}
			status = proof.Value.Status
			normalized = proof.Value.Desired
		} else {
			normalized, err = eventstore.NewContent(objects).Receipt(t.Context(), row.Payload)
			if err != nil {
				t.Fatal(err)
			}
		}
		var fields outboxFields
		if err := json.Unmarshal(normalized, &fields); err != nil || fields != entry.payload {
			t.Fatalf("%s imported payload=%+v want=%+v error=%v", entry.name, fields, entry.payload, err)
		}
		if status != entry.want {
			t.Fatalf("%s status=%s want=%s error=%v", entry.name, status, entry.want, err)
		}
		if entry.want == "pending" {
			var item projectstore.GitHubOutbound
			err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				item, err = w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(t.Context(), f.service.config.Clock(), f.service.config.Clock().Add(time.Minute), fmt.Sprint(index+1))
				return err
			})
			if err != nil || item.Key != entry.name || item.IssueNumber != int64(entry.claim) {
				t.Fatalf("claim=%+v error=%v", item, err)
			}
			payload, err := eventstore.NewContent(objects).Receipt(t.Context(), item.Payload)
			if err != nil {
				t.Fatal(err)
			}
			var desired WorkpadDesired
			if err := json.Unmarshal(payload, &desired); err != nil || desired.IssueNumber != entry.claim || !desired.Summary {
				t.Fatalf("claimed payload ignored typed source fields: %+v error=%v", desired, err)
			}
			claimed = append(claimed, item)
		}
	}
	for index, status := range lifecycles {
		var item projectstore.GitHubOutbound
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			item, err = w.(projectstore.GitHubOutboxWriter).ClaimGitHubOutbound(t.Context(), f.service.config.Clock(), f.service.config.Clock().Add(time.Minute), "attempted-"+status)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if item.Key != "attempted-"+status || item.IssueNumber != int64(index+91) || item.Outcome != "uncertain" || item.Attempts != 4 || item.Status != "processing" || item.CoalesceKey != "historical-"+status {
			t.Fatalf("attempted historical outbox lost reconciliation authority: %+v", item)
		}
		payload, err := eventstore.NewContent(objects).Receipt(t.Context(), item.Payload)
		if err != nil {
			t.Fatal(err)
		}
		var desired struct {
			IssueNumber int    `json:"issue_number"`
			Summary     bool   `json:"summary"`
			Body        string `json:"body"`
			Extra       string `json:"extra"`
		}
		if err := json.Unmarshal(payload, &desired); err != nil || desired.IssueNumber != index+91 || desired.Summary || desired.Body != "Retained source body" || desired.Extra != "retained" {
			t.Fatalf("historical outbox payload lost source fields: %+v error=%v", desired, err)
		}
		claimed = append(claimed, item)
	}
	if count, err := ImportCoreGitHubOutbox(t.Context(), f.service.database.postgres, input); err != nil || count != 9 {
		t.Fatalf("historical outbox replay=%d error=%v", count, err)
	}
	for _, previous := range claimed {
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			current, err := r.(projectstore.GitHubOutboxWriter).GitHubOutbound(t.Context(), previous.Key)
			if err == nil && !reflect.DeepEqual(current, previous) {
				t.Fatalf("outbox replay reset live claim: %+v want %+v", current, previous)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	var rows int
	if err := hostedPostgresAdmin(t, f.service).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1", f.project.OrganizationID).Scan(&rows); err != nil || rows != 7 {
		t.Fatalf("outbox import fabricated or duplicated queue rows: %d error=%v", rows, err)
	}
}

func TestHostedImportReplaySurvivesLegacyEvaluationFailure(t *testing.T) {
	t.Parallel()
	details := &memoryRunDetails{}
	f := newDefaultNativeFixture(t, Config{RunDetails: details})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
	if err != nil {
		t.Fatal(err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	issue := f.create(t, "before replay")
	title := "recorded"
	request := tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "typed-replay"}, ExpectedRevision: issue.Revision, Title: &title}
	first, err := f.service.updateNativeIssueCommand(t.Context(), scope, string(issue.WorkItemID), request)
	if err != nil {
		t.Fatal(err)
	}
	var changed tracker.NativeIssue
	if err := json.Unmarshal(first, &changed); err != nil || changed.Title != title || changed.Revision != issue.Revision+1 {
		t.Fatalf("mutation response=%s error=%v", first, err)
	}
	row, version, err := f.service.readWorkItemRecord(t.Context(), scope, string(issue.WorkItemID))
	if err != nil {
		t.Fatal(err)
	}
	details.mu.Lock()
	saved, exists := details.data[version.PayloadObjectKey]
	delete(details.data, version.PayloadObjectKey)
	details.mu.Unlock()
	if !exists {
		t.Fatal("canonical source object was not stored")
	}
	t.Cleanup(func() {
		details.mu.Lock()
		details.data[version.PayloadObjectKey] = saved
		details.mu.Unlock()
	})
	replay, err := f.service.updateNativeIssueCommand(t.Context(), scope, string(issue.WorkItemID), request)
	if err != nil || !bytes.Equal(first, replay) {
		t.Fatalf("receipt replay=%s error=%v", replay, err)
	}
	request.IdempotencyKey, request.ExpectedRevision = "typed-new-command", changed.Revision
	if _, err := f.service.updateNativeIssueCommand(t.Context(), scope, string(issue.WorkItemID), request); !errors.Is(err, artifact.ErrMissing) {
		t.Fatalf("unevaluated new mutation ran: error=%v", err)
	}
	if err := f.service.config.TenantRepository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
		current, err := r.WorkItem(t.Context(), scope.Project, issue.WorkItemID)
		if err != nil {
			return err
		}
		if current.Revision != row.Revision || current.Title != row.Title {
			t.Fatalf("receipt replay or failed new mutation wrote another effect: current=%+v before=%+v", current, row)
		}
		if _, err := r.CollaborationVersion(t.Context(), string(issue.WorkItemID), row.Revision+1); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("receipt replay or failed new mutation wrote another version: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
