package harness

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenant"
	"github.com/digitaldrywood/detent/internal/cloudentry/tenantcall"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/policy"
)

const IssueContractSections = "## Acceptance criteria\nComplete the requested work.\n## Must not break\nExisting behavior.\n## How we know it worked\nRun the project checks."

func PilotPolicy() policy.Descriptor {
	return policy.Descriptor{
		SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"},
	}.WithID()
}

type Tenant struct {
	ID, Provider string
	Database     *sql.DB
	Config       hubserver.Config
	Service      *hubserver.Service
}

func NewTenant(t *testing.T, provider *FakeProvider, key ed25519.PrivateKey, id, providerID, owner, project string, configure ...func(*hubserver.Config)) (Tenant, http.Handler) {
	t.Helper()
	plans := PilotPlans()
	plans.Plans[0].Allowances["projects"] = 10
	var logs entrytest.LockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("tenant fixture logs:\n%s", logs.String())
		}
	})
	content := &ConversationArchive{}
	fixture := Tenant{ID: id, Provider: providerID, Config: hubserver.Config{InitialAdminToken: []byte(AdminKey), Logger: logger, Content: content.HandleContent, Conversation: &hubserver.ConversationConfig{Enabled: true},
		Hosted: &hubserver.HostedConfig{Plans: plans, OrganizationID: id, WorkOSOrganizationID: providerID, BootstrapSubject: owner, PublicURL: PublicURL, Provider: provider,
			StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"},
			SharedEntry: &hubserver.HostedSharedEntry{ArchiveContents: &ConversationArchive{}, Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}},
	}}
	for _, apply := range configure {
		apply(&fixture.Config)
	}
	if fixture.Config.DatabaseBackend != hubserver.DatabasePostgres {
		fixture.Database = ConfigurePostgresTenant(t, &fixture.Config)
	}
	shared, err := hubserver.Open(t.Context(), fixture.Config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shared.Close() })
	setup := tenantcall.New(tenantcall.Deps{Issuer: "entry", SigningKey: key, Now: time.Now})
	organization := tenant.Organization{ID: id, ProviderID: providerID, Name: providerID, Generation: 1}
	bootstrapJSON, err := json.Marshal(map[string]string{"organization_name": providerID})
	if err != nil {
		t.Fatal(err)
	}
	bootstrapClaims, err := setup.Claims(organization, cloudassert.KindService, http.MethodPost, "/internal/v1/owner/bootstrap", bootstrapJSON)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapClaims.Subject, bootstrapClaims.Email = owner, provider.Users[owner]
	status, bootstrapBody := SignedTenantCall(t, t.Context(), shared.Handler(), key, bootstrapClaims, bootstrapJSON, "")
	if status != http.StatusNoContent {
		t.Fatalf("bootstrap owner: status=%d body=%s", status, bootstrapBody)
	}
	identity, err := provider.Exchange(t.Context(), owner+":"+providerID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"idempotency_key": "fixture-project", "name": project, "grant_access": true})
	if err != nil {
		t.Fatal(err)
	}
	claims, err := setup.Claims(organization, cloudassert.KindBrowser, http.MethodPost, "/api/v2/organizations/"+id+"/projects", body)
	if err != nil {
		t.Fatal(err)
	}
	claims.Subject, claims.Email, claims.ProviderOrganization, claims.ProviderSession = owner, identity.Email, providerID, identity.Hosted.SessionID
	claims.SessionCreatedAt, claims.SessionExpiresAt = identity.Hosted.CreatedAt, identity.Hosted.ExpiresAt
	claims.Role, claims.AccessExpiresAt = "owner", identity.Hosted.ExpiresAt
	claims.Binding, claims.CSRF = cloudassert.AuthorizationBinding("fixture-session", id, identity.Hosted.SessionID), cloudassert.CSRFToken("fixture-csrf", id)
	status, response := SignedTenantCall(t, t.Context(), shared.Handler(), key, claims, body, claims.CSRF)
	if status != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", status, response)
	}
	fixture.Service = shared
	return fixture, shared.Handler()
}

func SocketEndpoint(name string) string {
	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		root = `C:\`
	}
	return "unix:" + filepath.Join(root, "tenants", name)
}

func CapacityPlans() *hubserver.HostedPlansConfig {
	plans := hubserver.CheckoutHostedPlans(nil)
	plans.Base = hubserver.PlanReference{ID: "free", Version: 1}
	return plans
}

func PilotPlans() *hubserver.HostedPlansConfig {
	free := map[string]int64{
		"members": 10, "projects": 1, "repositories": 10, "registered_runners": 4, "connected_runners": 4, "concurrent_work": 2,
		"api_mutations": 10000, "ingested_events": 10000, "collaboration_bytes": 64 << 20, "history_records": 10000,
	}
	plus := make(map[string]int64, len(free))
	for name, limit := range free {
		plus[name] = limit
	}
	plus["projects"] = 5
	features := []string{"collaboration", "native_execution"}
	return &hubserver.HostedPlansConfig{
		Base: hubserver.PlanReference{ID: "pilot_free", Version: 1}, WindowSeconds: 3600, RetentionWindows: 24, ConnectedSeconds: 90, InvitationSeconds: 86400,
		Plans: []hubserver.HostedPlan{
			{PlanReference: hubserver.PlanReference{ID: "pilot_free", Version: 1}, Features: features, Allowances: free},
			{PlanReference: hubserver.PlanReference{ID: "pilot_plus", Version: 1}, Features: features, Allowances: plus},
		},
	}
}

func SignedTenantCall(t *testing.T, ctx context.Context, handler http.Handler, key ed25519.PrivateKey, claims cloudassert.Claims, body []byte, csrf string) (int, []byte) {
	t.Helper()
	assertion, err := cloudassert.Sign(key, claims)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(ctx, claims.Method, "http://tenant"+claims.Path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set(cloudassert.Header, assertion)
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}
