package cloudentry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type sizedTransport struct{ size int }

func (t sizedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("x"), t.size)))}, nil
}

func TestMachineCallRefusesOversizedTenantAnswers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		path        string
		requestBody string
		size        int
		wantErr     bool
	}{
		{name: "at the limit", size: tenantCallResponseLimit},
		{name: "one byte over", size: tenantCallResponseLimit + 1, wantErr: true},
		{name: "export above ordinary transport limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_bulk_export"}`, size: mcp.MaxHTTPResponseBytes + 1},
		{name: "export at limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_bulk_export"}`, size: operatortool.WorkBulkExportMaxBytes},
		{name: "export over limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_bulk_export"}`, size: operatortool.WorkBulkExportMaxBytes + 1, wantErr: true},
		{name: "ordinary tool retains limit", path: "/internal/v1/keys/call", requestBody: `{"name":"work_list"}`, size: mcp.MaxHTTPResponseBytes + 1, wantErr: true},
		{name: "catalog retains limit", path: "/internal/v1/keys/catalog", size: mcp.MaxHTTPResponseBytes + 1, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			seed := make([]byte, ed25519.SeedSize)
			service, err := Open(t.Context(), Config{Platform: PlatformConfig{BootstrapAdminEmail: "bootstrap@example.test"}, PublicURL: testPublicURL, ListenAddress: "127.0.0.1:0", Issuer: "entry", SigningKey: ed25519.NewKeyFromSeed(seed), Provider: newFakeProvider(),
				StateDir: t.TempDir(), Logger: slog.New(slog.DiscardHandler), clientFS: fstest.MapFS{},
				transport: func(Organization) (http.RoundTripper, error) { return sizedTransport{size: test.size}, nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			organization := Organization{ID: "org_limit", ProviderID: "porg_limit", Name: "Limit", Endpoint: testSocketEndpoint("org_limit.sock"), Generation: 1}
			path := test.path
			if path == "" {
				path = "/api/v2/organizations/org_limit/entitlements"
			}
			status, body, err := service.machineCall(t.Context(), organization, http.MethodPost, path, []byte(test.requestBody), "token")
			if test.wantErr {
				if err == nil || !strings.Contains(err.Error(), "exceeds") {
					t.Fatalf("machineCall() = %d, %d bytes, %v; want a size error", status, len(body), err)
				}
				return
			}
			if err != nil || status != http.StatusOK || len(body) != test.size {
				t.Fatalf("machineCall() = %d, %d bytes, %v", status, len(body), err)
			}
		})
	}
}

func TestSharedEntryChangeSourcePublication(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, page := owner.get("/organizations/org_alpha/organization")
	csrf := csrfFrom(t, page)
	project := attachmentProject(t, owner, "org_alpha")
	base := "/api/v2/organizations/org_alpha/projects/" + project
	key := createTestAccessKey(t, owner, "project", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{project}}}}, "")
	client := newBrowser(t, f.service.Handler())
	request := func(t *testing.T, method, path string, value any, token string) *httptest.ResponseRecorder {
		t.Helper()
		var raw []byte
		var err error
		if value != nil {
			raw, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
		}
		return attachmentRequest(t, client, method, path, bytes.NewReader(raw), map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token})
	}
	require := func(t *testing.T, response *httptest.ResponseRecorder, status int) {
		t.Helper()
		if response.Code != status {
			t.Fatalf("status=%d want=%d: %.500s", response.Code, status, response.Body.String())
		}
	}
	descriptor := pilotPolicy()
	raw, err := json.Marshal(policy.Change{Policy: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	require(t, attachmentRequest(t, owner, http.MethodPut, base+"/onboarding/policy", bytes.NewReader(raw), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf}), http.StatusOK)
	raw, err = json.Marshal(tracker.ApproveChangeReviewPolicy{Mutation: tracker.Mutation{IdempotencyKey: "source-rules"}, Policy: tracker.ChangeReviewPolicy{PolicyID: descriptor.ID, RequiredChecks: []tracker.ChangeCheckSpec{}}})
	if err != nil {
		t.Fatal(err)
	}
	require(t, attachmentRequest(t, owner, http.MethodPut, base+"/change-review-policy", bytes.NewReader(raw), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf}), http.StatusOK)
	response := request(t, http.MethodPost, base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "source-item"}, Title: "Synthetic source publication", State: "Backlog"}, key.Token)
	require(t, response, http.StatusOK)
	var item tracker.NativeIssue
	if err := json.Unmarshal(response.Body.Bytes(), &item); err != nil {
		t.Fatal(err)
	}
	path := base + "/work-items/" + string(item.WorkItemID) + "/changes"
	response = request(t, http.MethodPost, path, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "source-change"}, Title: "Synthetic source"}, key.Token)
	require(t, response, http.StatusOK)
	var change tracker.ChangeRequest
	if err := json.Unmarshal(response.Body.Bytes(), &change); err != nil {
		t.Fatal(err)
	}
	path += "/" + change.ID + "/versions"
	directory := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-b", "main")
	sourcePath := filepath.Join(directory, "source.bin")
	if err := os.WriteFile(sourcePath, []byte("base source"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.bin")
	git("commit", "-m", "base")
	baseSHA := git("rev-parse", "HEAD")
	content := make([]byte, 3<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.bin")
	git("commit", "-m", "synthetic source")
	headSHA := git("rev-parse", "HEAD")
	capture, err := workspace.CaptureChangeSource(t.Context(), directory, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(capture.Bundle) <= cloudassert.MaxBodyBytes {
		t.Fatal("fixture does not cross shared entry body boundary")
	}
	publication := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "large-source"}, SourceBundle: capture.Bundle, ChangeVersionInput: tracker.ChangeVersionInput{Source: &capture.Source, BaseSHA: baseSHA, HeadSHA: headSHA, MergeBaseSHA: baseSHA, Repository: "https://github.com/example-org/example-project", PolicyID: descriptor.ID, Code: tracker.ChangeArtifact{Kind: "code", URI: "https://github.com/example-org/example-project/commit/" + headSHA, SHA256: policy.Digest([]byte(headSHA)), Availability: "unverified"}}}
	response = request(t, http.MethodPost, path, publication, key.Token)
	require(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	if err := json.Unmarshal(response.Body.Bytes(), &version); err != nil {
		t.Fatal(err)
	}
	if version.Source == nil || *version.Source != capture.Source || version.BaseSHA != baseSHA || version.HeadSHA != headSHA {
		t.Fatal("publication changed source identity")
	}
	replay := request(t, http.MethodPost, path, publication, key.Token)
	require(t, replay, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("replay changed immutable receipt")
	}
	alias := "/organizations/org_alpha" + path
	replay = request(t, http.MethodPost, alias, publication, key.Token)
	require(t, replay, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("route alias changed immutable receipt")
	}
	sourceURL := path + "/" + version.ID + "/source"
	read := request(t, http.MethodGet, sourceURL, nil, key.Token)
	require(t, read, http.StatusOK)
	if !bytes.Equal(read.Body.Bytes(), capture.Bundle) {
		t.Fatal("authorized source read changed bundle")
	}
	for _, test := range []struct {
		name   string
		edit   func(*tracker.PublishChangeVersion)
		status int
	}{
		{"stale version", func(p *tracker.PublishChangeVersion) { p.ExpectedVersionID = "version_stale" }, http.StatusConflict},
		{"hash mismatch", func(p *tracker.PublishChangeVersion) {
			source := *p.Source
			source.BundleSHA256 = strings.Repeat("f", 64)
			p.Source = &source
		}, http.StatusUnprocessableEntity},
		{"source maximum", func(p *tracker.PublishChangeVersion) {
			p.SourceBundle = bytes.Repeat([]byte("x"), tracker.MaxChangeSourceBytes+1)
			source := *p.Source
			source.Bytes = int64(len(p.SourceBundle))
			source.BundleSHA256 = tracker.ChangeSourceDigest(p.SourceBundle)
			p.Source = &source
		}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := publication
			p.IdempotencyKey = test.name
			p.ExpectedVersionID = version.ID
			test.edit(&p)
			require(t, request(t, http.MethodPost, path, p, key.Token), test.status)
		})
	}
	malformed, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	malformedFields := make(map[string]any)
	if err := json.Unmarshal(malformed, &malformedFields); err != nil {
		t.Fatal(err)
	}
	malformedFields["idempotency_key"] = "malformed-bundle"
	malformedFields["expected_version_id"] = version.ID
	malformedFields["source_bundle"] = strings.Repeat("!", 3<<20)
	require(t, request(t, http.MethodPost, path, malformedFields, key.Token), http.StatusUnprocessableEntity)
	require(t, request(t, http.MethodPost, path, publication, "invalid-key"), http.StatusUnauthorized)
	foreign := strings.Replace(path, project, "prj_foreign", 1)
	require(t, request(t, http.MethodPost, foreign, publication, key.Token), http.StatusForbidden)
	require(t, request(t, http.MethodGet, strings.Replace(sourceURL, project, "prj_foreign", 1), nil, key.Token), http.StatusForbidden)
	require(t, request(t, http.MethodPost, base+"/work-items", map[string]string{"title": strings.Repeat("x", cloudassert.MaxBodyBytes+1)}, key.Token), http.StatusRequestEntityTooLarge)
	bounded, err := json.Marshal(publication)
	if err != nil {
		t.Fatal(err)
	}
	bounded = append(bounded, bytes.Repeat([]byte(" "), (28<<20)-len(bounded))...)
	headers := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + key.Token}
	atLimit := attachmentRequest(t, client, http.MethodPost, path, bytes.NewReader(bounded), headers)
	require(t, atLimit, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), atLimit.Body.Bytes()) {
		t.Fatal("publication at JSON limit changed immutable receipt")
	}
	require(t, attachmentRequest(t, client, http.MethodPost, path, bytes.NewReader(append(bounded, ' ')), headers), http.StatusRequestEntityTooLarge)
	transport := f.service.config.transport
	f.service.config.transport = func(organization Organization) (http.RoundTripper, error) {
		return handlerTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(append(body, ' ')))
			f.tenants[organization.Endpoint].ServeHTTP(w, r)
		})}, nil
	}
	require(t, request(t, http.MethodPost, path, publication, key.Token), http.StatusUnauthorized)
	f.service.config.transport = transport

}
