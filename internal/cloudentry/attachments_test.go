package cloudentry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentStorageProbe(t *testing.T) {

	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, true)
	cfg := f.service.config
	cfg.StateDir = t.TempDir()
	settings := store.Config()
	cfg.Attachments = &settings
	cfg.AttachmentTransport = store.Transport
	if service, err := Open(t.Context(), cfg); err == nil {
		if err := service.Close(); err != nil {
			t.Fatal(err)
		}
		t.Fatal("entry enabled a public bucket")
	}
}

func TestAttachmentArchiveConfigurationRefusalKeepsEntryRunning(t *testing.T) {

	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	store.DenyArchive = true
	cfg := f.service.config
	cfg.StateDir = t.TempDir()
	settings := store.Config()
	cfg.Attachments = &settings
	cfg.AttachmentTransport = store.Transport
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatalf("entry refused to start when archive configuration was denied: %v", err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAttachmentRoutesIsolation(t *testing.T) {

	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.Config(), store.Transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	alice := newBrowser(t, f.service.Handler())
	alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, body := alice.Get("/organizations/org_alpha/app/bootstrap")
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
	beforeMetadata := len(store.Requests())
	metadata := attachmentRequest(t, alice, http.MethodGet, base+"/"+record.ID+"/metadata", nil, nil)
	if metadata.Code != http.StatusOK || metadata.Header().Get("Cache-Control") != "private, no-store" || len(store.Requests()) != beforeMetadata {
		t.Fatalf("metadata read=%d %s touched storage=%v", metadata.Code, metadata.Body.String(), len(store.Requests()) != beforeMetadata)
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
		before := len(store.Requests())
		metadataPath := canonical + "/" + created.ID + "/metadata"
		metadata := attachmentRequest(t, alice, http.MethodGet, metadataPath, nil, nil)
		var public attachment.Metadata
		if metadata.Code != http.StatusOK || json.Unmarshal(metadata.Body.Bytes(), &public) != nil || public.ID != created.ID || public.ProjectID != project || public.AuthorizedPrincipal != "" || metadata.Header().Get("Cache-Control") != "private, no-store" || len(store.Requests()) != before {
			t.Fatalf("canonical metadata=%d %s touched storage=%v", metadata.Code, metadata.Body.String(), len(store.Requests()) != before)
		}
		anonymous := newBrowser(t, f.service.Handler())
		denied := attachmentRequest(t, anonymous, http.MethodGet, metadataPath, nil, nil)
		if denied.Code != http.StatusNotFound || len(store.Requests()) != before {
			t.Fatalf("anonymous canonical metadata=%d touched storage=%v", denied.Code, len(store.Requests()) != before)
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
	otherProject := createEntryTestProject(t, alice, "Other attachment project", csrf)
	var writeToken string
	var readToken, foreignToken string
	for _, test := range []struct {
		scope, project       string
		wantRead, wantUpload int
	}{
		{"read", project, 200, 404}, {"write", project, 200, 201}, {"write", otherProject, 404, 404},
	} {
		scope := test.scope
		token := createTestAccessKey(t, alice, "project", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{test.project}}}}, "", scope)
		headers := map[string]string{"Authorization": "Bearer " + token.Token, "Content-Type": "text/plain", "X-Attachment-Name": "token.txt"}
		read := attachmentRequest(t, anonymous, http.MethodGet, base+"/"+record.ID, nil, headers)
		if read.Code != test.wantRead {
			t.Fatalf("%s token read=%d %s", scope, read.Code, read.Body.String())
		}
		before := len(store.Requests())
		upload := attachmentRequest(t, anonymous, http.MethodPost, base, strings.NewReader("token data"), headers)
		if test.wantUpload != http.StatusCreated {
			if upload.Code != test.wantUpload || len(store.Requests()) != before {
				t.Fatalf("read token upload=%d touched storage=%v", upload.Code, len(store.Requests()) != before)
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
			before := len(store.Requests())
			r := attachmentRequest(t, alice, http.MethodPost, base, bytes.NewReader(test.content), map[string]string{"Content-Type": test.media, "X-Attachment-Name": "file", "X-CSRF-Token": csrf})
			if r.Code != test.status || len(store.Requests()) != before {
				t.Fatalf("upload=%d %s touched storage=%v", r.Code, r.Body.String(), len(store.Requests()) != before)
			}
		})
	}
	alice.Login("/organizations/org_beta/organization", "user_alice:porg_beta")
	betaProject := attachmentProject(t, alice, "org_beta")
	_, betaBody := alice.Get("/organizations/org_beta/app/bootstrap")
	betaCSRF := csrfFrom(t, betaBody)
	_, body = alice.Get("/organizations/org_alpha/app/bootstrap")
	csrf = csrfFrom(t, body)
	for _, test := range []struct {
		name, method, path string
		client             *browser
		headers            map[string]string
		want               int
	}{
		{name: "another org", method: http.MethodGet, path: "/organizations/org_beta/api/v2/projects/" + betaProject + "/attachments/" + record.ID, client: alice, want: 404},
		{name: "another org metadata", method: http.MethodGet, path: "/organizations/org_beta/api/v2/projects/" + betaProject + "/attachments/" + record.ID + "/metadata", client: alice, want: 404},
		{name: "another org bounded content", method: http.MethodGet, path: "/organizations/org_beta/api/v2/projects/" + betaProject + "/attachments/" + record.ID + "?length=1", client: alice, want: 404},
		{name: "another project", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/prj_other/attachments/" + record.ID, client: alice, want: 404},
		{name: "another project metadata", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/prj_other/attachments/" + record.ID + "/metadata", client: alice, want: 404},
		{name: "ungranted bounded content", method: http.MethodGet, path: base + "/" + record.ID + "?length=1", client: anonymous, headers: map[string]string{"Authorization": "Bearer " + foreignToken}, want: 404},
		{name: "another org project", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/" + betaProject + "/attachments/" + record.ID, client: alice, want: 404},
		{name: "another org project metadata", method: http.MethodGet, path: "/organizations/org_alpha/api/v2/projects/" + betaProject + "/attachments/" + record.ID + "/metadata", client: alice, want: 404},
		{name: "anonymous", method: http.MethodGet, path: base + "/" + record.ID, client: anonymous, want: 404},
		{name: "anonymous metadata", method: http.MethodGet, path: base + "/" + record.ID + "/metadata", client: anonymous, want: 404},
		{name: "anonymous bounded content", method: http.MethodGet, path: base + "/" + record.ID + "?length=1", client: anonymous, want: 404},
		{name: "anonymous write", method: http.MethodPost, path: base, client: anonymous, want: 404},
		{name: "evidence without attempt", method: http.MethodPost, path: base, client: alice, headers: map[string]string{"X-CSRF-Token": csrf, "X-Detent-Evidence": "unexpected"}, want: 400},
		{name: "missing attempt evidence", method: http.MethodPost, path: strings.TrimSuffix(base, "/attachments") + "/attempts/attempt_example/evidence", client: alice, headers: map[string]string{"X-CSRF-Token": csrf}, want: 400},
		{name: "malformed attempt evidence", method: http.MethodPost, path: strings.TrimSuffix(base, "/attachments") + "/attempts/attempt_example/evidence", client: alice, headers: map[string]string{"X-CSRF-Token": csrf, "X-Detent-Evidence": "!"}, want: 400},
		{name: "cross org write", method: http.MethodPost, path: base, client: alice, headers: map[string]string{"X-CSRF-Token": betaCSRF}, want: 404},
		{name: "another project write", method: http.MethodPost, path: strings.Replace(base, project, betaProject, 1), client: alice, headers: map[string]string{"X-CSRF-Token": csrf}, want: 404},
		{name: "metadata injection", method: http.MethodPost, path: "/api/v2/organizations/org_alpha/projects/" + project + "/attachment-metadata", client: alice, want: 404},
		{name: "private metadata read", method: http.MethodGet, path: "/api/v2/organizations/org_alpha/projects/" + project + "/attachment-metadata/" + record.ID, client: alice, want: 404},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := len(store.Requests())
			r := attachmentRequest(t, test.client, test.method, test.path, strings.NewReader("hi"), test.headers)
			if r.Code != test.want {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			if len(store.Requests()) != before {
				t.Fatal("unauthorized request touched storage")
			}
		})
	}
	for _, request := range store.Requests() {
		if strings.Contains(request, "orgs/org_beta/") {
			t.Fatalf("org alpha request touched org beta: %s", request)
		}
	}
	store.Mu.Lock()
	if _, ok := store.Objects[key]; !ok {
		t.Error("authenticated org key missing")
	}
	store.Mu.Unlock()
	var audits int
	if err := f.service.auth.store.db.QueryRowContext(t.Context(), "SELECT count(*) FROM platform.audit WHERE event IN ('attachment_uploaded','attachment_read') AND organization_id='org_alpha'").Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 3 {
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
	store.Mu.Lock()
	defer store.Mu.Unlock()
	if _, ok := store.Objects[key]; ok {
		t.Fatal("deleted object remains")
	}
}

func TestAttachmentOrphanSweepAndDeprovision(t *testing.T) {

	t.Parallel()
	f := newEntryFixture(t)
	if err := f.service.closeAllocator(); err != nil {
		t.Fatal(err)
	}
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.Config(), store.Transport)
	if err != nil {
		t.Fatal(err)
	}
	walks := &lockProbeStorage{Storage: storage, locks: &f.service.attachmentLocks, organization: "org_alpha", refresh: map[string]func(){}}
	f.service.attachments = walks
	old := time.Now().Add(-attachment.OrphanTTL - time.Hour)
	ids := []string{"att_00000000000000000000000000000001", "att_00000000000000000000000000000002", "att_00000000000000000000000000000003"}
	keys := []string{}
	for i, org := range []string{"org_alpha", "org_alpha", "org_beta"} {
		key, err := attachment.Key(org, ids[i])
		if err != nil {
			t.Fatal(err)
		}
		stamp := old
		if i != 0 {
			stamp = time.Now()
		}
		store.Mu.Lock()
		store.Objects[key] = spacesObject{Content: []byte("orphan"), Created: stamp}
		store.Mu.Unlock()
		keys = append(keys, key)
	}
	reused, err := attachment.Key("org_alpha", "att_00000000000000000000000000000004")
	if err != nil {
		t.Fatal(err)
	}
	store.Mu.Lock()
	store.Objects[reused] = spacesObject{Content: []byte("reused"), Created: old}
	store.Mu.Unlock()
	walks.refresh[reused] = func() {
		store.Mu.Lock()
		store.Objects[reused] = spacesObject{Content: []byte("reused"), Created: time.Now()}
		store.Mu.Unlock()
	}
	organization, err := f.service.readyOrganization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
		t.Fatal(err)
	}
	store.Mu.Lock()
	_, reusedExists := store.Objects[reused]
	store.Mu.Unlock()
	if !reusedExists {
		t.Fatal("sweep deleted an object refreshed after it was listed")
	}
	store.Mu.Lock()
	_, oldExists := store.Objects[keys[0]]
	_, newExists := store.Objects[keys[1]]
	_, betaExists := store.Objects[keys[2]]
	store.Mu.Unlock()
	if oldExists || !newExists || !betaExists {
		t.Fatalf("old=%v new=%v other org=%v", oldExists, newExists, betaExists)
	}
	if walks.visits == 0 || walks.blocked != 0 {
		t.Fatalf("organization readers blocked during %d of %d sweep visits", walks.blocked, walks.visits)
	}
	t.Run("bounded pages", func(t *testing.T) {
		store.Mu.Lock()
		for i := range 40 {
			store.Objects[fmt.Sprintf("orgs/org_alpha/aux/%04d", i)] = spacesObject{Created: old}
		}
		store.Mu.Unlock()
		if _, err := f.service.registry.store.db.ExecContext(t.Context(), "INSERT INTO platform.organizations(id,name,state,endpoint,generation,created_at,updated_at) SELECT 'org_0'||lpad(i::text,2,'0'),'Example','disabled','',1,now(),now() FROM generate_series(1,32) i"); err != nil {
			t.Fatal(err)
		}
		f.service.attachmentAfter = ""
		f.service.attachmentObjectCursor = ""
		f.service.attachmentSweepAt = time.Time{}
		f.service.attachmentObjectSweepAt = time.Time{}
		f.service.sweepAttachments(t.Context())
		if f.service.attachmentAfter != "org_032" || f.service.attachmentObjectCursor == "" {
			t.Fatal("first tick did not stop after one tenant page and one object page")
		}
		f.service.sweepAttachments(t.Context())
		if f.service.attachmentAfter != "" || !f.service.attachmentSweepAt.After(f.service.config.Now()) || f.service.attachmentObjectCursor != "" || !f.service.attachmentObjectSweepAt.After(f.service.config.Now()) {
			t.Fatal("second tick did not finish both scans")
		}
	})
	f.service.attachments = storage
	f.service.config.Allocation = &AllocationConfig{Launcher: &silentLauncher{running: map[string]bool{}}}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE platform.organizations SET state='deleting' WHERE id='org_alpha'"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.finishDeletion(t.Context(), "org_alpha"); err != nil {
		t.Fatal(err)
	}
	store.Mu.Lock()
	defer store.Mu.Unlock()
	if _, ok := store.Objects[keys[1]]; ok {
		t.Fatal("deprovisioning retained tenant objects")
	}
	if _, ok := store.Objects[keys[2]]; !ok {
		t.Fatal("deprovisioning deleted another tenant")
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
			issue, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "attachment-issue-" + mode}, Title: map[string]string{"API": "Screenshot rendering", "MCP": "Protocol transfer", "worker": "Worker publication"}[mode], State: "Backlog"})
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
		})
	}
}

type lockProbeStorage struct {
	attachment.Storage
	locks        *attachmentLocks
	organization string
	refresh      map[string]func()
	visits       int
	blocked      int
}

func (s *lockProbeStorage) Walk(ctx context.Context, prefix string, visit func(attachment.Object) error) error {
	return s.Storage.Walk(ctx, prefix, func(object attachment.Object) error {
		s.visits++
		s.locks.mu.Lock()
		entry := s.locks.entries[s.organization]
		s.locks.mu.Unlock()
		if entry != nil {
			if entry.TryRLock() {
				entry.RUnlock()
			} else {
				s.blocked++
			}
		}
		if refresh := s.refresh[object.Key]; refresh != nil {
			refresh()
		}
		return visit(object)
	})
}

func (s *Service) sweepOrganizationAttachments(ctx context.Context, organization Organization) error {
	if _, err := s.attachmentHandlers().SweepExpired(ctx, organization, 100); err != nil {
		return err
	}
	prefix, err := attachment.OrganizationPrefix(organization.ID)
	if err != nil {
		return err
	}
	for _, scope := range attachmentObjectScopes {
		err := s.attachments.Walk(ctx, prefix+scope.prefix, func(object attachment.Object) error {
			return s.attachmentHandlers().SweepOrphan(ctx, organization, scope.prefix, scope.endpoint, object)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func exerciseAttachmentMCPOperations(t *testing.T, f entryFixture, store *spacesFixture, browser, anonymous *browser, project, otherProject, writeToken, readToken, foreignToken string, record attachment.Metadata, content string) {
	t.Helper()
	personal := createTestAccessKey(t, browser, "project", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{project}}}}, "")
	if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE platform.access_keys SET permission='read' WHERE id=$1", personal.ID); err != nil {
		t.Fatal(err)
	}
	selector := map[string]any{"project_id": project, "attachment_id": record.ID}
	for _, route := range []string{"per-org", "personal global"} {
		for _, protocol := range []string{"2024-11-05", "2025-11-25", mcp.ProtocolVersion} {
			t.Run("attachment operations "+route+" "+protocol, func(t *testing.T) {
				call := attachmentMCPClient(t, anonymous, readToken, protocol)
				if route == "personal global" {
					invoke := entryMCPClient(t, anonymous, personal.Token, protocol, "/mcp")
					call = func(name string, arguments any) (json.RawMessage, bool) {
						if input, ok := arguments.(map[string]any); ok {
							if _, selected := input["organization_id"]; !selected {
								input["organization_id"] = "org_alpha"
							}
						}
						return invoke(name, arguments)
					}
				}
				selector := map[string]any{"project_id": project, "attachment_id": record.ID}
				listing, failed := call("tools/list", nil)
				if failed || !strings.Contains(string(listing), `"name":"read_attachment"`) || strings.Contains(string(listing), `"name":"delete_attachment"`) || strings.Contains(string(listing), `"name":"reference_attachment"`) {
					t.Fatalf("read-only catalog=%s", listing)
				}
				before := len(store.Requests())
				raw, failed := call(operatortool.ReadAttachmentMetadata, selector)
				var metadata attachment.Metadata
				if failed || json.Unmarshal(raw, &metadata) != nil || metadata.ID != record.ID || metadata.Size != record.Size || metadata.SHA256 != record.SHA256 || len(store.Requests()) != before {
					t.Fatalf("metadata=%s storage=%v", raw, len(store.Requests()) != before)
				}
				for _, part := range []struct {
					offset int64
					length int
					want   int
					eof    bool
				}{
					{0, 0, operatortool.AttachmentContentBytes, false},
					{17, 31, 31, false},
					{record.Size - 7, 31, 7, true},
					{record.Size, 1, 0, true},
				} {
					input := map[string]any{"project_id": project, "attachment_id": record.ID, "offset": part.offset}
					if part.length != 0 {
						input["length"] = part.length
					}
					raw, failed := call(operatortool.ReadAttachment, input)
					var read struct {
						Metadata attachment.Metadata `json:"metadata"`
						Content  string              `json:"content_base64"`
						Offset   int64               `json:"offset"`
						Returned int                 `json:"returned_bytes"`
						EOF      bool                `json:"eof"`
					}
					if failed || json.Unmarshal(raw, &read) != nil {
						t.Fatalf("read=%s", raw)
					}
					decoded, err := base64.StdEncoding.DecodeString(read.Content)
					if err != nil || read.Offset != part.offset || read.Returned != part.want || read.EOF != part.eof || string(decoded) != content[part.offset:part.offset+int64(part.want)] || read.Metadata.SHA256 != record.SHA256 {
						t.Fatalf("read evidence=%s", raw)
					}
				}
				for _, name := range []string{operatortool.DeleteAttachment, operatortool.ReferenceAttachment} {
					input := map[string]any{"project_id": project, "attachment_id": record.ID, "request_id": "read-only", "work_item_id": "wi_missing"}
					if name == operatortool.DeleteAttachment {
						delete(input, "work_item_id")
					}
					before := len(store.Requests())
					if raw, failed := call(name, input); !failed || len(store.Requests()) != before {
						t.Fatalf("read-only mutation=%s", raw)
					}
				}
				if route == "personal global" {
					for _, name := range []string{operatortool.ReadAttachment, operatortool.ReadAttachmentMetadata} {
						for _, input := range []map[string]any{
							{"project_id": project, "attachment_id": "att_00000000000000000000000000000001"},
							{"project_id": otherProject, "attachment_id": record.ID},
							{"organization_id": "org_beta", "project_id": project, "attachment_id": record.ID},
						} {
							before := len(store.Requests())
							if raw, failed := call(name, input); !failed || len(store.Requests()) != before {
								t.Fatalf("missing or forbidden personal read=%s", raw)
							}
						}
					}
					for _, input := range []map[string]any{
						{"project_id": project, "attachment_id": record.ID, "length": 32769},
						{"project_id": project, "attachment_id": record.ID, "length": 0},
						{"project_id": project, "attachment_id": record.ID, "offset": -1},
						{"project_id": project, "attachment_id": record.ID, "offset": record.Size + 1},
					} {
						before := len(store.Requests())
						if raw, failed := call(operatortool.ReadAttachment, input); !failed || len(store.Requests()) != before {
							t.Fatalf("invalid personal read=%s", raw)
						}
					}
				}
			})
		}
	}
	call := attachmentMCPClient(t, anonymous, writeToken, mcp.ProtocolVersion)
	for _, input := range []map[string]any{
		{"project_id": project, "attachment_id": record.ID, "length": 32769},
		{"project_id": project, "attachment_id": record.ID, "length": 0},
		{"project_id": project, "attachment_id": record.ID, "offset": -1},
		{"project_id": project, "attachment_id": record.ID, "offset": record.Size + 1},
		{"project_id": project, "attachment_id": "att_../../secret"},
		{"project_id": otherProject, "attachment_id": record.ID},
		{"project_id": project, "attachment_id": "att_00000000000000000000000000000001"},
	} {
		before := len(store.Requests())
		if raw, failed := call(operatortool.ReadAttachment, input); !failed || len(store.Requests()) != before {
			t.Fatalf("invalid or foreign read=%s", raw)
		}
	}
	foreign := attachmentMCPClient(t, anonymous, foreignToken, mcp.ProtocolVersion)
	if raw, failed := foreign(operatortool.ReadAttachmentMetadata, selector); !failed {
		t.Fatalf("ungranted project read=%s", raw)
	}
	betaProject := attachmentProject(t, browser, "org_beta")
	before := len(store.Requests())
	if raw, failed := call(operatortool.ReadAttachmentMetadata, map[string]string{"project_id": betaProject, "attachment_id": record.ID}); !failed || len(store.Requests()) != before {
		t.Fatalf("foreign organization project=%s", raw)
	}
	objectKey, err := attachment.Key("org_alpha", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.Mu.Lock()
	stored := store.Objects[objectKey]
	store.Objects[objectKey] = spacesObject{Content: []byte("short"), Created: stored.Created}
	store.Mu.Unlock()
	personalCall := entryMCPClient(t, anonymous, personal.Token, mcp.ProtocolVersion, "/mcp")
	for _, test := range []struct {
		name string
		read func() (json.RawMessage, bool)
	}{
		{name: "service", read: func() (json.RawMessage, bool) { return call(operatortool.ReadAttachment, selector) }},
		{name: "personal", read: func() (json.RawMessage, bool) {
			return personalCall(operatortool.ReadAttachment, map[string]any{"organization_id": "org_alpha", "project_id": project, "attachment_id": record.ID})
		}},
	} {
		t.Run("incomplete content "+test.name, func(t *testing.T) {
			raw, failed := test.read()
			var refusal apikey.Refusal
			if !failed || json.Unmarshal(raw, &refusal) != nil || refusal.Code != "attachment_unavailable" {
				t.Fatalf("incomplete content=%s failed=%v", raw, failed)
			}
		})
	}
	store.Mu.Lock()
	store.Objects[objectKey] = stored
	store.Mu.Unlock()
	client, err := hubclient.New(hubclient.Config{URL: testPublicURL + "/organizations/org_alpha", TokenSource: func() string { return writeToken }, HTTPClient: &http.Client{Transport: handlerTransport{handler: f.service.Handler()}}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_alpha", tracker.ProjectID(project))
	if err != nil {
		t.Fatal(err)
	}
	issue, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "attachment-operations-issue"}, Title: "Attachment operations", State: "Backlog"})
	if err != nil {
		t.Fatal(err)
	}
	comment, err := native.CreateComment(t.Context(), issue.WorkItemID, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "attachment-operations-comment"}, Body: "Reference target"})
	if err != nil {
		t.Fatal(err)
	}
	for _, commentID := range []string{"", comment.ID} {
		input := map[string]any{"project_id": project, "attachment_id": record.ID, "request_id": "bind-" + commentID, "work_item_id": string(issue.WorkItemID), "comment_id": commentID}
		for range 2 {
			if raw, failed := call(operatortool.ReferenceAttachment, input); failed {
				t.Fatalf("bind=%s", raw)
			}
		}
	}
	raw, failed := call(operatortool.ReadAttachmentMetadata, selector)
	var bound attachment.Metadata
	if failed || json.Unmarshal(raw, &bound) != nil || len(bound.ReferencedBy) != 2 {
		t.Fatalf("replayed references=%s", raw)
	}
	_, page := browser.Get("/organizations/org_alpha/app/bootstrap")
	csrf := csrfFrom(t, page)
	foreignIssue := attachmentRequest(t, browser, http.MethodPost, "/api/v2/organizations/org_alpha/projects/"+otherProject+"/work-items", strings.NewReader(`{"idempotency_key":"foreign-binding-target","title":"Foreign target","state":"Backlog"}`), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
	var foreignItem tracker.NativeIssue
	if foreignIssue.Code != http.StatusOK || json.Unmarshal(foreignIssue.Body.Bytes(), &foreignItem) != nil {
		t.Fatalf("foreign item=%s", foreignIssue.Body.String())
	}
	otherItem, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "other-comment-owner"}, Title: "Other comment owner", State: "Backlog"})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []attachment.SourceReference{{WorkItemID: "wi_missing"}, {WorkItemID: string(issue.WorkItemID), CommentID: "missing"}, {WorkItemID: string(foreignItem.WorkItemID)}, {WorkItemID: string(foreignItem.WorkItemID), CommentID: comment.ID}, {WorkItemID: string(otherItem.WorkItemID), CommentID: comment.ID}} {
		input := map[string]any{"project_id": project, "attachment_id": record.ID, "request_id": "missing-target", "work_item_id": target.WorkItemID, "comment_id": target.CommentID}
		if raw, failed := call(operatortool.ReferenceAttachment, input); !failed {
			t.Fatalf("foreign binding=%s", raw)
		}
	}
	for _, scenario := range []string{"direct", "revoked", "storage unavailable"} {
		t.Run("attachment deletion "+scenario, func(t *testing.T) {
			credential := createTestAccessKey(t, browser, "project", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{project}}}}, "")
			invoke := attachmentMCPClient(t, anonymous, credential.Token, "2025-11-25")
			upload := map[string]string{"request_id": "upload-" + scenario, "project_id": project, "name": "delete.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("delete evidence"))}
			raw, failed := invoke(operatortool.UploadAttachment, upload)
			var uploaded attachment.Metadata
			if failed || json.Unmarshal(raw, &uploaded) != nil {
				t.Fatalf("upload=%s", raw)
			}
			objectKey, err := attachment.Key("org_alpha", uploaded.ID)
			if err != nil {
				t.Fatal(err)
			}
			input := map[string]string{"project_id": project, "attachment_id": uploaded.ID, "request_id": "delete-" + scenario}
			if scenario == "revoked" {
				revoked := attachmentRequest(t, browser, http.MethodDelete, "/api/cloud/account/api-keys/"+credential.ID, nil, map[string]string{"X-CSRF-Token": csrf})
				if revoked.Code != 204 {
					t.Fatalf("revoke=%s", revoked.Body.String())
				}
				before := len(store.Requests())
				if raw, failed := invoke(operatortool.DeleteAttachment, input); !failed || len(store.Requests()) != before {
					t.Fatalf("revoked deletion=%s", raw)
				}
				if _, failed := invoke(operatortool.UploadAttachment, upload); !failed || len(store.Requests()) != before {
					t.Fatal("revoked upload replay touched storage")
				}
				return
			}
			store.Mu.Lock()
			store.FailDelete = scenario == "storage unavailable"
			store.Mu.Unlock()
			raw, failed = invoke(operatortool.DeleteAttachment, input)
			var receipt struct {
				Preview        chat.Action `json:"preview"`
				Status         string      `json:"status"`
				ObjectDeletion string      `json:"object_deletion"`
			}
			if failed || json.Unmarshal(raw, &receipt) != nil || receipt.Status != "succeeded" || strings.Contains(string(raw), "approval_url") {
				t.Fatalf("deletion=%s", raw)
			}
			if scenario == "storage unavailable" {
				if receipt.ObjectDeletion != "pending" {
					t.Fatalf("storage pending=%s", raw)
				}
				store.Mu.Lock()
				_, retained := store.Objects[objectKey]
				store.FailDelete = false
				store.Mu.Unlock()
				if !retained {
					t.Fatal("failed storage deletion removed bytes")
				}
				organization, err := f.service.readyOrganization(t.Context(), "org_alpha")
				if err != nil {
					t.Fatal(err)
				}
				if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
					t.Fatal(err)
				}
				raw, failed = invoke(operatortool.ActionResult, map[string]string{"action_id": receipt.Preview.ID})
				if failed || json.Unmarshal(raw, &receipt) != nil {
					t.Fatalf("deletion receipt=%s", raw)
				}
			}
			if receipt.ObjectDeletion != "confirmed" {
				t.Fatalf("object deletion=%s", raw)
			}
			store.Mu.Lock()
			_, retained := store.Objects[objectKey]
			store.Mu.Unlock()
			if retained {
				t.Fatal("completed deletion retained bytes")
			}
			raw, failed = invoke(operatortool.DeleteAttachment, input)
			if failed || json.Unmarshal(raw, &receipt) != nil || receipt.Status != "succeeded" {
				t.Fatalf("deletion replay=%s", raw)
			}
		})
	}
}
