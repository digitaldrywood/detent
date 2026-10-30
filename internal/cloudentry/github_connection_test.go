package cloudentry

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/digitaldrywood/detent/internal/connector/github"
)

type runtimeTransport func(*http.Request) (*http.Response, error)

func (f runtimeTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGitHubRepositoryConnection(t *testing.T) {
	f := newEntryFixture(t, func(cfg *Config) {
		cfg.LookupEnv = entrySecretEnvironment(1, 1)
	})
	s := f.service
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	reject := false
	var mu sync.Mutex
	recordedCalls := func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		return maps.Clone(calls)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.URL.RequestURI()]++
		shouldReject := reject
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if shouldReject {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"customer private repository secret"}`)
			return
		}
		switch r.URL.Path {
		case "/app":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Error("app metadata requires app token")
			}
			fmt.Fprint(w, `{"slug":"example-app"}`)
		case "/login/oauth/access_token":
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			if r.Form.Get("client_secret") != "client-secret" || r.Form.Get("code_verifier") == "" || r.Form.Get("redirect_uri") != testPublicURL+"/auth/github/callback" {
				t.Errorf("exchange did not include app credentials and PKCE")
			}
			fmt.Fprint(w, `{"access_token":"user-token","token_type":"bearer","expires_in":28800}`)
		case "/user/installations":
			if r.Header.Get("Authorization") != "Bearer user-token" {
				t.Error("listing requires user token")
			}
			installations := []map[string]int{}
			if r.URL.Query().Get("page") == "1" {
				for id := 1; id <= 100; id++ {
					installations = append(installations, map[string]int{"id": id})
				}
			} else {
				installations = append(installations, map[string]int{"id": 101})
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"installations": installations}); err != nil {
				t.Error(err)
			}
		default:
			if r.URL.Query().Get("per_page") != "100" {
				t.Error("wrong page size")
			}
			repos := []map[string]any{}
			switch {
			case r.URL.Path == "/user/installations/1/repositories" && r.URL.Query().Get("page") == "1":
				for id := 1; id <= 100; id++ {
					repos = append(repos, map[string]any{"name": fmt.Sprintf("repository-%03d", id), "owner": map[string]string{"login": "example-org"}, "private": true})
				}
			case r.URL.Path == "/user/installations/1/repositories":
				repos = append(repos, map[string]any{"name": "repository-tail", "owner": map[string]string{"login": "example-org"}})
			case r.URL.Path == "/user/installations/101/repositories":
				repos = append(repos, map[string]any{"name": "second-account", "owner": map[string]string{"login": "example-user"}})
			default:
				repos = append(repos, map[string]any{"name": "Repository-001", "owner": map[string]string{"login": "Example-Org"}})
			}
			if err := json.NewEncoder(w).Encode(map[string]any{"repositories": repos}); err != nil {
				t.Error(err)
			}
		}
	}))
	t.Cleanup(api.Close)
	endpoint, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	s.config.GitHubApp = &github.InstallationTokenConfig{AppID: "123", PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})), HTTPClient: &http.Client{Transport: runtimeTransport(func(request *http.Request) (*http.Response, error) {
		request = request.Clone(request.Context())
		request.URL.Scheme, request.URL.Host = endpoint.Scheme, endpoint.Host
		return api.Client().Transport.RoundTrip(request)
	})}}
	s.config.GitHubUserClientID, s.config.GitHubUserClientSecret = "client-id", "client-secret"
	b := newBrowser(t, s.Handler())
	b.login("/organizations", "user_alice:")
	read := func(b *browser) githubRepositoryList {
		t.Helper()
		p := b.do(http.MethodGet, githubConnectionBase+"/repositories", nil, nil)
		if p.StatusCode != http.StatusOK {
			t.Fatalf("listing: %d %s", p.StatusCode, p.Body)
		}
		var result githubRepositoryList
		if err := json.Unmarshal([]byte(p.Body), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := read(b); result.Connected || result.Reconnect || !result.Available || len(recordedCalls()) != 0 {
		t.Fatalf("unconnected read=%+v calls=%v", result, recordedCalls())
	}
	clientID := s.config.GitHubUserClientID
	s.config.GitHubUserClientID = ""
	if result := read(b); result.Available || result.Connected || len(recordedCalls()) != 0 {
		t.Fatal("unconfigured App performed discovery")
	}
	s.config.GitHubUserClientID = clientID
	returnTo := "/organizations/org_alpha/projects/project_alpha/setup?setup_step=repository&setup_runner=runner_example"
	denied := attachmentRequest(t, b, http.MethodPost, githubConnectionBase+"/connect", strings.NewReader(`{"return_to":"`+returnTo+`"}`), map[string]string{"Content-Type": "application/json", "Origin": "https://other.example.test"})
	if denied.Code != http.StatusUnauthorized || len(recordedCalls()) != 0 {
		t.Fatal("cross-origin connect performed discovery")
	}
	start := attachmentRequest(t, b, http.MethodPost, githubConnectionBase+"/connect", strings.NewReader(`{"return_to":"`+returnTo+`"}`), map[string]string{"Content-Type": "application/json", "Origin": testPublicURL})
	if start.Code != http.StatusOK {
		t.Fatalf("start=%d %s", start.Code, start.Body.String())
	}
	base, err := url.Parse(testPublicURL)
	if err != nil {
		t.Fatal(err)
	}
	b.jar.SetCookies(base, start.Result().Cookies())
	var target struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &target); err != nil {
		t.Fatal(err)
	}
	install, err := url.Parse(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	if install.Host != "github.com" || install.Path != "/apps/example-app/installations/new" {
		t.Fatalf("install URL=%s", install)
	}
	if p := b.do(http.MethodGet, "/auth/github/callback?state=wrong&installation_id=1", nil, nil); p.StatusCode != 400 {
		t.Fatalf("tampered state=%+v", p)
	}
	installed := b.do(http.MethodGet, "/auth/github/callback?installation_id=1&state="+url.QueryEscape(install.Query().Get("state")), nil, nil)
	authorize, err := url.Parse(installed.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if authorize.Path != "/login/oauth/authorize" || authorize.Query().Get("code_challenge_method") != "S256" || authorize.Query().Get("code_challenge") == "" {
		t.Fatalf("authorize=%s response=%+v", authorize, installed)
	}
	callback := "/auth/github/callback?code=accepted&state=" + url.QueryEscape(authorize.Query().Get("state"))
	connected := b.do(http.MethodGet, callback, nil, nil)
	if connected.StatusCode != http.StatusSeeOther || connected.Header.Get("Location") != returnTo {
		t.Fatalf("callback=%+v", connected)
	}
	if replay := b.do(http.MethodGet, callback, nil, nil); replay.StatusCode != 400 {
		t.Fatalf("replayed callback=%+v", replay)
	}
	result := read(b)
	if !result.Connected || result.Reconnect || len(result.Repositories) != 102 || !result.Repositories[0].Private {
		t.Fatalf("connected list=%+v", result)
	}
	for _, path := range []string{"/user/installations?per_page=100&page=2", "/user/installations/1/repositories?per_page=100&page=2", "/user/installations/101/repositories?per_page=100&page=1"} {
		if count := recordedCalls()[path]; count != 1 {
			t.Errorf("pagination %s calls=%d", path, count)
		}
	}
	var sealed string
	if err := s.auth.store.reader.QueryRowContext(t.Context(), `SELECT access_token FROM github_user_connections WHERE owner_subject='user_alice'`).Scan(&sealed); err != nil || !strings.HasPrefix(sealed, "hk1.") || strings.Contains(sealed, "user-token") {
		t.Fatalf("token storage not encrypted: %v", err)
	}
	other := newBrowser(t, s.Handler())
	other.login("/organizations", "user_bob:")
	if result := read(other); result.Connected || len(result.Repositories) != 0 {
		t.Fatal("another user received the connection")
	}
	if err := s.saveGitHubConnection(t.Context(), "user_bob", &oauth2.Token{AccessToken: "user-token"}); err != nil {
		t.Fatal(err)
	}
	if result := read(other); !result.Connected {
		t.Fatal("second user connection unusable")
	}
	grants := b.do(http.MethodGet, "/api/cloud/account/oauth-grants", nil, nil)
	var page oauthGrantPage
	if err := json.Unmarshal([]byte(grants.Body), &page); err != nil || len(page.Grants) != 1 || page.Grants[0].ClientName != "Detent Cloud GitHub App" {
		t.Fatalf("grants=%+v %v", grants, err)
	}
	id := page.Grants[0].ID
	if p := other.do(http.MethodDelete, "/api/cloud/account/oauth-grants/"+id, nil, nil); p.StatusCode != 404 {
		t.Fatal("foreign user revoked connection")
	}
	if p := b.do(http.MethodDelete, "/api/cloud/account/oauth-grants/"+id, nil, nil); p.StatusCode != 204 {
		t.Fatalf("revoke=%+v", p)
	}
	before := recordedCalls()["/user/installations?per_page=100&page=1"]
	if result := read(b); !result.Reconnect || result.Connected || len(result.Repositories) != 0 || recordedCalls()["/user/installations?per_page=100&page=1"] != before {
		t.Fatal("revoked token called GitHub or leaked repositories")
	}
	for _, test := range []struct {
		name    string
		expired bool
	}{{"expired", true}, {"remote revoked", false}} {
		t.Run(test.name, func(t *testing.T) {
			token := &oauth2.Token{AccessToken: "user-token"}
			if test.expired {
				token.Expiry = s.config.now().Add(-time.Hour)
			}
			if err := s.saveGitHubConnection(t.Context(), "user_alice", token); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			reject = !test.expired
			mu.Unlock()
			before := recordedCalls()["/user/installations?per_page=100&page=1"]
			p := b.do(http.MethodGet, githubConnectionBase+"/repositories", nil, nil)
			if strings.Contains(p.Body, "customer") || strings.Contains(p.Body, "user-token") {
				t.Fatal("listing leaked customer content or credentials")
			}
			var result githubRepositoryList
			if err := json.Unmarshal([]byte(p.Body), &result); err != nil || !result.Reconnect || result.Connected || len(result.Repositories) != 0 {
				t.Fatalf("fallback=%+v %v", result, err)
			}
			if test.expired && recordedCalls()["/user/installations?per_page=100&page=1"] != before {
				t.Fatal("expired token called GitHub")
			}
			after := recordedCalls()["/user/installations?per_page=100&page=1"]
			if result := read(b); !result.Reconnect || recordedCalls()["/user/installations?per_page=100&page=1"] != after {
				t.Fatal("inactive connection continued calling GitHub")
			}
		})
	}
}

func TestGitHubSetupReturnPath(t *testing.T) {
	for _, test := range []struct {
		path  string
		valid bool
	}{
		{"/example-org/example-project/projects/prj_example/setup?setup_step=repository", true},
		{"/projects/prj_example/setup", true},
		{"//external.example/projects/prj_example/setup", false},
		{"https://external.example/projects/prj_example/setup", false},
		{"/projects/prj_example/setup\r\nLocation:evil", false},
		{"/projects/../setup", false},
		{"/projects/prj_example/setup#other", false},
	} {
		t.Run(test.path, func(t *testing.T) {
			if got := validSetupReturn(test.path); got != test.valid {
				t.Errorf("valid=%v", got)
			}
		})
	}
}
