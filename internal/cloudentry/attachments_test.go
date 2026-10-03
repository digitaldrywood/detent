package cloudentry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type spacesObject struct {
	content []byte
	created time.Time
}
type spacesFixture struct {
	mu              sync.Mutex
	objects         map[string]spacesObject
	keys            []string
	public          bool
	failDelete      bool
	failPutResponse bool
	transport       http.RoundTripper
}

func newSpacesFixture(t *testing.T, public bool) *spacesFixture {
	t.Helper()
	f := &spacesFixture{objects: map[string]spacesObject{}, public: public}
	f.transport = handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/private/")
		if r.Header.Get("Authorization") == "" && !f.public {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/private/") && r.URL.Path != "/private" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
			type item struct {
				Key          string
				LastModified string
				Size         int
			}
			result := struct {
				XMLName   xml.Name `xml:"ListBucketResult"`
				Truncated bool     `xml:"IsTruncated"`
				Items     []item   `xml:"Contents"`
			}{}
			var keys []string
			for key := range f.objects {
				if strings.HasPrefix(key, r.URL.Query().Get("prefix")) {
					keys = append(keys, key)
				}
			}
			sort.Strings(keys)
			for _, key := range keys {
				object := f.objects[key]
				result.Items = append(result.Items, item{Key: key, LastModified: object.created.UTC().Format(time.RFC3339), Size: len(object.content)})
			}
			if err := xml.NewEncoder(w).Encode(result); err != nil {
				t.Error(err)
			}
			return
		}
		f.keys = append(f.keys, r.Method+" "+key)
		switch r.Method {
		case http.MethodPut:
			if r.Header.Get("If-None-Match") != "*" {
				t.Error("upload lacks conditional put")
			}
			if _, exists := f.objects[key]; exists {
				w.WriteHeader(http.StatusPreconditionFailed)
				return
			}
			content, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(500)
				return
			}
			f.objects[key] = spacesObject{content: content, created: time.Now()}
			if f.failPutResponse {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("ETag", `"test"`)
		case http.MethodGet:
			object, ok := f.objects[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if _, err := w.Write(object.content); err != nil {
				t.Error(err)
			}
		case http.MethodHead:
			if _, ok := f.objects[key]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		case http.MethodDelete:
			if f.failDelete {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			delete(f.objects, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})}
	return f
}

func (f *spacesFixture) config() attachment.Config {
	return attachment.Config{Endpoint: "http://127.0.0.1", Region: "test", Bucket: "private", AccessKeyID: "test-key", SecretAccessKey: "test-secret"}
}
func (f *spacesFixture) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.keys...)
}

func TestAttachmentStorageProbe(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, true)
	cfg := f.service.config
	cfg.StateDir = t.TempDir()
	settings := store.config()
	cfg.Attachments = &settings
	cfg.attachmentTransport = store.transport
	if service, err := Open(t.Context(), cfg); err == nil {
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("entry enabled a public bucket")
	}
}

func attachmentProject(t *testing.T, browser *browser, org string) string {
	t.Helper()
	response, body := browser.get("/api/v2/organizations/" + org + "/projects")
	var result []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &result); err != nil || response.StatusCode != http.StatusOK || len(result) == 0 {
		t.Fatalf("projects=%d %s", response.StatusCode, body)
	}
	return result[0].ID
}

func attachmentRequest(t *testing.T, b *browser, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, testPublicURL+target, body)
	if method != http.MethodGet {
		r.Header.Set("Origin", testPublicURL)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	base, _ := url.Parse(testPublicURL)
	for _, cookie := range b.jar.Cookies(base) {
		r.AddCookie(cookie)
	}
	recorder := httptest.NewRecorder()
	b.entry.ServeHTTP(recorder, r)
	return recorder
}

func TestAttachmentRoutesIsolation(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	alice := newBrowser(t, f.service.Handler())
	alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, body := alice.get("/organizations/org_alpha/organization")
	csrf := csrfFrom(t, body)
	project := attachmentProject(t, alice, "org_alpha")
	base := "/organizations/org_alpha/api/v2/projects/" + project + "/attachments"
	content := strings.Repeat("attachment-data\n", 180000)
	upload := attachmentRequest(t, alice, http.MethodPost, base, strings.NewReader(content), map[string]string{"Content-Type": "text/plain", "X-Attachment-Name": "orgs/org_beta/secret.txt", "X-CSRF-Token": csrf, "X-Detent-Organization": "org_beta"})
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload=%d %s", upload.Code, upload.Body.String())
	}
	var record attachment.Metadata
	if err := json.Unmarshal(upload.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.ProjectID != project || record.Name != "secret.txt" || record.Size <= cloudassert.MaxBodyBytes {
		t.Fatalf("metadata=%+v", record)
	}
	beforeMetadata := len(store.requests())
	metadata := attachmentRequest(t, alice, http.MethodGet, base+"/"+record.ID+"/metadata", nil, nil)
	if metadata.Code != http.StatusOK || metadata.Header().Get("Cache-Control") != "private, no-store" || len(store.requests()) != beforeMetadata {
		t.Fatalf("metadata read=%d %s touched storage=%v", metadata.Code, metadata.Body.String(), len(store.requests()) != beforeMetadata)
	}
	var public attachment.Metadata
	if err := json.Unmarshal(metadata.Body.Bytes(), &public); err != nil {
		t.Fatal(err)
	}
	if public.ID != record.ID || public.Name != record.Name || public.AuthorizedPrincipal != "" {
		t.Fatalf("public metadata=%+v", public)
	}
	t.Run("canonical account client routes", func(t *testing.T) {
		canonical := "/api/v2/organizations/org_alpha/projects/" + project + "/attachments"
		upload := attachmentRequest(t, alice, http.MethodPost, canonical, strings.NewReader("canonical attachment"), map[string]string{"Content-Type": "text/plain", "X-Attachment-Name": "canonical.txt", "X-CSRF-Token": csrf})
		if upload.Code != http.StatusCreated {
			t.Fatalf("canonical upload=%d %s", upload.Code, upload.Body.String())
		}
		var created attachment.Metadata
		if err := json.Unmarshal(upload.Body.Bytes(), &created); err != nil || created.ID == "" || created.ProjectID != project {
			t.Fatalf("canonical upload metadata=%+v %v", created, err)
		}
		before := len(store.requests())
		metadataPath := canonical + "/" + created.ID + "/metadata"
		metadata := attachmentRequest(t, alice, http.MethodGet, metadataPath, nil, nil)
		var public attachment.Metadata
		if metadata.Code != http.StatusOK || json.Unmarshal(metadata.Body.Bytes(), &public) != nil || public.ID != created.ID || public.ProjectID != project || public.AuthorizedPrincipal != "" || metadata.Header().Get("Cache-Control") != "private, no-store" || len(store.requests()) != before {
			t.Fatalf("canonical metadata=%d %s touched storage=%v", metadata.Code, metadata.Body.String(), len(store.requests()) != before)
		}
		anonymous := newBrowser(t, f.service.Handler())
		denied := attachmentRequest(t, anonymous, http.MethodGet, metadataPath, nil, nil)
		if denied.Code != http.StatusNotFound || len(store.requests()) != before {
			t.Fatalf("anonymous canonical metadata=%d touched storage=%v", denied.Code, len(store.requests()) != before)
		}
	})
	key, err := attachment.Key("org_alpha", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	read := attachmentRequest(t, alice, http.MethodGet, base+"/"+record.ID, nil, nil)
	if read.Code != http.StatusOK || read.Body.String() != content {
		t.Fatalf("read=%d", read.Code)
	}
	for name, want := range map[string]string{"Content-Security-Policy": "default-src 'none'; sandbox", "X-Content-Type-Options": "nosniff", "Cache-Control": "private, max-age=300", "Cross-Origin-Resource-Policy": "same-origin"} {
		if read.Header().Get(name) != want {
			t.Fatalf("%s=%q", name, read.Header().Get(name))
		}
	}
	if !strings.HasPrefix(read.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatal("download rendered inline")
	}
	anonymous := newBrowser(t, f.service.Handler())
	other := alice.do(http.MethodPost, "/organizations/org_alpha/projects", url.Values{"name": {"Other attachment project"}, "grant_access": {"true"}, "csrf": {csrf}}, nil)
	if other.StatusCode != http.StatusSeeOther {
		t.Fatalf("other project=%d", other.StatusCode)
	}
	otherProject := strings.TrimPrefix(other.Header.Get("Location"), "/organizations/org_alpha/projects/")
	var writeToken string
	var readToken, foreignToken string
	for _, test := range []struct {
		scope, project       string
		wantRead, wantUpload int
	}{
		{"read", project, 200, 404}, {"write", project, 200, 201}, {"write", otherProject, 404, 404},
	} {
		scope := test.scope
		keyRequest, err := json.Marshal(map[string]any{"name": "attachment-key-" + scope + test.project, "scope": scope, "expires_days": 1, "project_ids": []string{test.project}})
		if err != nil {
			t.Fatal(err)
		}
		created := attachmentRequest(t, alice, http.MethodPost, "/api/v2/organizations/org_alpha/api-keys", bytes.NewReader(keyRequest), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
		if created.Code != http.StatusCreated {
			t.Fatalf("key=%d %s", created.Code, created.Body.String())
		}
		var token struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(created.Body.Bytes(), &token); err != nil {
			t.Fatal(err)
		}
		headers := map[string]string{"Authorization": "Bearer " + token.Token, "Content-Type": "text/plain", "X-Attachment-Name": "token.txt"}
		read := attachmentRequest(t, anonymous, http.MethodGet, base+"/"+record.ID, nil, headers)
		if read.Code != test.wantRead {
			t.Fatalf("%s token read=%d %s", scope, read.Code, read.Body.String())
		}
		before := len(store.requests())
		upload := attachmentRequest(t, anonymous, http.MethodPost, base, strings.NewReader("token data"), headers)
		if test.wantUpload == 404 {
			if upload.Code != http.StatusNotFound || len(store.requests()) != before {
				t.Fatalf("read token upload=%d touched storage=%v", upload.Code, len(store.requests()) != before)
			}
		} else if upload.Code != http.StatusCreated {
			t.Fatalf("write token upload=%d %s", upload.Code, upload.Body.String())
		}
		if test.wantUpload == 201 {
			writeToken = token.Token
		}
		if scope == "read" {
			readToken = token.Token
		}
		if test.project == otherProject {
			foreignToken = token.Token
		}
	}

	for _, test := range []struct {
		name, media string
		content     []byte
		status      int
	}{
		{name: "SVG", media: "image/svg+xml", content: []byte("<svg/>"), status: 400},
		{name: "HTML", media: "text/plain", content: []byte("<html>hi</html>"), status: 400},
		{name: "oversized", media: "text/plain", content: bytes.Repeat([]byte("a"), attachment.MaxBytes+1), status: 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(store.requests())
			r := attachmentRequest(t, alice, http.MethodPost, base, bytes.NewReader(test.content), map[string]string{"Content-Type": test.media, "X-Attachment-Name": "file", "X-CSRF-Token": csrf})
			if r.Code != test.status || len(store.requests()) != before {
				t.Fatalf("upload=%d %s touched storage=%v", r.Code, r.Body.String(), len(store.requests()) != before)
			}
		})
	}
	alice.login("/organizations/org_beta/organization", "user_alice:porg_beta")
	betaProject := attachmentProject(t, alice, "org_beta")
	_, betaBody := alice.get("/organizations/org_beta/organization")
	betaCSRF := csrfFrom(t, betaBody)
	_, body = alice.get("/organizations/org_alpha/organization")
	csrf = csrfFrom(t, body)
	for _, test := range []struct {
		name, method, path string
		client             *browser
		headers            map[string]string
		want               int
	}{
		{name: "another org", method: http.MethodGet, path: "/organizations/org_beta/api/v2/projects/" + betaProject + "/attachments/" + record.ID, client: alice, want: 404},
		{name: "another org metadata", method: http.MethodGet, path: "/organizations/org_beta/api/v2/projects/" + betaProject + "/attachments/" + record.ID + "/metadata", client: alice, want: 404},
		{name: "another project", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/prj_other/attachments/" + record.ID, client: alice, want: 404},
		{name: "another project metadata", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/prj_other/attachments/" + record.ID + "/metadata", client: alice, want: 404},
		{name: "another org project", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/" + betaProject + "/attachments/" + record.ID, client: alice, want: 404},
		{name: "another org project metadata", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/" + betaProject + "/attachments/" + record.ID + "/metadata", client: alice, want: 404},
		{name: "anonymous", method: http.MethodGet, path: base + "/" + record.ID, client: anonymous, want: 404},
		{name: "anonymous metadata", method: http.MethodGet, path: base + "/" + record.ID + "/metadata", client: anonymous, want: 404},
		{name: "anonymous write", method: http.MethodPost, path: base, client: anonymous, want: 404},
		{name: "cross org write", method: http.MethodPost, path: base, client: alice, headers: map[string]string{"X-CSRF-Token": betaCSRF}, want: 404},
		{name: "another project write", method: http.MethodPost, path: strings.Replace(base, project, betaProject, 1), client: alice, headers: map[string]string{"X-CSRF-Token": csrf}, want: 404},
		{name: "metadata injection", method: http.MethodPost, path: "/api/v2/organizations/org_alpha/projects/" + project + "/attachment-metadata", client: alice, want: 404},
		{name: "private metadata read", method: http.MethodGet, path: "/api/v2/organizations/org_alpha/projects/" + project + "/attachment-metadata/" + record.ID, client: alice, want: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(store.requests())
			r := attachmentRequest(t, test.client, test.method, test.path, strings.NewReader("hi"), test.headers)
			if r.Code != test.want {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			if len(store.requests()) != before {
				t.Fatal("unauthorized request touched storage")
			}
		})
	}
	for _, request := range store.requests() {
		if strings.Contains(request, "orgs/org_beta/") {
			t.Fatalf("org alpha request touched org beta: %s", request)
		}
	}
	store.mu.Lock()
	if _, ok := store.objects[key]; !ok {
		t.Error("authenticated org key missing")
	}
	store.mu.Unlock()
	var audits int
	if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM audit WHERE event IN ('attachment_uploaded','attachment_read') AND organization_id='org_alpha'").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	// Six existing operations plus the canonical upload and metadata read.
	if audits != 8 {
		t.Fatalf("audit entries=%d", audits)
	}
	exerciseAttachmentClients(t, f, alice, anonymous, project, writeToken)
	exerciseAttachmentMCPOperations(t, f, store, alice, anonymous, project, otherProject, writeToken, readToken, foreignToken, record, content)
	deleted := attachmentRequest(t, alice, http.MethodDelete, base+"/"+record.ID, nil, map[string]string{"X-CSRF-Token": csrf})
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("delete=%d %s", deleted.Code, deleted.Body.String())
	}
	if read := attachmentRequest(t, alice, http.MethodGet, base+"/"+record.ID, nil, nil); read.Code != 404 {
		t.Fatalf("deleted read=%d", read.Code)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.objects[key]; ok {
		t.Fatal("deleted object remains")
	}
}

func TestAttachmentOrphanSweepAndDeprovision(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	old := time.Now().Add(-attachment.OrphanTTL - time.Hour)
	ids := []string{"att_00000000000000000000000000000001", "att_00000000000000000000000000000002", "att_00000000000000000000000000000003"}
	keys := []string{}
	for i, org := range []string{"org_alpha", "org_alpha", "org_beta"} {
		key, err := attachment.Key(org, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		stamp := old
		if i == 1 {
			stamp = time.Now()
		}
		store.mu.Lock()
		store.objects[key] = spacesObject{content: []byte("orphan"), created: stamp}
		store.mu.Unlock()
		keys = append(keys, key)
	}
	organization, err := f.service.readyOrganization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	_, oldExists := store.objects[keys[0]]
	_, newExists := store.objects[keys[1]]
	_, betaExists := store.objects[keys[2]]
	store.mu.Unlock()
	if oldExists || !newExists || !betaExists {
		t.Fatalf("old=%v new=%v other org=%v", oldExists, newExists, betaExists)
	}
	f.service.config.Allocation = &AllocationConfig{Launcher: &silentLauncher{running: map[string]bool{}}}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='deleting' WHERE id='org_alpha'"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.finishDeletion(t.Context(), "org_alpha"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, ok := store.objects[keys[1]]; ok {
		t.Fatal("deprovisioning retained tenant objects")
	}
	if _, ok := store.objects[keys[2]]; !ok {
		t.Fatal("deprovisioning deleted another tenant")
	}
}

func TestAttachmentRoutesDisabled(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	r := attachmentRequest(t, newBrowser(t, f.service.Handler()), http.MethodPost, "/organizations/org_alpha/api/v2/projects/prj_any/attachments", bytes.NewReader(nil), nil)
	if r.Code != 503 || !strings.Contains(r.Body.String(), "attachments_not_configured") {
		t.Fatalf("response=%d %s", r.Code, r.Body.String())
	}
}

type attachmentTenantTransport struct {
	base      http.RoundTripper
	intercept func(*http.Request) (*http.Response, error)
}

func (t attachmentTenantTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/attachment-metadata") {
		return t.intercept(request)
	}
	return t.base.RoundTrip(request)
}

func TestAttachmentMetadataSettlement(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		quota       bool
		keyed       bool
		concurrent  bool
		storageLoss bool
		status      int
	}{{name: "lost committed response", status: http.StatusCreated}, {name: "quota race", quota: true, status: http.StatusTooManyRequests}, {name: "keyed lost committed response", keyed: true, status: http.StatusCreated}, {name: "concurrent keyed uploads", keyed: true, concurrent: true, status: http.StatusCreated}, {name: "stored object response loss", keyed: true, storageLoss: true, status: http.StatusCreated}} {
		t.Run(test.name, func(t *testing.T) {
			f := newEntryFixture(t)
			store := newSpacesFixture(t, false)
			storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
			if err != nil {
				t.Fatal(err)
			}
			store.failPutResponse = test.storageLoss
			f.service.attachments = storage
			baseTransport := f.service.config.transport
			f.service.config.transport = func(org Organization) (http.RoundTripper, error) {
				base, err := baseTransport(org)
				if err != nil {
					return nil, err
				}
				return attachmentTenantTransport{base: base, intercept: func(request *http.Request) (*http.Response, error) {
					if test.quota {
						recorder := httptest.NewRecorder()
						recorder.Header().Set("Content-Type", "application/json")
						recorder.WriteHeader(429)
						if _, err := recorder.WriteString(`{"code":"allowance_exhausted","message":"quota race"}`); err != nil {
							t.Fatal(err)
						}
						return recorder.Result(), nil
					}
					response, err := base.RoundTrip(request)
					if err != nil {
						return nil, err
					}
					if err := response.Body.Close(); err != nil {
						return nil, err
					}
					return nil, errors.New("response lost after metadata commit")
				}}, nil
			}
			alice := newBrowser(t, f.service.Handler())
			alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			_, body := alice.get("/organizations/org_alpha/organization")
			csrf := csrfFrom(t, body)
			project := attachmentProject(t, alice, "org_alpha")
			path := "/organizations/org_alpha/api/v2/projects/" + project + "/attachments"
			headers := map[string]string{"Content-Type": "text/plain", "X-Attachment-Name": "file.txt", "X-CSRF-Token": csrf}
			if test.keyed {
				headers["Idempotency-Key"] = "settlement"
			}
			var concurrent <-chan *httptest.ResponseRecorder
			if test.concurrent {
				responses := make(chan *httptest.ResponseRecorder, 1)
				concurrent = responses
				go func() {
					responses <- attachmentRequest(t, alice, http.MethodPost, path, strings.NewReader("settlement data"), headers)
				}()
			}
			response := attachmentRequest(t, alice, http.MethodPost, path, strings.NewReader("settlement data"), headers)
			if concurrent != nil {
				other := <-concurrent
				if other.Code != http.StatusCreated || other.Body.String() != response.Body.String() {
					t.Fatalf("concurrent upload=%d %s original=%s", other.Code, other.Body.String(), response.Body.String())
				}
			}
			if test.keyed {
				replay := attachmentRequest(t, alice, http.MethodPost, path, strings.NewReader("settlement data"), headers)
				if replay.Code != http.StatusCreated || replay.Body.String() != response.Body.String() {
					t.Fatalf("replay=%d %s original=%s", replay.Code, replay.Body.String(), response.Body.String())
				}
			}
			if response.Code != test.status {
				t.Fatalf("settlement=%d %s", response.Code, response.Body.String())
			}
			store.mu.Lock()
			count := len(store.objects)
			store.mu.Unlock()
			if test.quota {
				if count != 0 {
					t.Fatal("quota race retained uploaded object")
				}
				return
			}
			if count != 1 {
				t.Fatal("response loss deleted committed attachment")
			}
			var record attachment.Metadata
			if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil {
				t.Fatal(err)
			}
			read := attachmentRequest(t, alice, http.MethodGet, path+"/"+record.ID, nil, nil)
			if read.Code != 200 || read.Body.String() != "settlement data" {
				t.Fatalf("settled read=%d %s", read.Code, read.Body.String())
			}
		})
	}
}

func exerciseAttachmentClients(t *testing.T, f entryFixture, browser, anonymous *browser, project, token string) {
	t.Helper()
	var pixels bytes.Buffer
	if err := png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	client, err := hubclient.New(hubclient.Config{URL: testPublicURL + "/organizations/org_alpha", TokenSource: func() string { return token }, HTTPClient: &http.Client{Transport: handlerTransport{handler: f.service.Handler()}}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_alpha", tracker.ProjectID(project))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"API", "MCP", "worker"} {
		t.Run(mode, func(t *testing.T) {
			issue, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "attachment-issue-" + mode}, Title: "Attachment " + mode, Body: "PNG evidence", State: "Todo"})
			if err != nil {
				t.Fatal(err)
			}
			var uploaded attachment.Metadata
			switch mode {
			case "worker":
				err = native.PublishValidationEvidence(t.Context(), issue.WorkItemID, tracker.Mutation{IdempotencyKey: "worker-evidence"}, []runner.ValidationEvidence{{Name: "test.png", ContentType: "image/png", Content: pixels.Bytes()}})
			case "API":
				uploaded, err = native.UploadAttachment(t.Context(), bytes.NewReader(pixels.Bytes()), "test.png", "image/png")
			default:
				path := "/organizations/org_alpha/mcp"
				headers := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token, "Mcp-Protocol-Version": "2025-11-25"}
				initialized := attachmentRequest(t, anonymous, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"attachment-test","version":"1"}}}`), headers)
				if initialized.Code != 200 {
					t.Fatalf("initialize=%d %s", initialized.Code, initialized.Body.String())
				}
				headers["Mcp-Session-Id"] = initialized.Header().Get("Mcp-Session-Id")
				attachmentRequest(t, anonymous, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), headers)
				arguments := map[string]string{"request_id": "upload-" + mode, "project_id": project, "name": "test.png", "content_type": "image/png", "content_base64": base64.StdEncoding.EncodeToString(pixels.Bytes())}
				call, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "upload_attachment", "arguments": arguments}})
				if err != nil {
					t.Fatal(err)
				}
				response := attachmentRequest(t, anonymous, http.MethodPost, path, bytes.NewReader(call), headers)
				var result struct {
					Result struct {
						IsError    bool                `json:"isError"`
						Attachment attachment.Metadata `json:"structuredContent"`
					} `json:"result"`
				}
				if json.Unmarshal(response.Body.Bytes(), &result) != nil || response.Code != 200 || result.Result.IsError {
					t.Fatalf("MCP upload=%d %s", response.Code, response.Body.String())
				}
				uploaded = result.Result.Attachment
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode != "worker" {
				if uploaded.Reference != uploaded.Markdown("org_alpha") || uploaded.Validate() != nil {
					t.Fatalf("metadata=%+v", uploaded)
				}
				_, err = native.CreateComment(t.Context(), issue.WorkItemID, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "attachment-comment-" + mode}, Body: uploaded.Reference})
				if err != nil {
					t.Fatal(err)
				}
			}
			comments, err := native.Comments(t.Context(), issue.WorkItemID, "")
			if err != nil || len(comments.Items) != 1 {
				t.Fatalf("comments=%+v %v", comments, err)
			}
			refs := attachment.References(comments.Items[0].Body, "org_alpha", project)
			var ref attachment.Reference
			for _, part := range refs {
				if part.ID != "" {
					ref = part
				}
			}
			if ref.ID == "" || !ref.Image {
				t.Fatalf("no inline image: %s", comments.Items[0].Body)
			}
			read := attachmentRequest(t, browser, http.MethodGet, ref.URL, nil, nil)
			if read.Code != 200 {
				t.Fatalf("PNG read=%d %s", read.Code, read.Body.String())
			}
			if _, err := png.Decode(bytes.NewReader(read.Body.Bytes())); err != nil {
				t.Fatal(err)
			}
			binding, err := json.Marshal(map[string]string{"work_item_id": string(issue.WorkItemID)})
			if err != nil {
				t.Fatal(err)
			}
			rebind := attachmentRequest(t, anonymous, http.MethodPost, ref.URL+"/reference", bytes.NewReader(binding), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token})
			if rebind.Code != http.StatusNoContent {
				t.Fatalf("additional attachment reference=%d %s", rebind.Code, rebind.Body.String())
			}
			page, body := browser.get("/organizations/org_alpha/work/i/" + string(issue.WorkItemID))
			if page.StatusCode != 200 || !strings.Contains(body, `id="root"`) {
				t.Fatalf("issue page=%d", page.StatusCode)
			}
		})
	}
}
