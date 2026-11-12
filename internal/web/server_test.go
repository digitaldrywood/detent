package web_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	detent "github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/auth"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/web"
)

func TestMain(m *testing.M) {
	if err := os.Unsetenv("DETENT_API_TOKEN"); err != nil {
		panic("clear DETENT_API_TOKEN: " + err.Error())
	}
	os.Exit(m.Run())
}

func TestNewServerRejectsSessionAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		auth globalconfig.Auth
		want string
	}{
		{
			name: "magic link",
			auth: globalconfig.Auth{
				Mode:          globalconfig.AuthModeMagicLink,
				AllowedEmails: []string{"operator@example.com"},
				SMTP:          globalconfig.SMTP{Host: "smtp.example.com", From: "operator@example.com"},
			},
			want: "configure magic link auth: ",
		},
		{
			name: "OIDC",
			auth: globalconfig.Auth{Mode: globalconfig.AuthModeOIDC, AllowedEmails: []string{"operator@example.com"}},
			want: "configure oidc sessions: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := web.NewServer(web.Config{Mode: web.ModeOnboarding, GlobalConfig: globalconfig.Config{Auth: tt.auth}}, web.Dependencies{})
			if !errors.Is(err, auth.ErrMissingStore) || !strings.HasPrefix(err.Error(), tt.want) {
				t.Fatalf("NewServer() error = %v, want %q wrapping the missing auth store", err, tt.want)
			}
		})
	}
}

func TestServerStaticAssetsUseFingerprintsAndCacheHeaders(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	css := "body{color:purple}"
	favicon := `<svg xmlns="http://www.w3.org/2000/svg"><path fill="#3730A3" d="M0 0h1v1H0z"/></svg>`
	writeTestCSS(t, staticDir, css)
	writeTestStaticAsset(t, staticDir, "img/detent-mark.svg", favicon)

	server := newOnboardingServer(t, web.Config{StaticDir: staticDir})

	tests := []struct {
		name    string
		path    string
		content string
	}{
		{name: "stylesheet", path: "/static/css/output." + shortTestHash(css) + ".css", content: css},
		{name: "favicon", path: "/static/img/detent-mark." + shortTestHash(favicon) + ".svg", content: favicon},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tt.path, nil)

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
			}
			if rec.Body.String() != tt.content {
				t.Fatalf("body = %q, want %q", rec.Body.String(), tt.content)
			}
			if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
				t.Fatalf("Cache-Control = %q, want immutable static caching", got)
			}
			if got := rec.Header().Get("ETag"); got == "" {
				t.Fatal("ETag is empty")
			}
		})
	}
}

func TestServerLegacyStaticAssetsRequireRevalidation(t *testing.T) {
	t.Parallel()

	staticDir := t.TempDir()
	css := "body{color:green}"
	writeTestCSS(t, staticDir, css)

	server := newOnboardingServer(t, web.Config{StaticDir: staticDir})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/output.css", nil)

	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if rec.Body.String() != css {
		t.Fatalf("body = %q, want %q", rec.Body.String(), css)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", got)
	}
	if got := rec.Header().Get("ETag"); got == "" {
		t.Fatal("ETag is empty")
	}
}

func TestServerServesDefaultStaticAssetsFromArbitraryWorkingDirectory(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}
	defer func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore Chdir() error = %v", err)
		}
	}()

	server := newOnboardingServer(t, web.Config{})

	var paths []string
	chunks := 0
	clientPrepared := false
	if err := fs.WalkDir(detent.StaticFS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && name == "app/conversation" {
			clientPrepared = true
		}
		if !entry.IsDir() {
			paths = append(paths, name)
			if strings.HasPrefix(name, "app/conversation/chunks/") {
				chunks++
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("embedded static assets are missing")
	}
	if clientPrepared {
		paths = append(paths, "app/conversation/app.js", "app/conversation/app.css", "app/conversation/THIRD_PARTY_LICENSES.txt", "app/conversation/index.html")
		if chunks == 0 {
			t.Fatal("conversation lazy chunks are missing")
		}
	}
	for _, name := range slices.Compact(slices.Sorted(slices.Values(paths))) {
		t.Run(name, func(t *testing.T) {
			expected, err := fs.ReadFile(detent.StaticFS(), name)
			if err != nil || len(expected) == 0 {
				t.Fatalf("embedded asset %s: %v", name, err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/static/"+name, nil)
			server.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), expected) {
				t.Fatalf("served asset %s: status %d, bytes %d, want %d", name, rec.Code, rec.Body.Len(), len(expected))
			}
			if got := rec.Header().Get("Content-Type"); strings.HasSuffix(name, ".css") && !strings.HasPrefix(got, "text/css") {
				t.Fatalf("Content-Type = %q, want text/css", got)
			}
		})
	}
}

func TestOnboardingModeDoesNotRequireRuntimeDependencies(t *testing.T) {
	t.Parallel()

	workflow := filepath.Join(t.TempDir(), "WORKFLOW.md")
	server := newOnboardingServer(t, web.Config{StaticDir: t.TempDir(), WorkflowPath: workflow})

	tests := []struct {
		name        string
		method      string
		path        string
		header      string
		wantStatus  int
		wantContent string
	}{
		{name: "retired setup root", method: http.MethodGet, path: "/", wantStatus: http.StatusNotFound},
		{name: "onboarding page", method: http.MethodGet, path: "/onboarding", wantStatus: http.StatusNotFound},
		{name: "retired dashboard API", method: http.MethodGet, path: "/api/v1/state", wantStatus: http.StatusNotFound, wantContent: `"code":"not_found"`},
		{name: "health", method: http.MethodGet, path: "/health", wantStatus: http.StatusOK, wantContent: `"mode":"onboarding"`},
		{name: "health with bearer credential", method: http.MethodGet, path: "/health", header: "Authorization", wantStatus: http.StatusOK, wantContent: `"mode":"onboarding"`},
		{name: "health with API key credential", method: http.MethodGet, path: "/health", header: "X-API-Key", wantStatus: http.StatusOK, wantContent: `"mode":"onboarding"`},
		{name: "retired tracker step", method: http.MethodPost, path: "/onboarding/tracker", wantStatus: http.StatusNotFound},
		{name: "retired credentials step", method: http.MethodPost, path: "/onboarding/credentials", wantStatus: http.StatusNotFound},
		{name: "retired project step", method: http.MethodPost, path: "/onboarding/project", wantStatus: http.StatusNotFound},
		{name: "retired agent step", method: http.MethodPost, path: "/onboarding/agent", wantStatus: http.StatusNotFound},
		{name: "retired write step", method: http.MethodPost, path: "/onboarding/write", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tt.method, tt.path, nil)
			if tt.header != "" {
				req.Header.Set(tt.header, "Bearer unused-credential")
			}

			server.Handler().ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantContent != "" && !strings.Contains(rec.Body.String(), tt.wantContent) {
				t.Fatalf("body missing %q:\n%s", tt.wantContent, rec.Body.String())
			}
			if _, err := os.Stat(workflow); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("retired setup wrote workflow: %v", err)
			}
		})
	}
}

func TestHealthReportsEnvironmentPath(t *testing.T) {
	t.Parallel()

	server := newOnboardingServer(t, web.Config{})
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var payload struct {
		Environment struct {
			Path string `json:"path"`
		} `json:"environment"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if payload.Environment.Path != os.Getenv("PATH") {
		t.Fatalf("environment path = %q, want %q", payload.Environment.Path, os.Getenv("PATH"))
	}
}

func newOnboardingServer(t *testing.T, cfg web.Config) *web.Server {
	t.Helper()
	cfg.Mode = web.ModeOnboarding
	server, err := web.NewServer(cfg, web.Dependencies{})
	if err != nil {
		t.Fatalf("NewServer() error = %v", err)
	}
	return server
}

func writeTestCSS(t *testing.T, staticDir string, css string) {
	t.Helper()

	writeTestStaticAsset(t, staticDir, "css/output.css", css)
}

func writeTestStaticAsset(t *testing.T, staticDir string, name string, content string) {
	t.Helper()

	filePath := filepath.Join(staticDir, name)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func shortTestHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:12]
}
