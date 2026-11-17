package usagecosts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type memoryRunDetails struct {
	mu          sync.Mutex
	data        map[string][]byte
	fail        bool
	uploads     int
	reads       int
	keyReads    map[string]int
	writerReads int
	afterStore  func(attachment.ArchiveReference)
	beforeRead  func(context.Context) error
}

func (m *memoryRunDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	m.mu.Lock()
	if m.fail {
		m.mu.Unlock()
		return attachment.ArchiveReference{}, errors.New("upload unavailable")
	}
	if m.data == nil {
		m.data = make(map[string][]byte)
	}
	if _, ok := m.data[ref.Key]; !ok {
		m.uploads++
	}
	m.data[ref.Key] = bytes.Clone(data)
	afterStore := m.afterStore
	m.mu.Unlock()
	if afterStore != nil {
		afterStore(ref)
	}
	return ref, nil
}

func (m *memoryRunDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if m.beforeRead != nil {
		if err := m.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	m.reads++
	if m.keyReads != nil {
		m.keyReads[ref.Key]++
	}

	data, ok := m.data[ref.Key]
	if !ok {
		return nil, artifact.ErrMissing
	}
	return bytes.Clone(data), nil
}
func validateDetail(ctx context.Context, store attachment.DetailStore, organization tracker.OrganizationID, ref attachment.ArchiveReference, expected []byte) error {
	if ref.Validate(string(organization)) != nil || ref.BindingID != "" || ref.CustomerOwned || ref.SchemaVersion != 1 || ref.UncompressedSize != int64(len(expected)) {
		return artifact.ErrIntegrity
	}
	actual, err := store.ReadDetail(ctx, string(organization), ref)
	if err != nil {
		return errors.Join(artifact.ErrIntegrity, err)
	}
	if !bytes.Equal(actual, expected) {
		return artifact.ErrIntegrity
	}
	return nil
}

func TestMain(m *testing.M) { hubtest.Main(m) }
func keys(t *testing.T) *hubsecrets.Keyring {
	t.Helper()
	k, e := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if e != nil {
		t.Fatal(e)
	}
	return k
}

type costFixture struct {
	*Service
	admin   *sql.DB
	details attachment.DetailStore
}

func newCostLedgerTestService(t *testing.T, now time.Time) *costFixture {
	t.Helper()
	admin := hubtest.Database(t)
	repo := hubtest.RepositoryOn(t, admin)
	details := hubtest.NewDetails()
	for _, org := range []tracker.OrganizationID{"org", "other-org"} {
		hubtest.Organization(t, repo, hubtest.OrganizationOptions{ID: org, Name: string(org), Slug: string(org), Now: now})
		for _, project := range []tracker.ProjectID{"original", "replacement", "one", "two"} {
			hubtest.Project(t, repo, org, hubtest.ProjectOptions{ID: project, Name: string(project)})
		}
	}
	return &costFixture{Service: New(Deps{Repository: repo, Details: details}), admin: admin, details: details}
}

const testHubAdminToken = "detent_test_hub_admin_token_00000000000000000000000000000000"

func nativeFixtureStates() []tracker.NativeState { return hubtest.States() }
func createOrganization(t *testing.T, repo tenantstore.Repository, name string) (testOrganization, error) {
	t.Helper()
	org := hubtest.Organization(t, repo, hubtest.OrganizationOptions{Name: name})
	return testOrganization{ID: org.OrganizationID, Name: org.Name, Slug: org.Slug}, nil
}
func seedAdmin(t *testing.T, repo tenantstore.Repository, org tracker.OrganizationID) {
	t.Helper()
	now := time.Now().UTC()
	if err := repo.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: org, ID: "test_hub_admin", Hash: apikey.HashToken(testHubAdminToken), Name: "Example admin", Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "service", CreatedAt: now, UpdatedAt: now})
	}); err != nil {
		t.Fatal(err)
	}
}
func costHTTP(t *testing.T, repo tenantstore.Repository, details attachment.DetailStore) *echo.Echo {
	t.Helper()
	h := hubtest.NewHTTP(t, hubtest.HTTPOptions{Repository: repo}, func(e *echo.Group, d hubtest.HTTPDeps) {
		service := New(Deps{Repository: repo, Details: details, Now: time.Now, ContentContext: func(ctx context.Context) context.Context { return ctx }, RecordError: hubauth.RecordError, AccountUsage: func(context.Context, tenantstore.Writer, time.Time, string) error { return nil }, ValidateDetail: validateDetail})
		RegisterRoutes(e, HTTPDeps{Service: service, Native: d.Native, InstanceAdmin: d.API(hubauth.Admin), Scope: func(c echo.Context, admin bool) hubapi.Scope {
			scope := hubauth.RequestScope(c)
			scope.Admin = admin
			return scope
		}, CanManage: func(c echo.Context) bool {
			v, _ := hubauth.RequestCredential(c)
			return v.Scope == hubauth.Admin || v.HostedRole == "owner" || v.HostedRole == "admin"
		}, Error: d.Error, AccessRead: func(ctx context.Context, org tracker.OrganizationID, read func(tenantstore.Reader) error) error {
			return repo.ReadTenant(ctx, org, corestore.Serving, read)
		}, RefuseImport: func(c echo.Context) error {
			return d.Error(c, &hubapi.Error{Status: http.StatusForbidden, Code: "forbidden", Message: "Importing infrastructure usage requires owner or admin access", Public: true})
		}})
	})
	return h.Echo
}
func performHubAPIRequest(t *testing.T, service http.Handler, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	var err error
	if payload != nil {
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if payload != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	service.ServeHTTP(w, r)
	return w
}
func requireNativeStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status = %d, want %d: %s", r.Code, want, r.Body.String())
	}
}
func decodeHubResponse(t *testing.T, r *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), target); err != nil {
		t.Fatal(err)
	}
}

type testOrganization struct {
	ID         tracker.OrganizationID
	Name, Slug string
}
