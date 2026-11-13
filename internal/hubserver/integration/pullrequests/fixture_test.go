package pullrequests_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/githubcontract"
	"github.com/digitaldrywood/detent/internal/hubserver/githubreconcile"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/pullrequests"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

var postgresStartupTemplate = struct{ Database func(*testing.T) *sql.DB }{hubtest.Database}

type Config struct {
	hubserver.Config
	now    func() time.Time
	jitter func() float64
}
type ReconcileRequest = githubcontract.ReconcileRequest
type ReconcileSnapshot = githubcontract.ReconcileSnapshot
type RepositorySource = githubcontract.RepositorySource
type PullRequestSource = githubcontract.PullRequestSource
type PullRequestDetailSource = githubcontract.PullRequestDetailSource
type pullRequestView = pullrequests.View
type pullRequestConnector = pullrequests.Connector

const pullRequestRefreshInterval = pullrequests.RefreshInterval
const workItemRecordBody = "## Acceptance criteria\nThe issue is served.\n## Must not break\nOther tenants.\n## How we know it worked\nRead the issue."

var newNativeID = hubapi.NewID
var nativeFixtureStates = hubtest.States
var hubTestPolicy = hubtest.PolicyDescriptor
var workItemStorageUnavailable = func() error { return hubapi.ErrPostgresFeatureUnavailable }

type nativeScope struct {
	organization tracker.OrganizationID
	project      tracker.ProjectID
}

func pullRequestCacheKey(scope nativeScope, item string) string {
	return pullrequests.CacheKey(scope.organization, scope.project, item)
}
func defaultChangeReviewPolicy(p policy.Descriptor) tracker.ChangeReviewPolicy {
	r := tracker.ChangeReviewPolicy{PolicyID: p.ID, RequireReview: p.Gates.HumanReview, RequiredChecks: []tracker.ChangeCheckSpec{}}
	r.ID = changerequest.PolicyID(r)
	return r
}

func changeTestInput() tracker.ChangeVersionInput {
	artifact := func(kind string) tracker.ChangeArtifact {
		return tracker.ChangeArtifact{Kind: kind, URI: "s3://customer/change/" + kind, SHA256: policy.Digest([]byte(kind)), Availability: "available"}
	}
	return tracker.ChangeVersionInput{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("a", 40), Repository: "https://github.com/example/repo", Code: artifact("code"), Artifacts: []tracker.ChangeArtifact{artifact("manifest")}, PolicyID: hubTestPolicy().ID}
}

type fixtureProvider struct {
	auth.HostedProvider
	members map[string]auth.Membership
}

func (p *fixtureProvider) Memberships(_ context.Context, user, org string) ([]auth.Membership, error) {
	m, ok := p.members[user]
	if !ok || m.OrganizationID != org {
		return nil, nil
	}
	return []auth.Membership{m}, nil
}
func (p *fixtureProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	return identity, nil
}

type hostedSecurityUser struct {
	identity auth.Identity
	token    string
	member   hubtest.SeededMember
}
type hostedSecurityFixture struct {
	service *fixtureService
	project tracker.ProjectID
	base    string
}
type fixtureService struct {
	*hubserver.Service
	config       *Config
	repository   tenantstore.Repository
	organization tracker.OrganizationID
	pullRequests *pullrequests.Cache
	provider     *fixtureProvider
	users        map[string]hostedSecurityUser
}

func newExportPostgresFixture(t *testing.T) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	return newExportPostgresFixtureInOrganization(t, hubtest.Database(t), "org_security")
}
func newExportPostgresFixtureInOrganization(t *testing.T, admin *sql.DB, org tracker.OrganizationID, configure ...func(*Config)) (tenantstore.Repository, hostedSecurityFixture, hostedSecurityUser, *sql.DB) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	cfg := &Config{now: func() time.Time { return now }, jitter: func() float64 { return .5 }}
	for _, apply := range configure {
		apply(cfg)
	}
	provider := &fixtureProvider{members: map[string]auth.Membership{}}
	plans := hubserver.HostedPlansConfig{Base: hubserver.PlanReference{ID: "pilot_free", Version: 1}, WindowSeconds: 3600, RetentionWindows: 24, ConnectedSeconds: 90, InvitationSeconds: 86400, Plans: []hubserver.HostedPlan{{PlanReference: hubserver.PlanReference{ID: "pilot_free", Version: 1}, Features: []string{"collaboration", "native_execution", "github_integration"}, Allowances: map[string]int64{"members": 10, "projects": 10, "repositories": 10, "registered_runners": 10, "connected_runners": 10, "concurrent_work": 5, "api_mutations": 1000000, "ingested_events": 1000000, "collaboration_bytes": 64 << 20, "history_records": 10000}}}}
	cfg.Hosted = &hostedplans.HostedConfig{OrganizationID: string(org), WorkOSOrganizationID: "org_provider", PublicURL: "https://hub.example.test", Provider: provider, Plans: &plans}
	cfg.Clock = func() time.Time { return cfg.now() }
	cfg.WebhookMaintenanceInterval = time.Hour
	cfg.DatabaseBackend = hubserver.DatabasePostgres
	config := hubtest.PostgresConfig(t, admin, hubtest.WithMaxConnections(4))
	cfg.Postgres = &config
	cfg.RunDetails = hubtest.NewDetails()
	keys, err := hubsecrets.FromEnvironment(func(name string) string {
		switch name {
		case "DETENT_HUB_SECRET_KEYS":
			return `{"1":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x6b}, 32)) + `"}`
		case "DETENT_HUB_SECRET_KEY_VERSION":
			return "1"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg.SecretKeys = keys
	repository := hubtest.RepositoryOn(t, admin, hubtest.WithMaxConnections(4), hubtest.WithClock(cfg.Clock))
	now = cfg.now()
	err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example", CreatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: org, ProviderID: "org_provider", PublicURL: cfg.Hosted.PublicURL, Deployment: "origin"}); err != nil {
			return err
		}
		cursor, err := keys.Seal(bytes.Repeat([]byte{0x36}, 32), accessstore.SecretAAD(org, "", accessstore.NativeCursorKeyKind))
		if err != nil {
			return err
		}
		return w.StoreAccessSecret(t.Context(), accessstore.Secret{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Envelope: cursor, UpdatedAt: now}, accessstore.SecretAudit{OrganizationID: org, Kind: accessstore.NativeCursorKeyKind, Actor: "migration", Event: "import", KeyVersion: cursor.Version, RecordedAt: now})
	})
	if err != nil {
		t.Fatal(err)
	}
	project := hubtest.Project(t, repository, org, hubtest.ProjectOptions{ID: "project_example", Name: "Example", States: hubtest.States()})
	hubtest.Policy(t, repository, cfg.RunDetails, project, hubtest.PolicyOptions{Now: now})
	rootcfg := cfg.Config
	rootcfg.ReconcileBackend = nil
	rootcfg.ImportBackend = nil
	service, err := hubserver.Open(t.Context(), rootcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	s := &fixtureService{Service: service, config: cfg, repository: repository, organization: org, pullRequests: pullrequests.NewCache(), provider: provider, users: map[string]hostedSecurityUser{}}
	f := hostedSecurityFixture{service: s, project: project.ID, base: "/api/v2/organizations/" + string(org) + "/projects/" + string(project.ID)}
	owner := f.user(t, "review_admin", "owner", "review-admin@example.test", "", "")
	return repository, f, owner, admin
}
func (f hostedSecurityFixture) user(t *testing.T, name, role, email, grant, support string) hostedSecurityUser {
	t.Helper()
	s := f.service
	now := s.config.now()
	member := hubtest.Member(t, s.repository, s.organization, hubtest.MemberOptions{Role: role, Email: email, Projects: []tracker.ProjectID{f.project}, Now: now})
	m := auth.Membership{ID: member.Member.MembershipID, UserID: member.Member.UserID, OrganizationID: "org_provider", Status: "active"}
	m.Role.Slug = role
	s.provider.members[m.UserID] = m
	token := hubapi.NewID("session")
	err := s.repository.WriteTenant(t.Context(), s.organization, corestore.Serving, func(w tenantstore.Writer) error {
		return w.CreateAccessSession(t.Context(), accessstore.Session{OrganizationID: s.organization, Hash: apikey.HashToken(token), Email: email, Subject: m.UserID, ProviderOrganizationID: "org_provider", SessionID: hubapi.NewID("provider_session"), IdentityCreatedAt: now, IdentityExpiresAt: now.Add(time.Hour), CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	})
	if err != nil {
		t.Fatal(err)
	}
	user := hostedSecurityUser{identity: auth.Identity{Subject: m.UserID, Email: email, EmailVerified: true}, token: token, member: member}
	s.users[m.UserID] = user
	return user
}
func (f hostedSecurityFixture) rootRequest(t *testing.T, user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(t.Context())
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: "detent_hosted_session", Value: user.token})
	sum := sha256.Sum256([]byte("detent-hosted-csrf:" + user.token))
	request.Header.Set("X-CSRF-Token", hex.EncodeToString(sum[:]))
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}
func (f hostedSecurityFixture) request(t *testing.T, user hostedSecurityUser, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	clean, query, _ := strings.Cut(path, "?")
	if !strings.HasSuffix(clean, "/pull-requests") {
		return f.rootRequest(t, user, method, path, body)
	}
	parts := strings.Split(clean, "/")
	item := parts[len(parts)-2]
	refresh := ""
	if query != "" {
		if !strings.HasPrefix(query, "refresh=") {
			return featureResponse(t, nil, hubapi.PublicInvalid("Unknown query parameter"))
		}
		refresh = strings.TrimPrefix(query, "refresh=")
	}
	if refresh != "" && refresh != "1" {
		return featureResponse(t, nil, hubapi.PublicInvalid("refresh accepts only 1"))
	}
	scope := hubtest.HostedScope(t, hubtest.HostedFacts{Member: user.member, Project: f.project, ProviderRole: user.member.Member.Role, Now: f.service.config.now})
	views, err := f.feature(t, user, scope).Read(t.Context(), scope, item, refresh)
	if errors.Is(err, corestore.ErrRevisionConflict) {
		err = hubapi.Conflict(0)
	}
	return featureResponse(t, views, err)
}
func featureResponse(t *testing.T, value any, err error) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	code := http.StatusOK
	if err != nil {
		var failure *hubapi.Error
		if errors.As(err, &failure) {
			code = failure.Status
			value = failure
		} else if errors.Is(err, corestore.ErrNotFound) {
			code = http.StatusNotFound
			value = hubapi.NotFound()
		} else if errors.Is(err, accessstore.ErrDenied) {
			code = http.StatusForbidden
			value = map[string]string{"code": "access_denied"}
		} else {
			t.Fatal(err)
		}
	}
	response.WriteHeader(code)
	if err := json.NewEncoder(response).Encode(value); err != nil {
		t.Fatal(err)
	}
	return response
}
func (f hostedSecurityFixture) feature(t *testing.T, user hostedSecurityUser, scope hubapi.Scope) *pullrequests.Service {
	t.Helper()
	s := f.service
	read := func(path string, out any) error {
		r := f.rootRequest(t, user, http.MethodGet, path, nil)
		if r.Code != http.StatusOK {
			var e hubapi.Error
			if err := json.Unmarshal(r.Body.Bytes(), &e); err != nil {
				return err
			}
			e.Status = r.Code
			return &e
		}
		return json.Unmarshal(r.Body.Bytes(), out)
	}
	base := "/api/v2/organizations/" + string(scope.Organization) + "/projects/" + string(scope.Project)
	return pullrequests.New(pullrequests.Deps{Repository: s.repository, Now: s.config.now, Cache: s.pullRequests, TransportAvailable: s.config.ReconcileBackend != nil, Unavailable: hubapi.ErrPostgresFeatureUnavailable, ValidCheckout: runnerstore.ValidCheckoutRepository,
		VerifiedCheckout: func(ctx context.Context, repository string, now time.Time) (bool, error) {
			var v bool
			err := s.repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				v, err = r.RunnerCheckoutVerified(ctx, scope.Project, repository, now)
				return err
			})
			return v, err
		},
		ReadItem: func(ctx context.Context, item string) error {
			var issue tracker.NativeIssue
			return read(base+"/work-items/"+item, &issue)
		},
		ChangeDetail: func(ctx context.Context, item, id string, now time.Time) (tracker.ChangeDetail, error) {
			var d tracker.ChangeDetail
			err := read(base+"/work-items/"+item+"/changes/"+id, &d)
			return d, err
		},
		Changes: func(ctx context.Context, item string) ([]tracker.ChangeRequest, error) {
			var c []tracker.ChangeRequest
			err := read(base+"/work-items/"+item+"/changes", &c)
			return c, err
		},
		AuthorizeItem: func(ctx context.Context, r tenantstore.Reader, item tracker.NativeWorkItemID) error {
			if err := scope.Authorize(ctx, r, false); err != nil {
				return err
			}
			_, err := r.WorkItem(ctx, scope.Project, item)
			return err
		},
		ReleaseDelivery: func(ctx context.Context, item string) (tracker.ReleaseDelivery, bool, error) {
			var d tracker.ReleaseDelivery
			found := false
			err := s.repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
				row, err := r.ReleaseDelivery(ctx, scope.Project, tracker.NativeWorkItemID(item))
				if errors.Is(err, corestore.ErrNotFound) {
					return nil
				}
				if err != nil {
					return err
				}
				d = row.ReleaseDelivery
				found = true
				return nil
			})
			return d, found, err
		},
	})
}
func (s *fixtureService) runDetailStore() attachment.DetailStore { return s.config.RunDetails }

func (s *fixtureService) maintainGitHubReconciliationRecords(ctx context.Context) error {
	return githubreconcile.New(githubreconcile.Deps{Repository: s.repository, Organization: s.organization, Backend: s.config.ReconcileBackend, Importer: s.config.ImportBackend, Ready: true, Unavailable: hubapi.ErrPostgresFeatureUnavailable, Now: s.config.now, Jitter: s.config.jitter, Interval: time.Minute, Integration: func(ctx context.Context, org tracker.OrganizationID, project tracker.ProjectID) (githubreconcile.Integration, error) {
		var record projectstore.Record
		err := s.repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			record, err = r.ProjectRecord(ctx, project)
			return err
		})
		return githubreconcile.Integration{Repository: record.Settings.CheckoutRepository}, err
	}}).Maintain(ctx)
}
func requireNativeStatus(t *testing.T, r *httptest.ResponseRecorder, want int) {
	t.Helper()
	if r.Code != want {
		t.Fatalf("status = %d, want %d: %s", r.Code, want, r.Body.String())
	}
}
func requireNativeCode(t *testing.T, r *httptest.ResponseRecorder, want int, code string) {
	t.Helper()
	requireNativeStatus(t, r, want)
	var body struct{ Code string }
	decodeHubResponse(t, r, &body)
	if body.Code != code {
		t.Fatalf("code=%s want=%s", body.Code, code)
	}
}
func decodeHubResponse(t *testing.T, r *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), out); err != nil {
		t.Fatal(err)
	}
}

func drainTestNativeBoard(t *testing.T, service *fixtureService) {
	t.Helper()
	organization := service.organization
	var current []tracker.NativeWorkItemID
	for {
		var candidate viewstore.BoardCandidate
		var pending bool
		err := service.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			candidate, err = r.NextBoardCandidate(t.Context(), "", current...)
			if errors.Is(err, corestore.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			item, err := r.WorkItem(t.Context(), candidate.ProjectID, candidate.WorkItemID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			expected := err == nil
			project, err := r.Project(t.Context(), candidate.ProjectID)
			if err != nil && !errors.Is(err, corestore.ErrNotFound) {
				return err
			}
			expected = expected && err == nil && project.DeletedAt == nil && project.Profile == "native"
			row, err := r.BoardItem(t.Context(), candidate.ProjectID, candidate.WorkItemID)
			if errors.Is(err, corestore.ErrNotFound) {
				pending = expected || candidate.Required
				return nil
			}
			if err != nil {
				return err
			}
			pending = !expected || candidate.Required || !row.Display.Ready || row.PageIssue == nil || row.ListIssue == nil ||
				row.SourceRevision != item.Revision || row.SourceEventSequence != item.EventSequence ||
				row.SourceProjectRevision != project.ContentSettingsRevision || row.RefreshAfter != nil && !row.RefreshAfter.After(service.config.now())
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if candidate.WorkItemID == "" {
			return
		}
		if !pending {
			current = append(current, candidate.WorkItemID)
			continue
		}
		if _, _, err := service.MaintainBoardRecord(hubmaintenance.Budget{Items: hubmaintenance.ItemLimit, Bytes: hubmaintenance.ByteLimit}.Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1}), organization, "", current...); err != nil {
			t.Fatal(err)
		}
		current = current[:0]
	}
}

func (f hostedSecurityFixture) scope(t *testing.T, user hostedSecurityUser) hubapi.Scope {
	return hubtest.HostedScope(t, hubtest.HostedFacts{Member: user.member, Project: f.project, ProviderRole: user.member.Member.Role, Now: f.service.config.now})
}

type linkedTestImporter struct{}

func (linkedTestImporter) FetchImportPage(context.Context, githubcontract.GitHubImportRequest) (githubcontract.GitHubImportPage, error) {
	return githubcontract.GitHubImportPage{}, errors.New("unexpected linked source read")
}

var Permanent = githubcontract.Permanent
var IsPermanent = githubcontract.IsPermanent
