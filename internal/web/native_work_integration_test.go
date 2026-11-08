package web_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
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
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
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

func TestNativeWorkFormsPersistWithoutGitHub(t *testing.T) {
	t.Parallel()
	config := nativeFormsPostgresConfig(t)
	for _, organization := range []tracker.OrganizationID{"org_example", "org_other"} {
		for _, project := range []tracker.ProjectID{"prj_forms", "prj_second"} {
			t.Run(string(organization)+"/"+string(project), func(t *testing.T) {
				hosted := *config.Hosted
				hosted.OrganizationID = string(organization)
				hosted.WorkOSOrganizationID = "provider_" + string(organization)
				config.Hosted = &hosted
				testNativeWorkForms(t, config, organization, project)
			})
		}
	}
}

func testNativeWorkForms(t *testing.T, config hubserver.Config, organization tracker.OrganizationID, project tracker.ProjectID) {
	t.Helper()
	open := func() (*hubserver.Service, *hubclient.NativeClient) {
		service, err := hubserver.Open(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := service.Close(); err != nil {
				t.Error(err)
			}
		})
		server := httptest.NewServer(service.Handler())
		t.Cleanup(server.Close)
		foreign := "org_other"
		if organization == "org_other" {
			foreign = "org_example"
		}
		for _, probe := range []struct {
			organization, token string
			want                int
		}{
			{string(organization), "", http.StatusUnauthorized},
			{foreign, "form-" + string(organization), http.StatusNotFound},
		} {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v2/organizations/"+probe.organization+"/projects/"+string(project)+"/work-items", nil)
			if err != nil {
				t.Fatal(err)
			}
			if probe.token != "" {
				request.Header.Set("Authorization", "Bearer "+probe.token)
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != probe.want {
				t.Fatalf("scope denial = %d, want %d", response.StatusCode, probe.want)
			}
		}
		client, err := hubclient.New(hubclient.Config{URL: server.URL, HTTPClient: server.Client(), TokenSource: func() string { return "form-" + string(organization) }})
		if err != nil {
			t.Fatal(err)
		}
		native, err := client.Native(organization, project)
		if err != nil {
			t.Fatal(err)
		}
		return service, native
	}
	service, native := open()
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
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, restored := open()
	restoredIssue, err := restored.Issue(t.Context(), id)
	if err != nil || restoredIssue.Title != current.Title || restoredIssue.Body != current.Body || restoredIssue.State != current.State || restoredIssue.Priority == nil || *restoredIssue.Priority != 0 || len(restoredIssue.Dependencies) != 0 {
		t.Fatalf("restored issue: %#v, %v", restoredIssue, err)
	}
	restoredComments, err := restored.Comments(t.Context(), id, "")
	if err != nil || len(restoredComments.Items) != 1 || restoredComments.Items[0].Body != "Edited comment" {
		t.Fatalf("restored comments: %#v, %v", restoredComments, err)
	}
	restoredChanges, err := restored.Changes(t.Context(), blocker.WorkItemID)
	if err != nil || len(restoredChanges) != 1 || len(restoredChanges[0].LinkedIssues) != 2 {
		t.Fatalf("restored changes: %#v, %v", restoredChanges, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
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

func nativeFormsPostgresConfig(t *testing.T) hubserver.Config {
	t.Helper()
	var logs bytes.Buffer
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(logs.String())
		}
	})
	template := nativeFormsTemplate
	admin := template.Database(t)
	role, password := "forms_"+strings.ReplaceAll(uuid.NewString(), "-", ""), uuid.NewString()
	if _, err := admin.ExecContext(t.Context(), "CREATE ROLE "+pgx.Identifier{role}.Sanitize()+" LOGIN PASSWORD "+pgtransport.QuoteLiteral(password)+" IN ROLE detent_runtime_v2"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline, _ := t.Deadline()
		ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
			t.Error(err)
		}
	})
	connection := template.Connection(t, admin, role, password)
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		if name == "DETENT_HUB_SECRET_KEYS" {
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)) + `"}`
		}
		return "1"
	})
	if err != nil {
		t.Fatal(err)
	}
	poolConfig, err := connection.PoolConfig()
	if err != nil {
		t.Fatal(err)
	}
	poolConfig.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(t.Context(), poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	budgets := postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1}
	repository, err := postgres.NewRepository(t.Context(), pool, budgets)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, organization := range []tracker.OrganizationID{"org_example", "org_other"} {
		now := time.Now().UTC().Truncate(time.Microsecond)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: string(organization), Slug: strings.ReplaceAll(string(organization), "_", "-"), CreatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: organization, ProviderID: "provider_" + string(organization), AllocationGeneration: 1}); err != nil {
				return err
			}
			key := bytes.Repeat([]byte{0x5a}, 32)
			if err := w.CreateHubIdentity(t.Context(), accessstore.HubIdentity{OrganizationID: organization, ID: "hub_" + artifact.Digest([]byte(organization))[:32], CursorKey: key}); err != nil {
				return err
			}
			envelope, err := keys.Seal(key, accessstore.SecretAAD(organization, "", accessstore.NativeCursorKeyKind))
			if err != nil {
				return err
			}
			if err := w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Envelope: envelope, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: organization, Kind: accessstore.NativeCursorKeyKind, Actor: "fixture", Event: "create", KeyVersion: envelope.Version, RecordedAt: now}); err != nil {
				return err
			}
			user, membership, principal := "user_"+string(organization), "membership_"+string(organization), "operator_"+string(organization)
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: principal, Name: "Form operator", Hash: apikey.HashToken("form-" + string(organization)), Scope: "operator", KeyScope: "write", NativeOnly: true, ProjectAccess: "all", Kind: "personal", UserID: user, MembershipID: membership, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: user, Email: user + "@example.test", MembershipID: membership, Role: "owner", Active: true, PrincipalID: principal, CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for _, project := range []tracker.ProjectID{"prj_forms", "prj_second"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: string(project), Slug: strings.TrimPrefix(string(project), "prj_"), Profile: "native", CreatedAt: now}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress"}}, {Name: "In Progress", Dispatchable: true}}}); err != nil {
					return err
				}
				if err := w.SetAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, ProjectID: project, UserID: user, CanWrite: true}); err != nil {
					return err
				}
				if err := w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, ProjectID: project, TokenID: principal}); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	provider, err := auth.NewHostedProvider(auth.IdentityProviderWorkOS, auth.WorkOSConfig{APIURL: "https://identity.example.test", ClientID: "client_example", APIKey: "fixture-key", RedirectURL: "https://hub.example.test/auth/callback", HTTPClient: &http.Client{Transport: nativeFormIdentityTransport{}}})
	if err != nil {
		t.Fatal(err)
	}
	return hubserver.Config{Logger: slog.New(slog.NewTextHandler(&logs, nil)), DatabaseBackend: hubserver.DatabasePostgres, Postgres: &postgres.Config{Connection: connection, Pool: budgets}, SecretKeys: keys, Content: newContentStore(), RunDetails: hubmaintenance.LimitDetails(&nativeFormDetails{data: make(map[string][]byte)}), Hosted: &hubserver.HostedConfig{OrganizationID: "org_example", WorkOSOrganizationID: "provider_org_example", PublicURL: "https://hub.example.test", Provider: provider}}
}

type nativeFormIdentityTransport struct{}

func (nativeFormIdentityTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Path != "/user_management/organization_memberships" {
		return nil, errors.New("unexpected identity provider request")
	}
	organization := strings.TrimPrefix(request.URL.Query().Get("organization_id"), "provider_")
	membership := auth.Membership{ID: "membership_" + organization, UserID: "user_" + organization, OrganizationID: "provider_" + organization, Status: "active"}
	membership.Role.Slug = "owner"
	response := httptest.NewRecorder()
	if err := json.NewEncoder(response).Encode(map[string]any{"data": []auth.Membership{membership}}); err != nil {
		return nil, err
	}
	return response.Result(), nil
}

type nativeFormDetails struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (s *nativeFormDetails) StoreDetail(ctx context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil || ctx.Err() != nil {
		return ref, errors.Join(err, ctx.Err())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[ref.Key] = bytes.Clone(data)
	return ref, nil
}

func (s *nativeFormDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	return s.read(ctx, ref.Key)
}

func (s *nativeFormDetails) ReadCanonicalDetail(ctx context.Context, organization, project, key string) ([]byte, error) {
	if err := attachment.ValidateCanonicalDetailKey(organization, project, key); err != nil {
		return nil, err
	}
	return s.read(ctx, key)
}

func (s *nativeFormDetails) read(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, found := s.data[key]
	if !found {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}
