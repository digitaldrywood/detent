package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	PublicURL = "http://127.0.0.1:18017"
	AdminKey  = "detent_test_admin_token_0123456789abcdefghijklmnop"
)

func JSONBody(t *testing.T, value any) io.Reader {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(raw)
}

type Browser struct {
	T      *testing.T
	Entry  http.Handler
	Jar    *cookiejar.Jar
	Origin string
}

type Page struct {
	StatusCode int
	Header     http.Header
	Body       string
}

func NewBrowser(t *testing.T, entry http.Handler) *Browser {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Browser{T: t, Entry: entry, Jar: jar, Origin: PublicURL}
}

func (b *Browser) Do(method, target string, form url.Values, headers map[string]string) Page {
	b.T.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, b.Origin+target, body)
	if method == http.MethodGet || method == http.MethodHead {
		request.Header.Set("Sec-Fetch-Mode", "navigate")
		request.Header.Set("Sec-Fetch-Dest", "document")
	}
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method != http.MethodGet {
		request.Header.Set("Origin", b.Origin)
	}
	for key, value := range headers {
		if value == "" {
			request.Header.Del(key)
		} else {
			request.Header.Set(key, value)
		}
	}
	base, _ := url.Parse(b.Origin)
	for _, cookie := range b.Jar.Cookies(base) {
		request.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	b.Entry.ServeHTTP(recorder, request)
	b.Jar.SetCookies(base, (&http.Response{Header: recorder.Header()}).Cookies())
	return Page{StatusCode: recorder.Code, Header: recorder.Header(), Body: recorder.Body.String()}
}

func (b *Browser) Get(target string) (Page, string) {
	b.T.Helper()
	response := b.Do(http.MethodGet, target, nil, nil)
	return response, response.Body
}

func (b *Browser) Login(target, code string) string {
	b.T.Helper()
	response, body := b.Get(target)
	if response.StatusCode != http.StatusSeeOther {
		b.T.Fatalf("GET %s status = %d: %s", target, response.StatusCode, body)
	}
	location := response.Header.Get("Location")
	if strings.HasPrefix(location, "/auth/oidc/start") {
		response, _ = b.Get(location)
		location = response.Header.Get("Location")
	}
	provider, err := url.Parse(location)
	if err != nil || provider.Host != "identity.example.test" {
		b.T.Fatalf("login redirect = %q", location)
	}
	callback, _ := b.Get("/auth/oidc/callback?" + url.Values{"code": {code}, "state": {provider.Query().Get("state")}}.Encode())
	if callback.StatusCode != http.StatusSeeOther {
		b.T.Fatalf("callback status = %d", callback.StatusCode)
	}
	return callback.Header.Get("Location")
}

func CSRF(t *testing.T, body string) string {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		return BootstrapCSRF(t, body)
	}
	_, rest, ok := strings.Cut(body, `name="csrf" value="`)
	if !ok {
		t.Fatalf("page has no CSRF token: %s", body)
	}
	value, _, _ := strings.Cut(rest, `"`)
	return value
}

func BootstrapCSRF(t *testing.T, body string) string {
	t.Helper()
	var bootstrap struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(body), &bootstrap); err != nil {
		t.Fatal(err)
	}
	if bootstrap.CSRF == "" {
		t.Fatal("bootstrap has no CSRF token")
	}
	return bootstrap.CSRF
}
