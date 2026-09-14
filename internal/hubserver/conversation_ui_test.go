package hubserver

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// conversationHostedFixture is a hosted hub with the conversation product
// enabled and three members: an owner with write access, a member with a
// read-only grant and a viewer with a grant.
type conversationHostedFixture struct {
	service  *Service
	provider *browserHostedProvider
	cookies  map[string]*http.Cookie
	project  string
	base     string
}

const conversationHostedOrganization = "org_chat_preview"

func newConversationHostedFixture(t *testing.T) *conversationHostedFixture {
	t.Helper()
	base := "https://hosted.chat.test"
	provider := &browserHostedProvider{
		base: base, organization: auth.Organization{ID: "org_chat_provider", ExternalID: conversationHostedOrganization, Name: "Chat organization"},
		members: make(map[string]auth.Membership), sessions: make(map[string]auth.HostedIdentity), invitations: make(map[string]auth.Invitation), inviteRoles: make(map[string]string), authorizations: make(map[string]string), codes: make(map[string]auth.Identity),
	}
	cfg := Config{InitialAdminToken: []byte(testHubAdminToken), DatabasePath: filepath.Join(t.TempDir(), "hosted-chat.db"), GitHubDisabled: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Conversation: &ConversationConfig{Enabled: true},
		Hosted: &HostedConfig{
			OrganizationID: conversationHostedOrganization, BootstrapSubject: "user_chat_owner", PublicURL: base, Provider: provider, WorkOSOrganizationID: provider.organization.ID,
			Directory: []HostedDestination{{OrganizationID: conversationHostedOrganization, WorkOSOrganizationID: provider.organization.ID, PublicURL: base}},
		}}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	f := &conversationHostedFixture{service: service, provider: provider, cookies: make(map[string]*http.Cookie), base: "/api/v2/organizations/" + conversationHostedOrganization}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, account := range []struct{ name, user, email, role string }{
		{name: "owner", user: "user_chat_owner", email: "owner@chat.test", role: "owner"},
		{name: "member", user: "user_chat_member", email: "member@chat.test", role: "member"},
		{name: "viewer", user: "user_chat_viewer", email: "viewer@chat.test", role: "viewer"},
	} {
		identity := provider.identity(account.user, account.email, provider.organization.ID, "")
		membership, err := provider.CreateMembership(t.Context(), account.user, provider.organization.ID, account.role)
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
		token, session, err := service.hostedSessions.CreateIdentitySession(t.Context(), identity)
		if err != nil {
			t.Fatal(err)
		}
		f.cookies[account.name] = &http.Cookie{Name: hostedCookie, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt}
	}
	var project tracker.NativeProject
	response := f.request(t, "owner", http.MethodPost, f.base+"/projects", map[string]any{"idempotency_key": "chat-project", "name": "Chat project", "grant_access": true})
	browserHostedStatus(t, response, http.StatusCreated)
	browserHostedDecode(t, response, &project)
	f.project = string(project.ID)
	if !strings.HasPrefix(f.project, "prj_") {
		t.Fatalf("project = %#v", project)
	}
	for _, user := range []string{"user_chat_member", "user_chat_viewer"} {
		f.grant(t, "owner", user, f.project, false, false, false)
	}
	f.base += "/projects/" + f.project
	return f
}

func (f *conversationHostedFixture) request(t *testing.T, account, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		body = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequest(method, path, body)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if cookie := f.cookies[account]; cookie != nil {
		request.AddCookie(cookie)
		request.Header.Set("X-CSRF-Token", hostedCSRF(cookie.Value))
	}
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

// form posts one form-encoded body, the way a browser without the client
// would. Only the logout redirect still accepts one (decisions section 12).
func (f *conversationHostedFixture) form(t *testing.T, account, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	cookie := f.cookies[account]
	if cookie == nil {
		t.Fatal("account session cookie is missing")
	}
	if values == nil {
		values = url.Values{}
	}
	values.Set("csrf", hostedCSRF(cookie.Value))
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	f.service.Handler().ServeHTTP(response, request)
	return response
}

// grant changes one member's project access through the section 12 endpoint.
func (f *conversationHostedFixture) grant(t *testing.T, account, user, project string, write, runner, revoke bool) {
	t.Helper()
	response := f.request(t, account, http.MethodPut, "/api/v2/organizations/"+conversationHostedOrganization+"/members/membership_"+user+"/grants", map[string]any{
		"idempotency_key": "grant-" + user + "-" + project, "project_id": project, "write": write, "runner": runner, "revoke": revoke,
	})
	browserHostedStatus(t, response, http.StatusOK)
}

type conversationBootstrapResponse struct {
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
	Actor struct {
		PrincipalID string `json:"principal_id"`
		Subject     string `json:"subject"`
		Email       string `json:"email"`
		Role        string `json:"role"`
	} `json:"actor"`
	Projects []struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		CanWrite bool   `json:"can_write"`
	} `json:"projects"`
	CSRFToken    string `json:"csrf_token"`
	Capabilities struct {
		Coordinator bool `json:"coordinator"`
		Attachments bool `json:"attachments"`
	} `json:"capabilities"`
	APIBase string `json:"api_base"`
	Feature struct {
		Conversation bool `json:"conversation"`
	} `json:"feature"`
}

func TestConversationUIShell(t *testing.T) {
	f := newConversationHostedFixture(t)
	tests := []struct {
		name     string
		account  string
		path     string
		status   int
		location string
		contains string
	}{
		{name: "anonymous redirects to login", path: "/chat", status: http.StatusSeeOther, location: "/login"},
		{name: "anonymous nested redirects", path: "/chat/c/conv_1", status: http.StatusSeeOther, location: "/login"},
		{name: "owner shell", account: "owner", path: "/chat", status: http.StatusOK, contains: `id="root"`},
		{name: "viewer nested shell", account: "viewer", path: "/chat/p/" + f.project, status: http.StatusOK, contains: "/static/app/conversation/app.js"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := f.request(t, tt.account, http.MethodGet, tt.path, nil)
			browserHostedStatus(t, response, tt.status)
			if tt.location != "" && response.Header().Get("Location") != tt.location {
				t.Fatalf("location = %q, want %q", response.Header().Get("Location"), tt.location)
			}
			if tt.contains != "" {
				if !strings.Contains(response.Body.String(), tt.contains) {
					t.Fatalf("shell missing %q: %s", tt.contains, response.Body.String())
				}
				if got := response.Header().Get("Cache-Control"); got != "no-cache" {
					t.Fatalf("cache control = %q", got)
				}
				if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
					t.Fatalf("content type = %q", got)
				}
			}
		})
	}
	t.Run("missing client", func(t *testing.T) {
		restore := conversationClientFS
		conversationClientFS = fstest.MapFS{}
		t.Cleanup(func() { conversationClientFS = restore })
		response := f.request(t, "owner", http.MethodGet, "/chat", nil)
		browserHostedStatus(t, response, http.StatusServiceUnavailable)
		var failure apiErrorResponse
		decodeHubResponse(t, response, &failure)
		if failure.Code != "client_unavailable" {
			t.Fatalf("code = %q", failure.Code)
		}
	})
	t.Run("static bundle is served", func(t *testing.T) {
		response := f.request(t, "", http.MethodGet, "/static/app/conversation/app.js", nil)
		browserHostedStatus(t, response, http.StatusOK)
	})
}

func TestConversationUIBootstrap(t *testing.T) {
	f := newConversationHostedFixture(t)
	response := f.request(t, "", http.MethodGet, "/chat/bootstrap", nil)
	browserHostedStatus(t, response, http.StatusUnauthorized)
	tests := []struct {
		name     string
		account  string
		subject  string
		email    string
		role     string
		canWrite bool
	}{
		{name: "owner", account: "owner", subject: "user_chat_owner", email: "owner@chat.test", role: "owner", canWrite: true},
		{name: "read-only member", account: "member", subject: "user_chat_member", email: "member@chat.test", role: "member", canWrite: false},
		{name: "viewer", account: "viewer", subject: "user_chat_viewer", email: "viewer@chat.test", role: "viewer", canWrite: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := f.request(t, tt.account, http.MethodGet, "/chat/bootstrap", nil)
			browserHostedStatus(t, response, http.StatusOK)
			var bootstrap conversationBootstrapResponse
			decodeHubResponse(t, response, &bootstrap)
			if bootstrap.Organization.ID != conversationHostedOrganization || bootstrap.Organization.Name == "" {
				t.Fatalf("organization = %#v", bootstrap.Organization)
			}
			var principal string
			if err := f.service.database.db.QueryRowContext(t.Context(), "SELECT principal_id FROM hosted_members WHERE user_id = ?", tt.subject).Scan(&principal); err != nil {
				t.Fatal(err)
			}
			if bootstrap.Actor.PrincipalID != principal || bootstrap.Actor.Subject != tt.subject || bootstrap.Actor.Email != tt.email || bootstrap.Actor.Role != tt.role {
				t.Fatalf("actor = %#v", bootstrap.Actor)
			}
			if len(bootstrap.Projects) != 1 || bootstrap.Projects[0].ID != f.project || bootstrap.Projects[0].Name != "Chat project" || bootstrap.Projects[0].CanWrite != tt.canWrite {
				t.Fatalf("projects = %#v", bootstrap.Projects)
			}
			if bootstrap.CSRFToken != hostedCSRF(f.cookies[tt.account].Value) {
				t.Fatalf("csrf token = %q", bootstrap.CSRFToken)
			}
			// Attachments come with the conversation product; the
			// coordinator needs a backend or an enrolled runner (section 17.1).
			if bootstrap.Capabilities.Coordinator || !bootstrap.Capabilities.Attachments || bootstrap.APIBase != "/api/v2/organizations/"+conversationHostedOrganization || !bootstrap.Feature.Conversation {
				t.Fatalf("bootstrap = %#v", bootstrap)
			}
			if got := response.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("cache control = %q", got)
			}
		})
	}
}

func TestConversationUIHostedAPIAccess(t *testing.T) {
	f := newConversationHostedFixture(t)
	response := f.request(t, "owner", http.MethodPost, f.base+"/conversations", map[string]any{"key": "hosted-create", "title": "Hosted chat"})
	browserHostedStatus(t, response, http.StatusCreated)
	var created conversationCreateResponse
	decodeHubResponse(t, response, &created)
	if created.Conversation.Owner.Subject != "user_chat_owner" || created.Conversation.Visibility != conversation.VisibilityPrivate {
		t.Fatalf("conversation = %#v", created.Conversation)
	}
	response = f.request(t, "owner", http.MethodPost, f.base+"/conversations/"+created.Conversation.ID+"/link", map[string]any{"key": "hosted-link", "share_history": true, "issue": map[string]any{"title": "Hosted issue", "description": "Shared"}})
	browserHostedStatus(t, response, http.StatusOK)
	message := conversation.Command{Key: "hosted-message", Kind: conversation.CommandMessage, Text: "Hello from a member"}
	tests := []struct {
		name    string
		account string
		method  string
		path    string
		payload any
		status  int
		code    string
	}{
		{name: "member reads shared", account: "member", method: http.MethodGet, path: f.base + "/conversations/" + created.Conversation.ID, status: http.StatusOK},
		{name: "viewer reads shared", account: "viewer", method: http.MethodGet, path: f.base + "/conversations/" + created.Conversation.ID, status: http.StatusOK},
		{name: "member lists", account: "member", method: http.MethodGet, path: f.base + "/conversations", status: http.StatusOK},
		{name: "member organization list", account: "member", method: http.MethodGet, path: "/api/v2/organizations/" + conversationHostedOrganization + "/conversations", status: http.StatusOK},
		{name: "read-only member cannot command", account: "member", method: http.MethodPost, path: f.base + "/conversations/" + created.Conversation.ID + "/commands", payload: message, status: http.StatusForbidden, code: "forbidden"},
		{name: "viewer cannot command", account: "viewer", method: http.MethodPost, path: f.base + "/conversations/" + created.Conversation.ID + "/commands", payload: message, status: http.StatusForbidden, code: "forbidden"},
		{name: "read-only member cannot create", account: "member", method: http.MethodPost, path: f.base + "/conversations", payload: map[string]any{"key": "nope", "title": "Nope"}, status: http.StatusForbidden, code: "forbidden"},
		{name: "owner commands", account: "owner", method: http.MethodPost, path: f.base + "/conversations/" + created.Conversation.ID + "/commands", payload: message, status: http.StatusOK},
		{name: "wrong organization", account: "owner", method: http.MethodGet, path: "/api/v2/organizations/org_other/conversations", status: http.StatusNotFound},
		{name: "anonymous", account: "", method: http.MethodGet, path: f.base + "/conversations", status: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := f.request(t, tt.account, tt.method, tt.path, tt.payload)
			browserHostedStatus(t, response, tt.status)
			if tt.code != "" {
				var failure apiErrorResponse
				decodeHubResponse(t, response, &failure)
				if failure.Code != tt.code {
					t.Fatalf("code = %q, want %q", failure.Code, tt.code)
				}
			}
		})
	}
	t.Run("organization list contains the shared conversation", func(t *testing.T) {
		response := f.request(t, "member", http.MethodGet, "/api/v2/organizations/"+conversationHostedOrganization+"/conversations", nil)
		browserHostedStatus(t, response, http.StatusOK)
		var page conversationListResponse
		decodeHubResponse(t, response, &page)
		if len(page.Conversations) != 1 || page.Conversations[0].ID != created.Conversation.ID {
			t.Fatalf("conversations = %#v", page.Conversations)
		}
	})
	t.Run("hosted stream closes when the grant is revoked", func(t *testing.T) {
		server := httptest.NewServer(f.service.Handler())
		t.Cleanup(server.Close)
		restoreHeartbeat, restoreAuthorize := conversationStreamHeartbeat, conversationStreamAuthorize
		conversationStreamHeartbeat, conversationStreamAuthorize = 40*time.Millisecond, 40*time.Millisecond
		t.Cleanup(func() { conversationStreamHeartbeat, conversationStreamAuthorize = restoreHeartbeat, restoreAuthorize })
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+f.base+"/conversations/"+created.Conversation.ID+"/events?after=0", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.AddCookie(f.cookies["member"])
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = response.Body.Close() })
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", response.StatusCode)
		}
		stream := &sseStream{response: response, reader: bufio.NewReader(response.Body), cancel: func() {}}
		if frame := stream.nextEvent(t); frame.ID != "1" {
			t.Fatalf("first frame = %#v", frame)
		}
		for frame := stream.next(t); frame.Event != string(conversation.EventHeartbeat); frame = stream.next(t) {
			// Drain the replay of the link events.
		}
		f.grant(t, "owner", "user_chat_member", f.project, false, false, true)
		requireClosed(t, stream.nextEvent(t), "access_revoked")
	})
}

func TestConversationUINotMountedWithoutHosting(t *testing.T) {
	t.Parallel()
	f := newConversationAPIFixture(t, nil)
	for _, path := range []string{"/chat", "/chat/bootstrap", "/chat/c/conv_1"} {
		response := performHubAPIRequest(t, f.service, http.MethodGet, path, "", nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", path, response.Code)
		}
	}
}
