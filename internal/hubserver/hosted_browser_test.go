package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/hubclient"
	policypkg "github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacerunner"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

// browserHostedOrganization is the preview hub's one organization.
const browserHostedOrganization = "org_browser_preview"

// browserConversationReply is the canned coordinator answer the browser
// fixture streams back, split into the deltas a real turn would emit.
var browserConversationReply = []string{
	"The renewal returns before the handoff is acknowledged, ",
	"so the lease can lapse under load. ",
	"Moving the renewal behind the acknowledgement closes the window.",
}

// browserConversationBackend scripts coordinator turns for the browser
// fixture. Every turn streams the same two sentences as deltas so a browser
// sees real streaming, and a prompt mentioning attention first calls the
// read-only list_attention tool so the tool path is exercised too. It never
// starts a process, so the fixture needs no provider.
type browserConversationBackend struct {
	turns atomic.Int64
}

func (b *browserConversationBackend) RunTurn(ctx context.Context, request runner.AgentTurnRequest, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	return b.RunTurnWithTools(ctx, request, nil, nil, onUpdate)
}

func (b *browserConversationBackend) RunTurnWithTools(ctx context.Context, request runner.AgentTurnRequest, _ []runner.AgentTool, handle runner.AgentToolHandler, onUpdate runner.AgentUpdateHandler) (runner.AgentTurnResult, error) {
	turn := b.turns.Add(1)
	if handle != nil && strings.Contains(strings.ToLower(request.Prompt), "attention") {
		if _, err := handle(ctx, runner.AgentToolCall{Name: coordinatorToolListAttention, Arguments: json.RawMessage(`{"scope":"project"}`)}); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	for _, delta := range browserConversationReply {
		if err := onUpdate(runner.AgentUpdate{Type: runner.AgentUpdateMessageDelta, Delta: delta}); err != nil {
			return runner.AgentTurnResult{}, err
		}
	}
	return runner.AgentTurnResult{ThreadID: "thread_browser_preview", TurnID: fmt.Sprintf("turn_browser_%d", turn)}, nil
}

type browserHostedProvider struct {
	mu             sync.Mutex
	base           string
	organization   auth.Organization
	members        map[string]auth.Membership
	sessions       map[string]auth.HostedIdentity
	invitations    map[string]auth.Invitation
	inviteRoles    map[string]string
	authorizations map[string]string
	codes          map[string]auth.Identity
	sequence       int
}

func (p *browserHostedProvider) AuthorizationURL(state, _ string, verifier string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.authorizations[state] = verifier
	return p.base + "/__preview/authorize?state=" + url.QueryEscape(state)
}

func (p *browserHostedProvider) Exchange(_ context.Context, code, verifier, nonce string) (auth.Identity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	identity, ok := p.codes[code]
	if !ok || verifier == "" || p.authorizations[nonce] != verifier {
		return auth.Identity{}, auth.ErrHostedIdentity
	}
	delete(p.codes, code)
	delete(p.authorizations, nonce)
	return identity, nil
}

func (p *browserHostedProvider) CurrentSession(_ context.Context, identity auth.HostedIdentity) (auth.HostedIdentity, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok := p.sessions[identity.SessionID]
	if !ok || !current.ExpiresAt.After(time.Now()) {
		return auth.HostedIdentity{}, auth.ErrHostedIdentity
	}
	return current, nil
}

func (p *browserHostedProvider) Memberships(_ context.Context, user, organization string) ([]auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var members []auth.Membership
	for _, member := range p.members {
		if (user == "" || member.UserID == user) && (organization == "" || member.OrganizationID == organization) {
			members = append(members, member)
		}
	}
	return members, nil
}

func (p *browserHostedProvider) Organization(_ context.Context, id string) (auth.Organization, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id != p.organization.ID {
		return auth.Organization{}, auth.ErrHostedIdentity
	}
	return p.organization, nil
}

func (p *browserHostedProvider) CreateOrganization(_ context.Context, externalID, name string) (auth.Organization, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.organization = auth.Organization{ID: "org_browser_provider", ExternalID: externalID, Name: name}
	return p.organization, nil
}

func (p *browserHostedProvider) CreateMembership(_ context.Context, user, organization, role string) (auth.Membership, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: organization, Status: "active"}
	member.Role.Slug = role
	p.members[member.ID] = member
	return member, nil
}

func (p *browserHostedProvider) SetMembershipRole(_ context.Context, id, role string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	member, ok := p.members[id]
	if !ok {
		return auth.ErrHostedIdentity
	}
	member.Role.Slug = role
	p.members[id] = member
	return nil
}

func (p *browserHostedProvider) RevokeMembership(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.members, id)
	return nil
}

func (p *browserHostedProvider) Invite(_ context.Context, organization, email, role, _ string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	invitation := auth.Invitation{ID: fmt.Sprintf("invitation_browser_%d", p.sequence), Email: email, OrganizationID: organization, State: "pending", ExpiresAt: time.Now().Add(time.Hour)}
	p.invitations[invitation.ID], p.inviteRoles[invitation.ID] = invitation, role
	return invitation, nil
}

func (p *browserHostedProvider) Invitation(_ context.Context, token string) (auth.Invitation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok {
		return auth.Invitation{}, auth.ErrHostedIdentity
	}
	return invitation, nil
}

func (p *browserHostedProvider) AcceptInvitation(_ context.Context, token, user string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	invitation, ok := p.invitations[token]
	if !ok || invitation.State != "pending" {
		return auth.ErrHostedIdentity
	}
	invitation.State, invitation.AcceptedUserID = "accepted", user
	p.invitations[token] = invitation
	member := auth.Membership{ID: "membership_" + user, UserID: user, OrganizationID: invitation.OrganizationID, Status: "active"}
	member.Role.Slug = p.inviteRoles[token]
	p.members[member.ID] = member
	return nil
}

func (p *browserHostedProvider) RevokeSession(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, id)
	return nil
}

func (p *browserHostedProvider) identity(user, email, organization, support string) auth.Identity {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sequence++
	now := time.Now().UTC()
	hosted := auth.HostedIdentity{Subject: user, OrganizationID: organization, SessionID: fmt.Sprintf("session_browser_%d", p.sequence), CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(browserHostedSessionTTL()), SupportActor: support}
	if support != "" {
		hosted.SupportReason = "customer-request"
	}
	p.sessions[hosted.SessionID] = hosted
	return auth.Identity{Subject: user, Email: email, EmailVerified: true, Hosted: &hosted}
}

// browserHostedSessionTTL is how long a fixture session lives. The tests
// want a short one; a preview that a person opens over hours needs its
// sessions to outlive the preview, or every page dies with "a hosted session
// is required" fifteen minutes after the hub started.
func browserHostedSessionTTL() time.Duration {
	if os.Getenv("DETENT_HOSTED_BROWSER_PREVIEW") != "1" {
		return 15 * time.Minute
	}
	return browserPreviewDuration() + time.Hour
}

// browserPreviewDuration reads how long the preview holds the hub open.
func browserPreviewDuration() time.Duration {
	duration := 5 * time.Minute
	if value := os.Getenv("DETENT_HOSTED_BROWSER_PREVIEW_DURATION"); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed > 0 {
			duration = parsed
		}
	}
	return duration
}

// browserPreviewConversationBackend chooses the fixture's hub-side coordinator
// backend. The preview answers an unlinked chat with a scripted backend, which
// is what the Playwright specs assert against and what the default preview
// keeps. Setting DETENT_HOSTED_BROWSER_PREVIEW_COORDINATOR=runner configures no
// backend at all, so the hub has no coordinator of its own, an unlinked chat
// creates a `detent:coordinator` work item and an enrolled runner claims it
// (decisions section 9). That is the only way to exercise the
// runner-dispatched coordinator without a real WorkOS organization; the
// September 11, 2026 dogfood run could not exercise it because the fixture
// hard-coded the scripted backend.
func browserPreviewConversationBackend() runner.AgentBackend {
	if browserPreviewRunnerCoordinator() {
		return nil
	}
	return &browserConversationBackend{}
}

// browserPreviewRunnerCoordinator reports whether the preview was asked to
// dispatch coordinator turns to an enrolled runner.
func browserPreviewRunnerCoordinator() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("DETENT_HOSTED_BROWSER_PREVIEW_COORDINATOR")), "runner")
}

const browserHostedOrganizationBase = "/api/v2/organizations/org_browser_preview"

// browserHostedOwnerEmail is the address the fixture's organization owner signs
// in with. It is a constant rather than a literal in three places because the
// browser fixture publishes it and `tests/visual/account.spec.js` picks the
// owner's row out of the members table by it.
const browserHostedOwnerEmail = "owner@example.test"

// TestBrowserPreviewConversationBackendOptsOutForRunner proves the preview can
// be started without a hub-side coordinator. With no backend the hub creates a
// `detent:coordinator` work item for an unlinked chat instead of answering it
// itself, which is the only way to exercise decisions section 9 locally. The
// default and the Playwright specs keep the scripted backend.
func TestBrowserPreviewConversationBackendOptsOutForRunner(t *testing.T) {
	for _, test := range []struct {
		name     string
		value    string
		scripted bool
	}{
		{name: "unset keeps the scripted backend", value: "", scripted: true},
		{name: "runner configures no hub backend", value: "runner"},
		{name: "runner ignores case and spacing", value: "  Runner ", scripted: false},
		{name: "any other value keeps the scripted backend", value: "hub", scripted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DETENT_HOSTED_BROWSER_PREVIEW_COORDINATOR", test.value)

			backend := browserPreviewConversationBackend()
			if test.scripted {
				if _, ok := backend.(*browserConversationBackend); !ok {
					t.Fatalf("backend = %#v, want the scripted browser backend", backend)
				}
				return
			}
			if backend != nil {
				t.Fatalf("backend = %#v, want no hub backend so the chat dispatches to a runner", backend)
			}
		})
	}
}

type browserHostedFixture struct {
	service        *Service
	server         *httptest.Server
	provider       *browserHostedProvider
	cookies        map[string]*http.Cookie
	project        string
	privateProject string
	// conversation is the pre-created conversation linked to workItem; both
	// are empty until the allocated fixture finishes its handoff.
	conversation string
	workItem     string
	// pullRequest is the number of the seeded pull request the panel draws
	// for workItem (decisions section 18.6); zero until the allocated fixture
	// has seeded it.
	pullRequest int
	// workspace is the pre-warmed files, exec and git workspace on workItem. It is
	// published in the fixture because a spec that wants to exercise the API
	// alone -- queue an action run with no browser attached and read it back
	// afterwards (decisions section 18.12) -- needs a workspace id and has no
	// other way to learn one without driving the panel first, which is the very
	// thing such a spec is proving unnecessary.
	workspace string
	// workspaceRunnerCredential and workspaceRunnerMachine name the enrolled
	// runner whose lane serves the workspace surfaces, so the preview can go
	// on beating for it. They are empty until seedWorkspaceRunner has run.
	//
	// They are kept because the claim gate reads the runner's own heartbeat:
	// section 18.1 offers a workspace item only to a runner that "reports
	// every capability in requires with a fresh heartbeat", and that report
	// rides the machine beat. One beat at seed time makes the runner eligible
	// for runnerauth.HeartbeatTimeout and no longer, which is why the preview
	// has to keep sending them rather than seed once and trust the lane.
	workspaceRunnerCredential string
	workspaceRunnerMachine    tracker.MachineID
	// prewarmed is the workspace seedWorkspaceRunner opened and took to ready,
	// so a test can close it and watch the next request be claimed.
	prewarmed workspacesession.Session
	// attemptDiff is the attempt whose stored diff the Diff surface draws for
	// workItem (decisions section 18.5); empty until seedAttemptDiff has run.
	attemptDiff string
	stop        chan struct{}
	stopOnce    sync.Once
}

func newBrowserHostedFixture(t *testing.T, allocated bool) *browserHostedFixture {
	t.Helper()
	server := httptest.NewUnstartedServer(http.NotFoundHandler())
	base := "http://" + server.Listener.Addr().String()
	provider := &browserHostedProvider{
		base: base, organization: auth.Organization{ID: "org_browser_provider", ExternalID: "org_browser_preview", Name: "Browser organization"},
		members: make(map[string]auth.Membership), sessions: make(map[string]auth.HostedIdentity), invitations: make(map[string]auth.Invitation), inviteRoles: make(map[string]string), authorizations: make(map[string]string), codes: make(map[string]auth.Identity),
	}
	previewTerminalRecording := true
	cfg := Config{InitialAdminToken: []byte(testHubAdminToken), DatabasePath: filepath.Join(t.TempDir(), "hosted-browser.db"), GitHubDisabled: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Hosted: &HostedConfig{
		OrganizationID: "org_browser_preview", BootstrapSubject: "user_browser_owner", PublicURL: base, Provider: provider,
		StaffEmails: []string{"staff@example.test", "support@example.test"}, SupportActors: []string{"support@example.test"},
		Directory: []HostedDestination{{OrganizationID: "org_browser_preview", WorkOSOrganizationID: "org_browser_provider", PublicURL: base}},
	}, Conversation: &ConversationConfig{Enabled: true, Backend: browserPreviewConversationBackend(), Workspace: t.TempDir()},
		// The Files surface needs a workspace to attach to (decisions section
		// 18.1). The preview enables the feature; seedWorkspaceRunner supplies
		// the runner half, so the panel reaches ready rather than failing with
		// no_runner five minutes later.
		//
		// The terminal is on, at user isolation (section 18.3). The preview's
		// runner is this process, which ships no container runtime hook of any
		// kind, so user is the level it can actually serve and reporting the
		// other one would be reporting a boundary that is not there. The
		// preview's owner account is an owner, which is exactly who that level
		// allows, and it holds the runners grant seedWorkspaceRunner gives it.
		Workspace: &WorkspaceConfig{Enabled: true, Terminal: WorkspaceTerminalConfig{
			Enabled: true, Isolation: workspacesession.IsolationUser, Record: &previewTerminalRecording,
		}},
		// The price table the usage report estimates with (section 17.5).
		Usage: &UsageConfig{Currency: "USD", Prices: map[string]UsagePrice{
			"gpt-6-astra":   {Input: 1.25, CachedInput: 0.125, Output: 10},
			"claude-opus-5": {Input: 5, CachedInput: 0.5, Output: 25},
		}}}
	if allocated {
		cfg.Hosted.WorkOSOrganizationID = provider.organization.ID
	}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	fixture := &browserHostedFixture{service: service, server: server, provider: provider, cookies: make(map[string]*http.Cookie), stop: make(chan struct{})}
	t.Cleanup(func() {
		server.Close()
		if err := fixture.service.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, account := range []struct {
		name, user, email, role, support string
	}{
		{name: "owner", user: "user_browser_owner", email: browserHostedOwnerEmail, role: "owner"},
		{name: "viewer", user: "user_browser_viewer", email: "viewer@example.test", role: "viewer"},
		{name: "staff", user: "user_browser_staff", email: "staff@example.test"},
		{name: "support-staff", user: "user_browser_support", email: "support@example.test"},
		{name: "support-viewer", user: "user_browser_viewer", email: "viewer@example.test", support: "support@example.test"},
		{name: "invitee", user: "user_browser_invitee", email: "invitee@example.test"},
		{name: "wrong-organization", user: "user_browser_owner", email: browserHostedOwnerEmail},
		{name: "revoked", user: "user_browser_viewer", email: "viewer@example.test"},
		{name: "expired", user: "user_browser_viewer", email: "viewer@example.test"},
	} {
		organization := ""
		if allocated && (account.role != "" || account.support != "" || account.name == "revoked" || account.name == "expired") {
			organization = provider.organization.ID
		}
		if account.name == "wrong-organization" {
			organization = "org_browser_other"
		}
		identity := provider.identity(account.user, account.email, organization, account.support)
		if allocated && account.role != "" {
			membership, err := provider.CreateMembership(t.Context(), account.user, organization, account.role)
			if err != nil {
				t.Fatal(err)
			}
			if account.name == "owner" {
				err = service.bootstrapHostedMember(t.Context(), identity)
			} else {
				err = service.addHostedMember(t.Context(), identity, membership)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		token, session, err := service.hostedSessions.CreateIdentitySession(t.Context(), identity)
		if err != nil {
			t.Fatal(err)
		}
		fixture.cookies[account.name] = &http.Cookie{Name: hostedCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt}
		if account.name == "revoked" {
			if err := provider.RevokeSession(t.Context(), identity.Hosted.SessionID); err != nil {
				t.Fatal(err)
			}
		}
		if account.name == "expired" {
			provider.mu.Lock()
			identity.Hosted.ExpiresAt = time.Now().Add(-time.Second)
			provider.sessions[identity.Hosted.SessionID] = *identity.Hosted
			provider.mu.Unlock()
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__preview/account/{account}", func(w http.ResponseWriter, r *http.Request) {
		cookie, ok := fixture.cookies[r.PathValue("account")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		http.SetCookie(w, cookie)
		http.Redirect(w, r, "/organization", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /__preview/authorize", func(w http.ResponseWriter, r *http.Request) {
		state := r.URL.Query().Get("state")
		provider.mu.Lock()
		organization := provider.organization.ID
		provider.mu.Unlock()
		identity := provider.identity("user_browser_owner", "owner@example.test", organization, "")
		provider.mu.Lock()
		code := "code_" + identity.Hosted.SessionID
		provider.codes[code] = identity
		provider.mu.Unlock()
		http.Redirect(w, r, "/auth/oidc/callback?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(state), http.StatusSeeOther)
	})
	mux.HandleFunc("POST /__preview/stop", func(w http.ResponseWriter, _ *http.Request) {
		fixture.stopOnce.Do(func() { close(fixture.stop) })
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", service.Handler())
	server.Config.Handler = mux
	server.Start()
	if allocated {
		fixture.project = fixture.createProject(t, "Browser collaboration")
		fixture.privateProject = fixture.createProject(t, "Owner private project")
		fixture.grant(t, "owner", "user_browser_viewer", fixture.project, false, false, false)
		payload, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "browser-preview-issue"}, Title: "Review the invitation flow", Body: "Browser fixture private body", State: "Todo"})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, base+browserHostedOrganizationBase+"/projects/"+fixture.project+"/work-items", strings.NewReader(string(payload)))
		request.Header.Set("Content-Type", "application/json")
		cookie := fixture.cookies["owner"]
		if cookie == nil {
			t.Fatal("owner session cookie is missing")
		}
		request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		service.Handler().ServeHTTP(response, request)
		browserHostedStatus(t, response, http.StatusOK)
		fixture.seedConversation(t)
		fixture.seedUsage(t)
	}
	return fixture
}

// seedPullRequest gives the linked issue one pull request to draw (decisions
// section 18.6): a change request with an external reference, an approving
// native review, and a connector projection the panel joins it with. The
// connector half is written straight to the projection the webhook ingest and
// the reconciler maintain, because a preview hub has no GitHub credential --
// the point of the fixture is the shape the client reads, not where the bytes
// came from.
//
// It is called by the tests that want a pull request rather than by the
// fixture itself: it approves a project policy and adds a change request, and
// every other hosted browser assertion is written against a project that has
// neither.
func (f *browserHostedFixture) seedPullRequest(t *testing.T) {
	t.Helper()
	base := browserHostedOrganizationBase + "/projects/" + f.project
	var change struct {
		ID string `json:"change_id"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/work-items/"+f.workItem+"/changes", map[string]any{
		"idempotency_key": "browser-preview-change",
		"title":           "Renew the lease before the handoff completes",
		"body":            "Move the lease renewal behind the handoff acknowledgement.",
	}, http.StatusOK), &change)
	now := f.service.config.now().UTC().Format(time.RFC3339Nano)
	head := strings.Repeat("b", 40)
	result, err := f.service.database.db.ExecContext(t.Context(),
		"INSERT INTO repositories (github_node_id, github_owner, github_name, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		"R_browser_preview", "digitaldrywood", "detent", now, now)
	if err != nil {
		t.Fatalf("seed repository: %v", err)
	}
	repositoryID, err := result.LastInsertId()
	if err != nil {
		t.Fatalf("repository id: %v", err)
	}
	// Inserting a repository auto-creates its github_compatible alias project
	// (migration 00008). The hosted project is the one that owns this
	// repository, and an extra project would consume a hosted allowance the
	// fixture never asked for, so the alias is removed rather than kept.
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"DELETE FROM projects WHERE repository_id = ? AND id <> ?", repositoryID, f.project); err != nil {
		t.Fatalf("remove repository alias project: %v", err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE projects SET repository_id = ?, github_repository_enabled = 1 WHERE id = ?", repositoryID, f.project); err != nil {
		t.Fatalf("bind repository: %v", err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(),
		`INSERT INTO pull_requests (repository_id, issue_id, github_node_id, github_number, title, url, github_state, draft,
head_ref, head_sha, base_ref, base_sha, mergeable_state, checks_summary_json, reviews_summary_json,
source_version, source_updated_at, synchronized_at, created_at, updated_at)
VALUES (?, (SELECT id FROM issues WHERE native_id = ?), ?, ?, ?, ?, 'open', 0, ?, ?, 'main', ?, 'clean',
'{"status":"completed","conclusion":"success","total":2,"passed":2}', '{"decision":"approved","approvals":1}',
'v1', ?, ?, ?, ?)`,
		repositoryID, f.workItem, "PR_browser_preview", 4211, "Renew the lease before the handoff completes",
		"https://github.com/digitaldrywood/detent/pull/4211", "detent/digitaldrywood_detent_4180", head,
		strings.Repeat("a", 40), now, now, now, now); err != nil {
		t.Fatalf("seed pull request: %v", err)
	}
	// Publishing a change version needs the project's approved policy. A
	// hosted hub has no API that approves one -- policy approval is an
	// instance-administrator action a hosted session cannot take -- so the
	// preview approves it in the store, which is what the operator of a real
	// hub would already have done before any change existed.
	approved := hubTestPolicy()
	metadata, err := json.Marshal(approved)
	if err != nil {
		t.Fatalf("encode preview policy: %v", err)
	}
	scope := "org_browser_preview/" + f.project
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"INSERT INTO policy_revisions (scope, policy_id, metadata_json, approved_by, approved_at) VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING",
		scope, approved.ID, string(metadata), "preview", now); err != nil {
		t.Fatalf("seed policy revision: %v", err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"INSERT INTO project_policies (scope, policy_id) VALUES (?, ?) ON CONFLICT (scope) DO UPDATE SET policy_id = excluded.policy_id",
		scope, approved.ID); err != nil {
		t.Fatalf("seed project policy: %v", err)
	}
	// The review policy the change is published under: human review, no
	// required checks, so one approval is the whole story the preview needs.
	rules := tracker.ChangeReviewPolicy{PolicyID: approved.ID, RequireReview: true, RequiredChecks: []tracker.ChangeCheckSpec{}}
	rules.ID = changerequest.PolicyID(rules)
	encodedRules, err := json.Marshal(rules)
	if err != nil {
		t.Fatalf("encode preview review policy: %v", err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(),
		`INSERT INTO change_review_policies (organization_id, project_id, policy_json) VALUES (?, ?, ?)
ON CONFLICT (organization_id, project_id) DO UPDATE SET policy_json = excluded.policy_json`,
		"org_browser_preview", f.project, string(encodedRules)); err != nil {
		t.Fatalf("seed review policy: %v", err)
	}
	var version struct {
		ID string `json:"version_id"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/work-items/"+f.workItem+"/changes/"+change.ID+"/versions", map[string]any{
		"idempotency_key": "browser-preview-version",
		"base_sha":        strings.Repeat("a", 40),
		"head_sha":        head,
		"merge_base_sha":  strings.Repeat("a", 40),
		"repository":      "https://github.com/digitaldrywood/detent",
		"code":            map[string]any{"kind": "code", "uri": "s3://customer/change/code", "sha256": changeTestArtifact("code").SHA256, "availability": "available"},
		"artifacts":       []any{},
		"policy_id":       approved.ID,
		"external":        map[string]any{"provider": "github", "id": "4211", "url": "https://github.com/digitaldrywood/detent/pull/4211"},
	}, http.StatusOK), &version)
	f.api(t, "owner", http.MethodPost, base+"/work-items/"+f.workItem+"/changes/"+change.ID+"/versions/"+version.ID+"/reviews", map[string]any{
		"idempotency_key": "browser-preview-review",
		"decision":        "approved",
	}, http.StatusOK)
	f.pullRequest = 4211
}

// browserAttemptDiffFiles is the worktree the seeded attempt reports: one
// modified file with a hunk, one added file, and one denylisted path. The
// third is the point of the set -- section 18.5 keeps a denied file in the
// list with its counts and without its contents, so a reader is told that
// .env.local changed and is not shown what it now says.
var browserAttemptDiffFiles = []tracker.AttemptDiffFile{
	{
		Path: "main.go", Status: tracker.DiffStatusModified, Additions: 1, Deletions: 0,
		Patch: "diff --git a/main.go b/main.go\n" +
			"index 1a2b3c4..5d6e7f8 100644\n" +
			"--- a/main.go\n" +
			"+++ b/main.go\n" +
			"@@ -1,5 +1,6 @@\n" +
			" package main\n" +
			" \n" +
			" func main() {\n" +
			"+\trenewLease()\n" +
			" \tserve()\n" +
			" }\n",
	},
	{
		Path: "internal/hubserver/renewal.go", Status: tracker.DiffStatusAdded, Additions: 4, Deletions: 0,
		Patch: "diff --git a/internal/hubserver/renewal.go b/internal/hubserver/renewal.go\n" +
			"new file mode 100644\n" +
			"index 0000000..9f86d08\n" +
			"--- /dev/null\n" +
			"+++ b/internal/hubserver/renewal.go\n" +
			"@@ -0,0 +1,4 @@\n" +
			"+package hubserver\n" +
			"+\n" +
			"+// renewLease renews before the handoff is acknowledged.\n" +
			"+func renewLease() {}\n",
	},
	{
		// Denied by tracker.DiffPathDenied on its ".env" prefix. The patch is
		// posted and the write-side filter drops it, which is exactly what the
		// runner's own post would have gone through.
		Path: ".env.local", Status: tracker.DiffStatusModified, Additions: 1, Deletions: 1,
		Patch: "diff --git a/.env.local b/.env.local\n--- a/.env.local\n+++ b/.env.local\n@@ -1 +1 @@\n-TOKEN=old\n+TOKEN=new\n",
	},
}

// seedAttemptDiff gives the linked issue one stored attempt diff to draw
// (decisions section 18.5), so the preview's Diff surface shows a real diff
// rather than only the round its change request published.
//
// The diff itself goes through storeAttemptDiff, the same function the runner's
// POST .../attempts/:attempt/diff lands in, so the stored rows are exactly what
// a producer would have written: the per-file patch cap, the files denylist and
// the generation ordering all run. Only the fencing around it is skipped, and
// deliberately. resolveAttemptDiffProducer requires a lease that is still
// current, pinned to the project's approved policy, owned by the authenticated
// runner, and a native_attempts row in status 'running' bound to that lease --
// that is, a run in flight. The preview has no such run, and holding a lease
// open for the life of the fixture would put a permanently running attempt on
// the issue page, which is a different fact from the one this seed is for. The
// attempt and its lease are therefore written to the store the way seedUsage
// writes its own, and only then is the diff stored through the producer path.
//
// It is called by the preview and by the test that asserts the fixture serves
// it, not by the fixture itself: the other hosted browser assertions are
// written against an issue that has no diff.
func (f *browserHostedFixture) seedAttemptDiff(t *testing.T) {
	t.Helper()
	now := f.service.config.now().UTC()
	// The allocated fixture already enrolled the preview runner for seedUsage,
	// and enrollment is not idempotent, so this reads the machine it made
	// rather than making a second one.
	var machine string
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT id FROM machines WHERE organization_id = ? ORDER BY id LIMIT 1", "org_browser_preview").Scan(&machine); err != nil {
		t.Fatalf("the browser fixture has no runner to attribute a diff to: %v", err)
	}
	var issueRow int64
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT id FROM issues WHERE organization_id = ? AND project_id = ? AND native_id = ?",
		"org_browser_preview", f.project, f.workItem).Scan(&issueRow); err != nil {
		t.Fatalf("the browser fixture has no issue to seed a diff against: %v", err)
	}
	attemptID := newNativeID("attempt")
	leaseID := "lease_browser_diff"
	started := now.Add(-browserUsageBusy)
	lease, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases
 (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		leaseID, issueRow, machine, "session_"+leaseID, formatHubTime(now), formatHubTime(started),
		formatHubTime(now), formatHubTime(now), formatHubTime(started), formatHubTime(now))
	if err != nil {
		t.Fatalf("seed diff lease: %v", err)
	}
	fencing, err := lease.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tracker.NativeRunData{
		RunID: "run_" + attemptID, AttemptID: attemptID, FencingToken: tracker.FencingToken(fencing),
		LeaseID: tracker.LeaseID(leaseID), MachineID: tracker.MachineID(machine), Outcome: "succeeded",
		Identity: &tracker.NativeExecutionIdentity{Role: "implementer", Backend: "codex", Model: "gpt-6-astra"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts
 (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, started_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'succeeded', ?, ?, ?)`,
		attemptID, "org_browser_preview", f.project, f.workItem, leaseID, fencing, "run_"+attemptID,
		string(data), formatHubTime(started), formatHubTime(now)); err != nil {
		t.Fatalf("seed diff attempt: %v", err)
	}
	scope := nativeScope{
		organization: "org_browser_preview", project: tracker.ProjectID(f.project),
		credential: apiCredential{Scope: apiScopeWorker, Runner: runnerauth.Identity{Binding: runnerauth.Binding{RunnerID: "runner_browser_preview"}}},
	}
	request := tracker.AttemptDiffRequest{
		Producer: tracker.DiffProducer{
			Kind: tracker.DiffSourceAttempt, ID: attemptID, RunnerID: "runner_browser_preview",
			LeaseID: tracker.LeaseID(leaseID), FencingToken: tracker.FencingToken(fencing),
		},
		Generation: tracker.DiffGeneration{Source: tracker.DiffSourceAttempt, Seq: 2},
		BaseSHA:    strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40),
		Files: browserAttemptDiffFiles,
	}
	if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, stamp time.Time) error {
		_, storeErr := storeAttemptDiff(t.Context(), tx, scope, attemptID, f.workItem, request, stamp)
		return storeErr
	}); err != nil {
		t.Fatalf("seed attempt diff: %v", err)
	}
	f.attemptDiff = attemptID
}

// browserUsageDays is how many days of usage the fixture seeds, and
// browserUsageEntries what one of those days spent. Both are named so an
// assertion can compute the report it expects rather than restate it.
const browserUsageDays = 3

var browserUsageEntries = []struct {
	Provider, Model       string
	Input, Cached, Output int64
}{
	{Provider: "codex", Model: "gpt-6-astra", Input: 820_000, Cached: 610_000, Output: 41_000},
	{Provider: "claude", Model: "claude-opus-5", Input: 310_000, Cached: 180_000, Output: 22_500},
}

// seedUsage fills attempt_usage so the usage page has a report to draw:
// three days across two providers and two models, priced by the hub's own
// table (decisions section 17.5). The rows are written straight to the store
// rather than through a run event, because the report only ever reads what a
// runner already reported. One runner is enrolled for real and every seeded
// attempt holds a released lease on its machine, so the Runners tab has the
// host, its sessions and its busy time to draw.
func (f *browserHostedFixture) seedUsage(t *testing.T) {
	t.Helper()
	now := f.service.config.now().UTC()
	prices := f.service.usagePrices()
	machine := f.enrollUsageRunner(t)
	var issueRow int64
	var nativeID string
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT id, native_id FROM issues WHERE organization_id = ? AND project_id = ? ORDER BY id LIMIT 1",
		"org_browser_preview", f.project).Scan(&issueRow, &nativeID); err != nil {
		t.Fatalf("the browser fixture has no issue to seed usage against: %v", err)
	}
	for day := range browserUsageDays {
		stamp := now.AddDate(0, 0, -day)
		for index, entry := range browserUsageEntries {
			cost, _, found := prices.estimate(entry.Model, entry.Input, entry.Cached, entry.Output)
			if !found {
				t.Fatalf("the browser fixture price table does not price %s", entry.Model)
			}
			attemptID := fmt.Sprintf("attempt_browser_usage_%d_%d", day, index)
			leaseID := fmt.Sprintf("lease_browser_usage_%d_%d", day, index)
			started := stamp.Add(-browserUsageBusy)
			lease, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO leases
 (lease_id, issue_id, machine_id, session_id, expires_at, acquired_at, renewed_at, released_at, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				leaseID, issueRow, machine, "session_"+leaseID, formatHubTime(stamp), formatHubTime(started),
				formatHubTime(stamp), formatHubTime(stamp), formatHubTime(started), formatHubTime(stamp))
			if err != nil {
				t.Fatalf("seed usage lease: %v", err)
			}
			fencing, err := lease.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(tracker.NativeRunData{RunID: "run_" + attemptID, AttemptID: attemptID, FencingToken: tracker.FencingToken(fencing), LeaseID: tracker.LeaseID(leaseID), MachineID: tracker.MachineID(machine), Outcome: "succeeded"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO native_attempts
 (id, organization_id, project_id, work_item_id, lease_id, fencing_token, run_id, sequence, status, data_json, started_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'succeeded', ?, ?, ?)`,
				attemptID, "org_browser_preview", f.project, nativeID, leaseID, fencing, "run_"+attemptID,
				string(data), formatHubTime(started), formatHubTime(stamp)); err != nil {
				t.Fatalf("seed usage attempt: %v", err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), `INSERT INTO attempt_usage
 (attempt_id, organization_id, project_id, day, provider, model, input, cached_input, output, cost_estimate, currency, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'USD', ?)`,
				attemptID, "org_browser_preview", f.project,
				stamp.Format(usageDayLayout), entry.Provider, entry.Model,
				entry.Input, entry.Cached, entry.Output, cost, formatHubTime(stamp)); err != nil {
				t.Fatalf("seed usage: %v", err)
			}
		}
	}
}

// browserUsageBusy is how long each seeded attempt held its runner.
const browserUsageBusy = 25 * time.Minute

// enrollUsageRunner registers one runner in the fixture's organization and
// returns its machine id, which is what a lease names. The rows are written
// directly: enrollment is an instance-admin operation the hosted boundary
// does not expose, and no runner process joins this fixture.
func (f *browserHostedFixture) enrollUsageRunner(t *testing.T) string {
	t.Helper()
	now := formatHubTime(f.service.config.now().UTC())
	later := formatHubTime(f.service.config.now().UTC().Add(time.Hour))
	const (
		machine    = "machine_browser_preview_runner"
		token      = "tok_browser_preview_runner"
		enrollment = "enr_browser_preview_runner"
		identity   = "runner_browser_preview"
	)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO api_tokens (id, name, token_hash, token_fingerprint, scope, created_at, updated_at, expires_at, native_only) VALUES (?, ?, ?, ?, 'worker', ?, ?, ?, 1)`,
			[]any{token, "Preview runner", strings.Repeat("a", 64), "preview", now, now, later}},
		{`INSERT INTO machines (id, hostname, display_name, capacity, version, last_heartbeat_at, registered_at, updated_at, organization_id, token_id) VALUES (?, ?, ?, 2, 'preview', ?, ?, ?, ?, ?)`,
			[]any{machine, "preview-host", "Preview runner", now, now, now, "org_browser_preview", token}},
		{`INSERT INTO runner_enrollments (id, organization_id, runner_id, machine_id, token_hash, operations_json, created_at, expires_at, created_by, redeemed_at) VALUES (?, ?, ?, ?, ?, '["claim"]', ?, ?, ?, ?)`,
			[]any{enrollment, "org_browser_preview", identity, machine, strings.Repeat("b", 64), now, later, token, now}},
		{`INSERT INTO runner_identities (id, organization_id, machine_id, token_id, enrollment_id, operations_json, created_at, display_name, capacity_limit, reported_capacity, os, architecture, last_heartbeat_at) VALUES (?, ?, ?, ?, ?, '["claim"]', ?, ?, 2, 2, 'linux', 'arm64', ?)`,
			[]any{identity, "org_browser_preview", machine, token, enrollment, now, "Preview runner", now}},
	} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement.query, statement.args...); err != nil {
			t.Fatalf("seed the preview runner: %v", err)
		}
	}
	// The composer's model picker lists what the organization's runners
	// report for the projects the actor can read, so the preview runner is
	// granted the project and reports a catalogue with per-model reasoning
	// ladders (decisions section 14). Reports go stale after
	// providercapacity.MaxAge, so the long-running preview refreshes it.
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO token_grants(token_id,organization_id,project_id) VALUES (?, 'org_browser_preview', ?)", token, f.project); err != nil {
		t.Fatalf("grant the preview runner: %v", err)
	}
	f.refreshPreviewRunnerReport(t, identity)
	return machine
}

// browserPreviewModelCatalogue is the catalogue the preview runner reports:
// a current model per rung of the ladder, a second current model, and one
// the backend has named a successor for, so every branch of the picker has
// a row to draw.
func browserPreviewModelCatalogue() []providercapacity.ModelDetail {
	ladder := []string{"low", "medium", "high", "xhigh", "max"}
	return []providercapacity.ModelDetail{
		{ID: "gpt-6-astra", Label: "GPT-6-Astra", Provider: "openai", Default: true, ReasoningEfforts: ladder, DefaultReasoningEffort: "medium"},
		{ID: "gpt-5.6-sol", Label: "GPT-5.6-Sol", Provider: "openai", ReasoningEfforts: ladder[:4], DefaultReasoningEffort: "medium"},
		{ID: "gpt-5.3-codex", Label: "GPT-5.3-Codex", Provider: "openai", ReasoningEfforts: ladder[:3], DefaultReasoningEffort: "low", Legacy: true},
	}
}

// refreshPreviewRunnerReport writes the preview runner's provider report as
// observed now, which is what keeps it inside providercapacity.MaxAge.
func (f *browserHostedFixture) refreshPreviewRunnerReport(t *testing.T, identity string) {
	t.Helper()
	now := f.service.config.now().UTC()
	catalogue := browserPreviewModelCatalogue()
	models := make([]string, 0, len(catalogue))
	for _, model := range catalogue {
		models = append(models, model.ID)
	}
	reports := []providercapacity.Report{{
		Provider: "openai", Backend: "codex", AccountAlias: "preview",
		Models: models, ModelDetails: catalogue, MaxConcurrent: 2, Availability: "available", ObservedAt: now,
	}}
	raw, err := json.Marshal(reports)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE runner_identities SET provider_reports_json = ?, last_heartbeat_at = ? WHERE id = ?", string(raw), formatHubTime(now), identity); err != nil {
		t.Fatalf("report the preview runner's catalogue: %v", err)
	}
}

// previewRunnerRefreshInterval is how often the preview re-reports on its
// runners' behalf.
//
// Two different clocks age out a seeded runner and the preview has to stay
// inside both: providercapacity.MaxAge drops the model picker's rows, and
// runnerauth.HeartbeatTimeout drops the workspace claim gate's idea that the
// runner is still there. Halving the shorter of the two keeps a whole beat in
// hand against either.
func previewRunnerRefreshInterval() time.Duration {
	shortest := providercapacity.MaxAge
	if runnerauth.HeartbeatTimeout < shortest {
		shortest = runnerauth.HeartbeatTimeout
	}
	return shortest / 2
}

// heartbeatWorkspaceRunner sends one machine heartbeat for the enrolled
// workspace runner, carrying what it can serve.
//
// It goes through the real endpoint rather than writing the row, because the
// heartbeat is the only thing that stores workspace_capabilities_json, and the
// claim gate reads that column beside last_heartbeat_at. Writing the row by
// hand would keep the runner looking fresh while reporting a capability set
// the real beat never produced.
func (f *browserHostedFixture) heartbeatWorkspaceRunner(t *testing.T) {
	t.Helper()
	if f.workspaceRunnerCredential == "" {
		t.Fatal("the workspace runner has not been seeded")
	}
	base := browserHostedOrganizationBase + "/projects/" + f.project
	f.token(t, f.workspaceRunnerCredential, http.MethodPost, base+"/machines/"+string(f.workspaceRunnerMachine)+"/heartbeat", map[string]any{
		"display_name": "Preview runner", "capacity": 4, "version": "preview",
		"workspace_capabilities": workspacerunner.Capabilities(workspacerunner.DefaultSupport()),
		"workspace_isolation":    workspacesession.IsolationUser,
	}, http.StatusNoContent)
}

// refreshPreviewRunners re-reports for every runner the preview seeded.
//
// The preview outlives both staleness windows, and each runner is stale for a
// reason of its own: the catalogue runner stops offering models, and the
// workspace runner stops being offered workspace items. Refreshing them
// together is what keeps a Files panel opened hours into a preview reaching a
// runner at all, which is exactly what a single seeded beat did not do.
func (f *browserHostedFixture) refreshPreviewRunners(t *testing.T) {
	t.Helper()
	f.refreshPreviewRunnerReport(t, "runner_browser_preview")
	if f.workspaceRunnerCredential != "" {
		f.heartbeatWorkspaceRunner(t)
	}
}

// api performs one JSON API call as account, asserting the status.
func (f *browserHostedFixture) api(t *testing.T, account, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	cookie := f.cookies[account]
	if cookie == nil {
		t.Fatal("account session cookie is missing")
	}
	request := httptest.NewRequest(method, f.server.URL+path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", f.server.URL)
	request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	browserHostedStatus(t, response, status)
	return response
}

// token is api for a bearer credential. The runner half of the preview
// authenticates the way a real runner does -- an enrolled identity's token,
// not a hosted session -- so it cannot go through the cookie helper.
func (f *browserHostedFixture) token(t *testing.T, credential, method, path string, body any, status int) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequest(method, f.server.URL+path, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+credential)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	browserHostedStatus(t, response, status)
	return response
}

func browserHostedDecode(t *testing.T, response *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), target); err != nil {
		t.Fatalf("decode %s: %v", response.Body.String(), err)
	}
}

// seedConversation pre-creates the conversation the browser fixture opens: a
// chat carrying one answered coordinator turn, then linked to a fresh issue
// so the linked-issue surfaces (breadcrumb chip, execution strip and the
// issue result card in the transcript) have real durable data behind them.
func (f *browserHostedFixture) seedConversation(t *testing.T) {
	t.Helper()
	base := "/api/v2/organizations/org_browser_preview/projects/" + f.project
	var created struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/conversations", map[string]any{
		"key":           "browser-preview-conversation",
		"title":         "Lease renewal under load",
		"first_message": map[string]any{"key": "browser-preview-message", "text": "Why does the lease lapse under load?"},
	}, http.StatusCreated), &created)
	f.conversation = created.Conversation.ID
	if f.conversation == "" {
		t.Fatal("created conversation has no id")
	}
	// With the coordinator handed to an enrolled runner there is no scripted
	// reply to wait for: the turn stays open until a runner claims it, which
	// may be minutes after the hub is up. The link below needs the
	// conversation, not the reply.
	if !browserPreviewRunnerCoordinator() {
		f.waitForAssistantReply(t, base, f.conversation)
	}
	var linked struct {
		Issue struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/conversations/"+f.conversation+"/link", map[string]any{
		"key":           "browser-preview-link",
		"share_history": true,
		"issue": map[string]any{
			"title":       "Renew the lease before the handoff completes",
			"description": "Move the lease renewal behind the handoff acknowledgement.",
		},
	}, http.StatusOK), &linked)
	f.workItem = linked.Issue.ID
	if f.workItem == "" {
		t.Fatal("linked issue has no work item id")
	}
}

// waitForAssistantReply polls the snapshot until the scripted coordinator has
// finished its turn, so the seeded transcript is stable before a browser or a
// later assertion reads it.
func (f *browserHostedFixture) waitForAssistantReply(t *testing.T, base, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		var snapshot struct {
			Messages []struct {
				Role     string `json:"role"`
				Text     string `json:"text"`
				Delivery string `json:"delivery"`
			} `json:"messages"`
		}
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, base+"/conversations/"+id, nil, http.StatusOK), &snapshot)
		for _, message := range snapshot.Messages {
			if message.Role == "assistant" && message.Delivery == "completed" && strings.Contains(message.Text, "closes the window") {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("coordinator reply did not complete for conversation %s", id)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (f *browserHostedFixture) page(t *testing.T, account, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, f.server.URL+path, nil)
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

func (f *browserHostedFixture) createProject(t *testing.T, name string) string {
	t.Helper()
	var project tracker.NativeProject
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/projects", map[string]any{
		"idempotency_key": "project-" + name, "name": name, "grant_access": true,
	}, http.StatusCreated), &project)
	if !strings.HasPrefix(string(project.ID), "prj_") {
		t.Fatalf("project creation returned %#v", project)
	}
	return string(project.ID)
}

// grant changes one member's project access through the section 12 endpoint.
func (f *browserHostedFixture) grant(t *testing.T, account, user, project string, write, runner, revoke bool) {
	t.Helper()
	f.api(t, account, http.MethodPut, browserHostedOrganizationBase+"/members/membership_"+user+"/grants", map[string]any{
		"idempotency_key": "grant-" + user + "-" + project, "project_id": project, "write": write, "runner": runner, "revoke": revoke,
	}, http.StatusOK)
}

func browserHostedStatus(t *testing.T, response *httptest.ResponseRecorder, status int) {
	t.Helper()
	if response.Code != status {
		t.Fatalf("status = %d, want %d: %s", response.Code, status, response.Body.String())
	}
}

// TestHostedBrowserShellAndAccess replaces the removed HTML page assertions:
// the shell answers every client route for a session, and the JSON endpoints
// behind it enforce the same access the Templ pages did (decisions section 12).
func TestHostedBrowserShellAndAccess(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	for _, test := range []struct {
		name, account, path string
		status              int
	}{
		{name: "login", path: "/login", status: http.StatusOK},
		{name: "anonymous work", path: "/work", status: http.StatusSeeOther},
		{name: "owner organization", account: "owner", path: "/organization", status: http.StatusOK},
		{name: "viewer work", account: "viewer", path: "/work", status: http.StatusOK},
		{name: "support viewer", account: "support-viewer", path: "/projects/" + f.project, status: http.StatusOK},
		{name: "revoked session", account: "revoked", path: "/work", status: http.StatusSeeOther},
		{name: "expired session", account: "expired", path: "/work", status: http.StatusSeeOther},
		{name: "staff support", account: "staff", path: "/support", status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := f.page(t, test.account, test.path)
			browserHostedStatus(t, response, test.status)
			if test.status != http.StatusOK {
				if response.Header().Get("Location") != "/login" {
					t.Fatalf("location = %q", response.Header().Get("Location"))
				}
				return
			}
			if !strings.Contains(response.Body.String(), `id="root"`) {
				t.Fatalf("shell body = %s", response.Body.String())
			}
			for _, forbidden := range []string{"sse-connect", "hx-get", "/api/v1/", "Members and invitations"} {
				if strings.Contains(response.Body.String(), forbidden) {
					t.Errorf("shell includes %q", forbidden)
				}
			}
		})
	}
	t.Run("project data follows the grant", func(t *testing.T) {
		var page struct {
			Items []tracker.NativeIssue `json:"items"`
		}
		browserHostedDecode(t, f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/projects/"+f.project+"/work-items", nil, http.StatusOK), &page)
		if len(page.Items) == 0 || page.Items[0].Title != "Review the invitation flow" {
			t.Fatalf("viewer work items = %#v", page.Items)
		}
		f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/projects/"+f.privateProject+"/work-items", nil, http.StatusNotFound)
		f.api(t, "staff", http.MethodGet, browserHostedOrganizationBase+"/projects/"+f.project+"/work-items", nil, http.StatusForbidden)
		f.api(t, "wrong-organization", http.MethodGet, browserHostedOrganizationBase+"/projects/"+f.project+"/work-items", nil, http.StatusForbidden)
		f.api(t, "revoked", http.MethodGet, browserHostedOrganizationBase+"/projects/"+f.project+"/work-items", nil, http.StatusUnauthorized)
	})
	t.Run("members list follows the role", func(t *testing.T) {
		var owner hostedMembersResponse
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &owner)
		if len(owner.Members) < 2 {
			t.Fatalf("owner members = %#v", owner.Members)
		}
		var viewer hostedMembersResponse
		browserHostedDecode(t, f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/members", nil, http.StatusOK), &viewer)
		if len(viewer.Members) != 1 || viewer.Members[0].UserID != "user_browser_viewer" || len(viewer.Invitations) != 0 {
			t.Fatalf("viewer members = %#v", viewer)
		}
	})
}

// TestHostedBrowserJSONMutations replaces the removed form tests with the
// section 12 JSON endpoints they became.
func TestHostedBrowserJSONMutations(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	for _, test := range []struct {
		name, account, method, path string
		payload                     any
		status                      int
	}{
		{name: "project requires explicit access", account: "owner", method: http.MethodPost, path: "/projects", payload: map[string]any{"idempotency_key": "unapproved", "name": "Unapproved project"}, status: http.StatusUnprocessableEntity},
		{name: "project requires an idempotency key", account: "owner", method: http.MethodPost, path: "/projects", payload: map[string]any{"name": "Keyless project", "grant_access": true}, status: http.StatusUnprocessableEntity},
		{name: "viewer cannot create", account: "viewer", method: http.MethodPost, path: "/projects", payload: map[string]any{"idempotency_key": "viewer", "name": "Viewer project", "grant_access": true}, status: http.StatusNotFound},
		{name: "owner creates project", account: "owner", method: http.MethodPost, path: "/projects", payload: map[string]any{"idempotency_key": "form", "name": "Form project", "grant_access": true}, status: http.StatusCreated},
		{name: "owner invites member", account: "owner", method: http.MethodPost, path: "/members/invitations", payload: map[string]any{"idempotency_key": "invite", "email": "invitee@example.test", "role": "viewer"}, status: http.StatusCreated},
		{name: "staff cannot invite", account: "staff", method: http.MethodPost, path: "/members/invitations", payload: map[string]any{"idempotency_key": "staff-invite", "email": "other@example.test", "role": "member"}, status: http.StatusForbidden},
		{name: "viewer cannot invite", account: "viewer", method: http.MethodPost, path: "/members/invitations", payload: map[string]any{"idempotency_key": "viewer-invite", "email": "other@example.test", "role": "member"}, status: http.StatusForbidden},
		{name: "unknown organization cannot be switched to", account: "viewer", method: http.MethodPost, path: "/switch", payload: map[string]any{"organization": "org_unknown"}, status: http.StatusForbidden},
		{name: "authorized support starts", account: "support-staff", method: http.MethodPost, path: "/support/start", payload: map[string]any{}, status: http.StatusOK},
		{name: "ordinary staff cannot impersonate", account: "staff", method: http.MethodPost, path: "/support/start", payload: map[string]any{}, status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.api(t, test.account, test.method, browserHostedOrganizationBase+test.path, test.payload, test.status)
		})
	}
	var invitation string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT id FROM hosted_invitations WHERE email = 'invitee@example.test'").Scan(&invitation); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, account string
		status        int
	}{
		{name: "wrong invited account", account: "viewer", status: http.StatusForbidden},
		{name: "invitation accepted", account: "invitee", status: http.StatusOK},
		{name: "invitation replay", account: "invitee", status: http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.api(t, test.account, http.MethodPost, browserHostedOrganizationBase+"/invitations/accept", map[string]any{"token": invitation}, test.status)
		})
	}
}

// TestHostedBrowserFleetAndPlan covers the fleet, plan and billing reads the
// account screens open with.
func TestHostedBrowserFleetAndPlan(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	var fleet hostedFleetResponse
	browserHostedDecode(t, f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/fleet", nil, http.StatusOK), &fleet)
	if fleet.Runners == nil || fleet.Spend != nil || fleet.Usage.Allowances == nil {
		t.Fatalf("fleet = %#v", fleet)
	}
	var projects []hostedProjectView
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, browserHostedOrganizationBase+"/projects", nil, http.StatusOK), &projects)
	if len(projects) != 2 {
		t.Fatalf("projects = %#v", projects)
	}
	for _, project := range projects {
		if len(project.States) != 3 || project.Onboarding.Ready || len(project.Onboarding.Steps) != 4 {
			t.Fatalf("project = %#v", project)
		}
	}
	// This fixture configures no hosted plans, so the plan report is
	// unavailable rather than forbidden for an owner.
	f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/plan", nil, http.StatusForbidden)
	f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/billing", nil, http.StatusForbidden)
}

func TestHostedBrowserPullRequestPanel(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	f.seedPullRequest(t)
	path := browserHostedOrganizationBase + "/projects/" + f.project + "/work-items/" + f.workItem + "/pull-requests"
	var views []pullRequestView
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, path, nil, http.StatusOK), &views)
	if len(views) != 1 {
		t.Fatalf("views = %d, want 1", len(views))
	}
	view := views[0]
	if view.Number != f.pullRequest || view.Connector == nil || view.Connector.Repository != "digitaldrywood/detent" {
		t.Fatalf("view = %#v", view)
	}
	if view.State != "open" || !view.Mergeable.Known || !view.Mergeable.Value || view.ReviewDecision != "approved" {
		t.Fatalf("view = %#v", view)
	}
	if len(view.Reviews) != 1 || view.Head.Ref != "detent/digitaldrywood_detent_4180" {
		t.Fatalf("view = %#v", view)
	}
	// A viewer reads the panel; only a writer may ask for an action.
	f.api(t, "viewer", http.MethodGet, path, nil, http.StatusOK)
	f.api(t, "viewer", http.MethodPost, path+"/actions",
		map[string]any{"idempotency_key": "viewer-open", "action": "open", "expected_head_sha": view.Head.SHA}, http.StatusNotFound)
}

// The preview serves one stored attempt diff for the linked issue, so the Diff
// surface has a real diff to draw rather than only the round its change request
// published (decisions 18.5). The seed goes through the producer's own store
// path, so this also asserts that the write-side rules ran: the denylisted file
// is listed with its counts and without its patch.
func TestHostedBrowserAttemptDiff(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	f.seedAttemptDiff(t)

	// The client finds the attempt on the work item and then reads its diff,
	// so both halves of that walk are exercised here.
	var attempts tracker.Page[tracker.NativeAttempt]
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet,
		browserHostedOrganizationBase+"/projects/"+f.project+"/work-items/"+f.workItem+"/attempts", nil, http.StatusOK), &attempts)
	latest := ""
	for _, attempt := range attempts.Items {
		if attempt.AttemptID == f.attemptDiff {
			latest = attempt.AttemptID
		}
	}
	if latest == "" {
		t.Fatalf("the seeded attempt %s is not on the work item: %#v", f.attemptDiff, attempts.Items)
	}

	path := browserHostedOrganizationBase + "/projects/" + f.project + "/attempts/" + latest + "/diff"
	var diff tracker.AttemptDiff
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, path, nil, http.StatusOK), &diff)
	if diff.AttemptID != latest || diff.Generation.Source != tracker.DiffSourceAttempt || diff.Generation.Seq != 2 {
		t.Fatalf("diff = %#v", diff)
	}
	if diff.HeadSHA != strings.Repeat("b", 40) || diff.BaseSHA != strings.Repeat("a", 40) {
		t.Fatalf("diff shas = %q/%q", diff.BaseSHA, diff.HeadSHA)
	}
	if len(diff.Files) != len(browserAttemptDiffFiles) || diff.FileCount != len(browserAttemptDiffFiles) {
		t.Fatalf("files = %#v", diff.Files)
	}
	byPath := map[string]tracker.AttemptDiffFile{}
	for _, file := range diff.Files {
		byPath[file.Path] = file
	}
	main, ok := byPath["main.go"]
	if !ok || main.Status != tracker.DiffStatusModified || !strings.Contains(main.Patch, "+\trenewLease()") {
		t.Fatalf("main.go = %#v", main)
	}
	added, ok := byPath["internal/hubserver/renewal.go"]
	if !ok || added.Status != tracker.DiffStatusAdded || !strings.Contains(added.Patch, "new file mode") {
		t.Fatalf("renewal.go = %#v", added)
	}
	// Section 18.5's denylist: the file stays in the list with its counts and
	// loses its contents, so a later reader is not shown what the live reader
	// was not.
	denied, ok := byPath[".env.local"]
	if !ok || !denied.Denied || denied.Patch != "" || denied.Additions != 1 || denied.Deletions != 1 {
		t.Fatalf(".env.local = %#v", denied)
	}

	// The read follows the issue's rule: a viewer granted the project sees it,
	// and an attempt that posted nothing is a 404 rather than an empty diff.
	f.api(t, "viewer", http.MethodGet, path, nil, http.StatusOK)
	f.api(t, "owner", http.MethodGet,
		browserHostedOrganizationBase+"/projects/"+f.project+"/attempts/"+newNativeID("attempt")+"/diff", nil, http.StatusNotFound)

	// And the issue-addressed read the client actually uses: the same diff in
	// one request, without having to walk the attempt list.
	itemPath := browserHostedOrganizationBase + "/projects/" + f.project + "/work-items/" + f.workItem + "/diff"
	var onItem tracker.WorkItemDiff
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, itemPath, nil, http.StatusOK), &onItem)
	if onItem.Diff == nil || onItem.Diff.ID != diff.ID || len(onItem.Diff.Files) != len(browserAttemptDiffFiles) {
		t.Fatalf("work item diff = %#v", onItem)
	}
	f.api(t, "viewer", http.MethodGet, itemPath, nil, http.StatusOK)
}

// An issue no attempt has posted a diff for answers the issue-addressed read
// with an explicit absence rather than a 404. That is the whole reason the
// route exists: a client that walked the attempt list instead would take a 404
// per attempt, and a browser console error with it, to learn one fact.
func TestHostedBrowserWorkItemDiffAbsent(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	path := browserHostedOrganizationBase + "/projects/" + f.project + "/work-items/" + f.workItem + "/diff"
	var answer tracker.WorkItemDiff
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, path, nil, http.StatusOK), &answer)
	if answer.Diff != nil {
		t.Fatalf("an issue with no attempt diff answered %#v", answer)
	}
	// An issue the reader cannot see is still a 404, so the absence above is
	// never mistaken for a permission answer.
	f.api(t, "viewer", http.MethodGet,
		browserHostedOrganizationBase+"/projects/"+f.privateProject+"/work-items/"+f.workItem+"/diff", nil, http.StatusNotFound)
}

func TestHostedBrowserFirstOrganization(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, false)
	f.api(t, "owner", http.MethodGet, "/app/bootstrap", nil, http.StatusForbidden)
	var created struct {
		Organization appBootstrapOrganization `json:"organization"`
		Next         string                   `json:"next"`
	}
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, "/api/v2/organizations", map[string]any{"idempotency_key": "first", "name": "New browser organization"}, http.StatusCreated), &created)
	var organization, role string
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT provider_id FROM hosted_tenant").Scan(&organization); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT role FROM hosted_members WHERE user_id = 'user_browser_owner'").Scan(&role); err != nil {
		t.Fatal(err)
	}
	if organization != "org_browser_provider" || role != "owner" || created.Next != "/auth/oidc/start" {
		t.Fatalf("organization creation = organization %q, role %q, next %q", organization, role, created.Next)
	}
	f.api(t, "owner", http.MethodPost, "/api/v2/organizations", map[string]any{"idempotency_key": "second", "name": "Second organization"}, http.StatusConflict)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	base, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(base, []*http.Cookie{f.cookies["owner"]})
	client := f.server.Client()
	client.Jar = jar
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.server.URL+"/auth/oidc/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(result.Body)
	closeErr := result.Body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read sign-in response: %v; close: %v", readErr, closeErr)
	}
	if result.StatusCode != http.StatusOK || result.Request.URL.Path != "/work" || !strings.Contains(string(body), `id="root"`) {
		t.Fatalf("first sign-in after organization creation failed: status %d, path %s, body %s", result.StatusCode, result.Request.URL.Path, body)
	}
	for _, cookie := range jar.Cookies(base) {
		if cookie.Name == hostedCookie {
			f.cookies["owner"] = cookie
		}
	}
	project := f.createProject(t, "First browser project")
	var bootstrap appBootstrap
	browserHostedDecode(t, f.api(t, "owner", http.MethodGet, "/app/bootstrap", nil, http.StatusOK), &bootstrap)
	if bootstrap.Organization.Name != "New browser organization" || len(bootstrap.Projects) != 1 || bootstrap.Projects[0].ID != project {
		t.Fatalf("bootstrap after setup = %#v", bootstrap)
	}
}

// seedWorkspaceRunner gives the preview a runner that can actually serve the
// Files surface (decisions sections 18.1, 18.2 and 18.4).
//
// It is the real runner half, not a stub: an enrolled runner reporting the
// files capability, the real claim path, and workspacerunner.Lane binding,
// heartbeating and answering files frames over the real relay. A stub would
// have proved that the client can draw a tree, and the thing worth proving is
// that a person clicking Files reaches a worktree through every layer between
// them.
//
// The worktree it serves is a small tree of its own rather than this
// repository: a preview that listed the developer's own checkout would be
// showing whatever happened to be on the machine.
func (f *browserHostedFixture) seedWorkspaceRunner(t *testing.T) workspacesession.Session {
	t.Helper()
	policy := hubTestPolicy()
	base := browserHostedOrganizationBase + "/projects/" + f.project
	f.api(t, "owner", http.MethodPut, base+"/policy", policypkg.Change{Policy: policy}, http.StatusOK)

	// Enrolling a runner needs the project's runner grant, which the owner
	// does not hold by default: the bootstrap member is an owner of the
	// organization, and a runner grant is per project on purpose.
	f.grant(t, "owner", "user_browser_owner", f.project, true, true, false)

	binding := runnerauth.NewBinding()
	credential, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	var enrollment runnerauth.Enrollment
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments",
		runnerauth.EnrollmentRequest{
			Binding: binding, ProjectIDs: []tracker.ProjectID{tracker.ProjectID(f.project)},
			Operations: []string{runnerauth.Read, runnerauth.Collaborate, runnerauth.Claim, runnerauth.Heartbeat, runnerauth.Events},
			TTLSeconds: 900,
		}, http.StatusCreated), &enrollment)
	redemption := runnerauth.Redemption{
		Binding: binding, Credential: credential, Hostname: "preview-runner",
		DisplayName: "Preview runner", Capacity: 4, Version: "preview",
	}
	f.token(t, enrollment.Token, http.MethodPost, browserHostedOrganizationBase+"/runner-enrollments/redeem", redemption, http.StatusCreated)
	// The capability report is what makes this runner eligible: the claim gate
	// asks for every capability in requires, reported with a fresh heartbeat.
	// It is a beat rather than a seeded row because the preview has to go on
	// sending it; see heartbeatWorkspaceRunner.
	f.workspaceRunnerCredential = credential
	f.workspaceRunnerMachine = binding.MachineID
	f.heartbeatWorkspaceRunner(t)

	worktree := t.TempDir()
	for name, content := range map[string]string{
		"README.md":       "# Preview worktree\n\nThe Files surface reads this tree over the relay.\n",
		"go.mod":          "module example.test/preview\n\ngo 1.26\n",
		".env":            "TOKEN=never-served\n",
		"main.go":         "package main\n\nfunc main() {\n\tprintln(\"preview\")\n}\n",
		"internal/app.go": "package internal\n\n// App is what the preview's file view renders.\ntype App struct{ Name string }\n",
	} {
		full := filepath.Join(worktree, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seedPreviewGitRepository(t, worktree)

	client, err := hubclient.New(hubclient.Config{URL: f.server.URL, TokenSource: func() string { return credential }})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(tracker.OrganizationID(browserHostedOrganization), tracker.ProjectID(f.project))
	if err != nil {
		t.Fatal(err)
	}
	sessions := 0
	claimer, err := hubclient.NewWorkspaceClaimer(native, hubclient.WorkspaceLaneConfig{
		PolicyID: policy.ID, MachineID: binding.MachineID,
		SessionID: func() (string, error) {
			sessions++
			return fmt.Sprintf("preview-workspace-%d", sessions), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	lane, err := workspacerunner.NewLane(workspacerunner.LaneConfig{
		Claimer: claimer, Hub: native, Worktree: fixedPreviewWorktree{path: worktree},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Poll: time.Second,
		// The same hostname the enrollment redeemed under. The workspace
		// resource reports it beside the worktree path, and the header's Open
		// picker disables every editor link naming a machine that is not the
		// reader's own -- so a preview that reported no hostname would show
		// the picker enabled for a worktree it cannot reach.
		Hostname: "preview-runner",
		// The same Support the heartbeat reports, so the capability the claim
		// gate matches and the one this lane's sessions bind with are one
		// answer (section 18.3).
		Support: workspacerunner.DefaultSupport(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = lane.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Log("the preview workspace lane did not stop in time")
		}
	})
	f.prewarmed = f.openPreviewWorkspace(t)
	return f.prewarmed
}

// openPreviewWorkspace requests a workspace on the fixture's issue and waits
// for the lane to take it all the way to ready.
//
// It does two jobs. It proves the runner half actually works, so a broken lane
// fails the preview here rather than leaving a browser watching Files spin;
// and it pre-warms the workspace, so a Playwright run opening Files finds an
// open one to reuse instead of waiting out a claim poll.
func (f *browserHostedFixture) openPreviewWorkspace(t *testing.T) workspacesession.Session {
	t.Helper()
	base := browserHostedOrganizationBase + "/projects/" + f.project
	var opened workspacesession.Session
	// Both capabilities, because the client asks for both when running an
	// action is the reason it wants a worktree (decisions section 18.12) and
	// reuse only adopts a workspace whose capabilities satisfy the request. A
	// files-only pre-warm would be skipped by the panel and a second workspace
	// opened behind it, which is exactly the claim poll the pre-warm exists to
	// avoid.
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/workspaces", map[string]any{
		"idempotency_key": "browser-preview-workspace",
		"work_item_id":    f.workItem,
		// exec and git as well as files: the claim gate matches requires
		// against what the runner reported, so asking for them is what makes
		// the workspace resource report the capabilities the Output surface
		// and the header's git group each enable from.
		"requires": []string{
			workspacesession.CapabilityFiles,
			workspacesession.CapabilityExec,
			workspacesession.CapabilityGit,
			// The terminal too (section 18.3), so the pre-warmed workspace the
			// preview hands the panel is one a shell can actually open on. A
			// workspace that reported the capability but did not require it
			// would be claimable by a runner that serves no terminal, and the
			// Playwright run would type into a card that had gone disabled.
			workspacesession.CapabilityTerminal,
		},
	}, http.StatusCreated), &opened)
	deadline := time.Now().Add(45 * time.Second)
	for {
		var current workspacesession.Session
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, base+"/workspaces/"+opened.ID, nil, http.StatusOK), &current)
		if current.State == workspacesession.StateReady || current.State == workspacesession.StateIdle {
			t.Logf("Preview workspace: %s ready on runner %s (%s at %s)",
				current.ID, current.RunnerID, current.MachineHostname, current.WorktreePath)
			f.workspace = current.ID
			return current
		}
		if workspacesession.Terminal(current.State) {
			t.Fatalf("preview workspace %s ended as %s (%s) before it was ready", current.ID, current.State, current.Reason)
		}
		if time.Now().After(deadline) {
			f.logRunnerReports(t)
			t.Fatalf("preview workspace %s stopped at %s (%s) requiring %v",
				current.ID, current.State, current.Reason, current.Requires)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// seedPreviewGitRepository turns the seeded tree into a real repository with a
// real remote, because the header's git action group is live in the preview
// (decisions section 18.12) and a Playwright run commits in this worktree and
// then watches the branch move.
//
// It has to be a repository and not a stub for the same reason the runner half
// is real: the runner reports the git capability only when the worktree is one,
// so a plain directory would disable the whole action group and the preview
// would be showing the disabled card instead of the surface. A bare repository
// beside it is the remote, so a push has somewhere to go that is not the
// developer's own machine.
//
// Every command carries its own -c identity and init.defaultBranch, so the
// fixture does not depend on whatever is in the machine's git config: a
// developer with no user.email set, or with init.defaultBranch=master, would
// otherwise get a different tree from CI.
//
// .env is deliberately left out of the initial commit and out of any ignore
// file. It is the Files surface's redaction case already, and it now doubles
// as the denylist case a commit has to exclude (section 18.4): gitignoring it
// would make git exclude it and the denylist would never be exercised.
func seedPreviewGitRepository(t *testing.T, worktree string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("the preview worktree needs git on PATH to be a repository: %v", err)
	}
	origin := filepath.Join(t.TempDir(), "origin.git")
	// The tracked set is named rather than added wholesale, because `git add
	// .` would commit .env and take the denylist case away.
	tracked := []string{"README.md", "go.mod", "main.go", filepath.Join("internal", "app.go")}
	for _, command := range [][]string{
		{"-c", "init.defaultBranch=main", "init", worktree},
		append([]string{"-C", worktree, "add", "--"}, tracked...),
		{"-C", worktree, "commit", "-m", "Seed the preview worktree"},
		{"-c", "init.defaultBranch=main", "init", "--bare", origin},
		{"-C", worktree, "remote", "add", "origin", origin},
		{"-C", worktree, "push", "--set-upstream", "origin", "main"},
	} {
		arguments := append([]string{
			"-c", "user.name=Preview Runner", "-c", "user.email=preview@example.test",
			"-c", "commit.gpgsign=false",
		}, command...)
		run := exec.CommandContext(t.Context(), "git", arguments...)
		run.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
		if output, err := run.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(command, " "), err, output)
		}
	}
	t.Logf("Preview worktree %s is a repository on main tracking %s", worktree, origin)
}

// The preview's runner half, run without the browser.
//
// TestHostedBrowserPreview is behind an environment variable and a Playwright
// run, so nothing in the ordinary suite proves that a workspace asking for
// files and git still finds a runner to serve it. That is the claim gate of
// section 18.1: a runner may claim a workspace only if it reports every
// capability in `requires` with a fresh heartbeat, so adding git to `requires`
// is exactly the kind of change that turns a ready workspace into a failed one
// with no_runner -- and the preview would then show the disabled card for
// every surface rather than the surfaces.
//
// It also asserts the two fields the header's Open picker compares, because a
// resource that reported no hostname would leave the picker enabled for a
// worktree the reader cannot reach.
func TestHostedBrowserPreviewWorkspaceBecomesReady(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	ready := f.seedWorkspaceRunner(t)
	if ready.Capabilities == nil || !ready.Capabilities.Files || !ready.Capabilities.Git {
		t.Fatalf("capabilities = %+v, want files and git", ready.Capabilities)
	}
	if ready.MachineHostname != "preview-runner" {
		t.Fatalf("machine hostname = %q, want the runner's own", ready.MachineHostname)
	}
	if ready.WorktreePath == "" {
		t.Fatal("the resource carries no worktree path, so no editor link can name one")
	}
}

// The preview's whole git surface rests on the seeded tree being a real
// repository, and the preview test itself is behind an environment variable
// and a browser. This runs the seeding on its own so a fixture that quietly
// stopped producing a repository fails here, in the ordinary suite, rather
// than as a disabled action group nobody is watching.
func TestPreviewWorktreeIsARepository(t *testing.T) {
	t.Parallel()
	worktree := t.TempDir()
	for _, name := range []string{"README.md", "go.mod", "main.go", filepath.Join("internal", "app.go"), ".env"} {
		full := filepath.Join(worktree, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("seed\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	seedPreviewGitRepository(t, worktree)
	for _, test := range []struct {
		name      string
		arguments []string
		want      string
	}{
		{name: "branch", arguments: []string{"rev-parse", "--abbrev-ref", "HEAD"}, want: "main"},
		{name: "upstream", arguments: []string{"rev-parse", "--abbrev-ref", "main@{upstream}"}, want: "origin/main"},
		{name: "pushed", arguments: []string{"rev-list", "--count", "main...origin/main"}, want: "0"},
		// The commit exists and .env is not in it, which is what makes it the
		// denylist case a commit through the git channel has to exclude.
		{name: "tracked", arguments: []string{"ls-files"}, want: "README.md\ngo.mod\ninternal/app.go\nmain.go"},
		{name: "untracked", arguments: []string{"ls-files", "--others", "--exclude-standard"}, want: ".env"},
	} {
		t.Run(test.name, func(t *testing.T) {
			run := exec.CommandContext(t.Context(), "git", append([]string{"-C", worktree}, test.arguments...)...)
			output, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("git %s: %v: %s", strings.Join(test.arguments, " "), err, output)
			}
			if got := strings.TrimSpace(string(output)); got != test.want {
				t.Fatalf("git %s = %q, want %q", strings.Join(test.arguments, " "), got, test.want)
			}
		})
	}
}

// logRunnerReports logs what every runner said it can serve for a workspace.
//
// It is what a preview workspace stuck in `requested` is almost always about:
// the claim gate of section 18.1 refused every runner, and the gate's whole
// input is this column beside the heartbeat that dates it. Without the dump
// the failure says only that nothing happened, which is the one thing already
// obvious.
func (f *browserHostedFixture) logRunnerReports(t *testing.T) {
	t.Helper()
	rows, err := f.service.database.db.QueryContext(t.Context(),
		"SELECT id, workspace_capabilities_json, workspace_isolation, last_heartbeat_at FROM runner_identities")
	if err != nil {
		t.Logf("read runner workspace reports: %v", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, reported, isolation, beat string
		if err := rows.Scan(&id, &reported, &isolation, &beat); err != nil {
			t.Logf("scan runner workspace report: %v", err)
			return
		}
		t.Logf("runner %s reported %s (%s) at %s", id, reported, isolation, beat)
	}
	if err := rows.Err(); err != nil {
		t.Logf("iterate runner workspace reports: %v", err)
	}
}

// fixedPreviewWorktree hands every workspace the same seeded tree. The preview
// never checks anything out: the point is the surface, not the checkout.
type fixedPreviewWorktree struct{ path string }

func (w fixedPreviewWorktree) Prepare(context.Context, hubclient.WorkspaceCheckout) (string, error) {
	return w.path, nil
}

func (w fixedPreviewWorktree) Release(context.Context, string, hubclient.WorkspaceCheckout) error {
	return nil
}

// workspaceRunnerHeartbeatFreshness reports what the claim gate sees for the
// enrolled workspace runner right now: what it says it can serve, and whether
// the beat that said so still counts.
func (f *browserHostedFixture) workspaceRunnerHeartbeatFreshness(t *testing.T) (workspacesession.Capabilities, bool) {
	t.Helper()
	var identity string
	if err := f.service.database.db.QueryRowContext(t.Context(),
		"SELECT id FROM runner_identities WHERE machine_id = ?", string(f.workspaceRunnerMachine)).Scan(&identity); err != nil {
		t.Fatalf("find the workspace runner identity: %v", err)
	}
	capabilities, _, fresh, err := runnerWorkspaceCapabilities(t.Context(), f.service.database.db, identity, f.service.config.now().UTC())
	if err != nil {
		t.Fatalf("read the workspace runner capabilities: %v", err)
	}
	return capabilities, fresh
}

// ageWorkspaceRunnerHeartbeat backdates the runner's last beat, which is how a
// preview that has been up for hours looks to the claim gate.
func (f *browserHostedFixture) ageWorkspaceRunnerHeartbeat(t *testing.T, age time.Duration) {
	t.Helper()
	stale := formatHubTime(f.service.config.now().UTC().Add(-age))
	if _, err := f.service.database.db.ExecContext(t.Context(),
		"UPDATE runner_identities SET last_heartbeat_at = ? WHERE machine_id = ?", stale, string(f.workspaceRunnerMachine)); err != nil {
		t.Fatalf("age the workspace runner heartbeat: %v", err)
	}
}

// TestHostedBrowserPreviewWorkspaceRunnerOutlivesItsFirstHeartbeat is the
// regression for a Files panel opened hours into a preview waiting on
// "Workspace · requested" for ever.
//
// The lane was never the problem: it kept polling, and the log showed neither
// a claim nor a failed claim because the gate had stopped offering it anything.
// Section 18.1 offers a workspace item only to a runner that reports every
// required capability "with a fresh heartbeat", that report rides the machine
// beat, and the preview seeded exactly one. Two minutes -- runnerauth.
// HeartbeatTimeout -- after start-up the runner was invisible to the gate,
// so the pre-warmed workspace was the only one the preview could ever serve.
//
// The test walks that timeline: pre-warm, close it as the idle timeout would,
// age the beat past the window, and then require that the preview's own
// refresh brings the runner back and the next request is actually claimed.
func TestHostedBrowserPreviewWorkspaceRunnerOutlivesItsFirstHeartbeat(t *testing.T) {
	t.Parallel()
	f := newBrowserHostedFixture(t, true)
	f.seedWorkspaceRunner(t)
	base := browserHostedOrganizationBase + "/projects/" + f.project

	// The pre-warmed workspace goes the way the preview's did: closed, so the
	// next Files click has to open a new one rather than reuse it.
	f.api(t, "owner", http.MethodDelete, base+"/workspaces/"+f.prewarmed.ID, nil, http.StatusNoContent)

	f.ageWorkspaceRunnerHeartbeat(t, runnerauth.HeartbeatTimeout+time.Minute)
	if _, fresh := f.workspaceRunnerHeartbeatFreshness(t); fresh {
		t.Fatal("a runner that has not beaten for longer than the heartbeat timeout is still fresh to the claim gate")
	}

	// This is the whole fix: the preview beats for its runner, so the gate can
	// see it again.
	f.refreshPreviewRunners(t)
	capabilities, fresh := f.workspaceRunnerHeartbeatFreshness(t)
	if !fresh {
		t.Fatal("the preview's refresh did not restore the workspace runner's heartbeat")
	}
	requires := []string{workspacesession.CapabilityFiles, workspacesession.CapabilityExec}
	if !capabilities.Satisfies(requires) {
		t.Fatalf("workspace capabilities after the refresh = %#v, want files and exec", capabilities)
	}

	// A request made after the previous workspace closed is accepted, and the
	// lane takes it all the way to ready without the preview being restarted.
	var opened workspacesession.Session
	browserHostedDecode(t, f.api(t, "owner", http.MethodPost, base+"/workspaces", map[string]any{
		"idempotency_key": "browser-preview-workspace-after-close",
		"work_item_id":    f.workItem,
		"requires":        requires,
	}, http.StatusCreated), &opened)
	if opened.ID == f.prewarmed.ID {
		t.Fatalf("the request replayed the closed workspace %s instead of opening a new one", opened.ID)
	}
	deadline := time.Now().Add(45 * time.Second)
	for {
		var current workspacesession.Session
		browserHostedDecode(t, f.api(t, "owner", http.MethodGet, base+"/workspaces/"+opened.ID, nil, http.StatusOK), &current)
		if current.State == workspacesession.StateReady || current.State == workspacesession.StateIdle {
			if current.RunnerID == "" {
				t.Fatalf("workspace %s reached %s with no runner", current.ID, current.State)
			}
			return
		}
		if workspacesession.Terminal(current.State) {
			t.Fatalf("workspace %s ended as %s (%s) before it was ready", current.ID, current.State, current.Reason)
		}
		if time.Now().After(deadline) {
			t.Fatalf("workspace %s stopped at %s (%s); the lane was never offered it", current.ID, current.State, current.Reason)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestHostedBrowserPreview(t *testing.T) {
	if os.Getenv("DETENT_HOSTED_BROWSER_PREVIEW") == "" {
		t.Skip("set DETENT_HOSTED_BROWSER_PREVIEW=1 to run the isolated browser preview")
	}
	duration := browserPreviewDuration()
	f := newBrowserHostedFixture(t, true)
	// The preview draws the pull request panel, so it gets the seed the
	// ordinary hosted assertions do not want.
	f.seedPullRequest(t)
	// And the stored attempt diff the Diff surface draws (section 18.5), so
	// the preview shows a diff rather than only the round behind it.
	f.seedAttemptDiff(t)
	f.seedWorkspaceRunner(t)
	accounts := make(map[string]string, len(f.cookies))
	for account := range f.cookies {
		accounts[account] = f.server.URL + "/__preview/account/" + account
	}
	fixture := struct {
		URL            string `json:"url"`
		Login          string `json:"login"`
		Organization   string `json:"organization"`
		Project        string `json:"project"`
		PrivateProject string `json:"private_project"`
		Chat           string `json:"chat"`
		ProjectID      string `json:"project_id"`
		Conversation   string `json:"conversation"`
		WorkItem       string `json:"work_item"`
		PullRequests   string `json:"pull_requests"`
		// Workspace is the pre-warmed files-and-exec workspace on WorkItem, so
		// a spec can queue a project action run through the API alone.
		Workspace string            `json:"workspace"`
		Accounts  map[string]string `json:"accounts"`
		// OwnerEmail names the organization owner's row in the members table.
		// A spec that has to find that one row needs the address, and guessing
		// at it from `Accounts` — which maps an account name to a sign-in URL
		// — is how a locator ends up matching every row instead.
		OwnerEmail string    `json:"owner_email"`
		Stop       string    `json:"stop"`
		Expires    time.Time `json:"expires"`
	}{f.server.URL, f.server.URL + "/login", f.server.URL + "/organization", f.server.URL + "/projects/" + f.project, f.server.URL + "/projects/" + f.privateProject, f.server.URL + "/chat", f.project, f.conversation, f.workItem,
		f.server.URL + browserHostedOrganizationBase + "/projects/" + f.project + "/work-items/" + f.workItem + "/pull-requests",
		f.workspace, accounts, browserHostedOwnerEmail, f.server.URL + "/__preview/stop", time.Now().Add(duration)}
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hosted-browser-preview.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Hosted browser fixture: %s", path)
	t.Logf("Hosted browser URL: %s", f.server.URL)
	timer := time.NewTimer(duration)
	defer timer.Stop()
	// Both seeded runners age out -- the catalogue after
	// providercapacity.MaxAge, the workspace runner after
	// runnerauth.HeartbeatTimeout -- and a real runner re-reports on every
	// heartbeat, so the preview does the same on their behalf. Without the
	// workspace beat the lane stays running and simply stops being offered
	// work, so a Files panel opened later waits on a claim that never comes.
	refresh := time.NewTicker(previewRunnerRefreshInterval())
	defer refresh.Stop()
	for {
		select {
		case <-refresh.C:
			f.refreshPreviewRunners(t)
		case <-f.stop:
			return
		case <-timer.C:
			return
		case <-t.Context().Done():
			return
		}
	}
}
