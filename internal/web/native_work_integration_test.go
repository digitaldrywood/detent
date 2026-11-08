package web_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/pgtransport"
	"github.com/digitaldrywood/detent/internal/testenv/pgtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var nativeFormsTemplate = pgtest.NewTemplate(func(ctx context.Context, db *sql.DB) error {
	roles, err := os.ReadFile("../hubserver/postgres/roles.sql")
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, string(roles)); err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "SET ROLE "+postgres.MigrationRoleName); err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, postgres.RoleMigrations(os.DirFS("../hubserver/postgres/migrations")), goose.WithDisableGlobalRegistry(true), goose.WithSlog(slog.New(slog.DiscardHandler)))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
})

type nativeFormsProvider struct{ auth.HostedProvider }

func (nativeFormsProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	membership := auth.Membership{ID: "membership_" + organization, UserID: user, OrganizationID: organization, Status: "active"}
	membership.Role.Slug = "owner"
	return []auth.Membership{membership}, nil
}

type nativeFormsDetails struct{ content entryauthority.ContentHandler }

func (d nativeFormsDetails) StoreDetail(ctx context.Context, organization string, content []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(content))
	if err != nil {
		return ref, err
	}
	_, err = d.content(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref, Write: true, Data: content})
	return ref, err
}

func (d nativeFormsDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	return d.content(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref})
}

func nativeFormsPostgresConfig(t *testing.T) hubserver.Config {
	t.Helper()
	admin := nativeFormsTemplate.Database(t)
	name, password := "example_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+quoted+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" IN ROLE "+postgres.RuntimeRoleName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+quoted); err != nil {
			t.Error(err)
		}
	})
	poolConfig := nativeFormsTemplate.NativeConfig(t, admin, name, password)
	poolConfig.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	poolLimits := postgres.PoolConfig{MaxConnections: 4, ServingConnections: 2, BackgroundConnections: 1}
	repository, err := postgres.NewRepository(t.Context(), pool, poolLimits)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []tracker.OrganizationID{"org_forms", "org_other"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			now := time.Now().UTC()
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "provider_" + string(org), AllocationGeneration: 1}); err != nil {
				return err
			}
			membership := "membership_provider_" + string(org)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "forms_operator", Hash: apikey.HashToken("integration-operator"), Name: "Forms operator", Scope: "operator", NativeOnly: true, Kind: "personal", UserID: "user_forms", MembershipID: membership, KeyScope: "write", ProjectAccess: "all", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: org, UserID: "user_forms", Email: "forms@example.test", MembershipID: membership, Role: "owner", Active: true, PrincipalID: "forms_operator", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: "prj_forms", Name: "Native forms", Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Transitions: []string{"In Progress"}}, {Name: "In Progress"}}}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: org, TokenID: "forms_operator", ProjectID: "prj_forms"}); err != nil {
				return err
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: org, UserID: "user_forms", ProjectID: "prj_forms", CanWrite: true}); err != nil {
				return err
			}
			cursor := bytes.Repeat([]byte{2}, 32)
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: org, ID: "hub_" + strings.ReplaceAll(uuid.NewString(), "-", ""), CursorKey: cursor}); err != nil {
				return err
			}
			envelope, err := keys.Seal(cursor, accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	content := newContentStore()
	return hubserver.Config{DatabaseBackend: hubserver.DatabasePostgres, Postgres: &postgres.Config{Connection: nativeFormsTemplate.Connection(t, admin, name, password), Pool: poolLimits}, Content: content, RunDetails: nativeFormsDetails{content: content}, SecretKeys: keys, Hosted: &hubserver.HostedConfig{OrganizationID: "org_forms", WorkOSOrganizationID: "provider_org_forms", PublicURL: "https://hub.example.test", Provider: nativeFormsProvider{}}}
}

func TestNativeWorkFormsPersistWithoutGitHub(t *testing.T) {
	t.Parallel()
	service, err := hubserver.Open(t.Context(), nativeFormsPostgresConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	org := tracker.OrganizationID("org_forms")
	hubHTTP := httptest.NewServer(service.Handler())
	t.Cleanup(hubHTTP.Close)
	client, err := hubclient.New(hubclient.Config{URL: hubHTTP.URL, TokenSource: func() string { return "integration-operator" }, HTTPClient: hubHTTP.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(org, "prj_forms")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Project(t.Context()); err != nil {
		t.Fatalf("read PostgreSQL form project: %v", err)
	}
	server := nativeWebServerWithClient(t, native, &nativeWebFixture{})
	const contractSections = "\n## Acceptance criteria\nCreate and edit native work through forms.\n## Must not break\nComment and Change operations.\n## How we know it worked\nThe native form integration test passes."
	const createdBody = "Full native body" + contractSections
	const editedBody = "Edited body" + contractSections
	const dependencyBody = "## Acceptance criteria\nResolve the prerequisite storage migration before dispatching dependent tasks.\n## Must not break\nPreserve database records, schema compatibility and tenant isolation.\n## How we know it worked\nVerify successful upgrades against populated PostgreSQL fixtures."
	submit := func(path string, form url.Values, want int) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Authorization", "Bearer web-secret")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := httptest.NewRecorder()
		server.Handler().ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("submit %s: %d want %d: %s", form.Get("action"), res.Code, want, res.Body)
		}
		return res.Header().Get("Location")
	}
	location := submit("/projects/native/issues/new", url.Values{"action": {"create"}, "key": {"create-issue"}, "title": {"Native authoring"}, "body": {createdBody}, "state": {"Todo"}}, 303)
	id := tracker.NativeWorkItemID(strings.TrimPrefix(location, "/projects/native/issues/"))
	issue, err := native.Issue(t.Context(), id)
	if err != nil || issue.Title != "Native authoring" || issue.Body != createdBody || len(issue.ExternalReferences) != 0 {
		t.Fatalf("created issue: %#v, %v", issue, err)
	}
	blocker, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "blocker"}, Title: "Dependency", Body: dependencyBody, State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		action string
		fields url.Values
	}{
		{"edit", url.Values{"title": {"Edited native title"}, "body": {editedBody}, "priority": {"0"}}},
		{"dependency", url.Values{"related": {string(blocker.WorkItemID)}, "operation": {"add"}}},
		{"dependency", url.Values{"related": {string(blocker.WorkItemID)}, "operation": {"remove"}}},
		{"transition", url.Values{"state": {"In Progress"}}},
		{"comment", url.Values{"body": {"Native comment"}}},
		{"change", url.Values{"title": {"Native Change"}, "body": {"Review discussion"}, "related": {string(blocker.WorkItemID)}}},
	} {
		t.Run(tt.action+tt.fields.Get("operation"), func(t *testing.T) {
			current, err := native.Issue(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			form := tt.fields
			form.Set("action", tt.action)
			form.Set("key", tt.action+tt.fields.Get("operation"))
			form.Set("revision", strconv.FormatInt(int64(current.Revision), 10))
			submit(location+"/edit", form, 303)
			if tt.action == "edit" {
				form.Set("key", "stale-edit")
				submit(location+"/edit", form, 409)
			}
		})
	}
	current, err := native.Issue(t.Context(), id)
	if err != nil || current.Title != "Edited native title" || current.Body != editedBody || current.State != "In Progress" || current.Priority == nil || *current.Priority != 0 || len(current.Dependencies) != 0 {
		t.Fatalf("persisted issue: %#v, %v", current, err)
	}
	comments, err := native.Comments(t.Context(), id, "")
	if err != nil || len(comments.Items) != 1 {
		t.Fatalf("comments: %#v, %v", comments, err)
	}
	comment := comments.Items[0]
	form := url.Values{"action": {"comment_edit"}, "comment": {comment.ID}, "key": {"comment-edit"}, "revision": {"1"}, "body": {"Edited comment"}}
	submit(location+"/edit", form, 303)
	form.Set("key", "stale-comment")
	submit(location+"/edit", form, 409)
	changes, err := native.Changes(t.Context(), blocker.WorkItemID)
	if err != nil || len(changes) != 1 || len(changes[0].LinkedIssues) != 2 {
		t.Fatalf("linked changes: %#v, %v", changes, err)
	}
}

func newContentStore() entryauthority.ContentHandler {
	var contentMu sync.Mutex
	contentObjects := make(map[string][]byte)
	return func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := request.Reference.Validate(request.Organization); err != nil {
			return nil, err
		}
		contentMu.Lock()
		defer contentMu.Unlock()
		if request.Write {
			contentObjects[request.Reference.Key] = bytes.Clone(request.Data)
			return nil, nil
		}
		data, found := contentObjects[request.Reference.Key]
		if !found {
			return nil, errors.New("archive unavailable")
		}
		return bytes.Clone(data), nil
	}
}
