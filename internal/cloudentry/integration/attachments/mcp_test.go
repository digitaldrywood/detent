package attachments_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/cloudentry"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entryfixture"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/entrytest"
	"github.com/digitaldrywood/detent/internal/cloudentry/internal/harness"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentEntryKeyUploads(t *testing.T) {
	t.Parallel()
	t.Run("postgres", func(t *testing.T) {
		f, store := newStorageFixture(t)

		owner := harness.NewBrowser(t, f.Service.Handler())
		owner.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
		project := harness.Project(t, owner, "org_alpha")
		harness.ApprovePolicy(t, owner, "org_alpha", project)
		anonymous := harness.NewBrowser(t, f.Service.Handler())
		var pixels bytes.Buffer
		if err := png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		organizations := []harness.KeyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{project}}}}
		personal := createTestAccessKey(t, owner, "global", nil, "")
		scopedPersonal := createTestAccessKey(t, owner, "project", organizations, "")
		service := createTestAccessKey(t, owner, "selected", organizations, "org_alpha")
		readOnlyService := createTestAccessKey(t, owner, "selected", organizations, "org_alpha")
		_, page := owner.Get("/organizations/org_alpha/app/bootstrap")
		otherProject := harness.CreateProject(t, owner, "Other evidence project", harness.CSRF(t, page))
		otherOrganizations := []harness.KeyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{otherProject}}}}
		otherPersonal := createTestAccessKey(t, owner, "project", otherOrganizations, "")
		otherService := createTestAccessKey(t, owner, "selected", otherOrganizations, "org_alpha")
		readOnly := createTestAccessKey(t, owner, "project", organizations, "")
		if _, err := f.DB.ExecContext(t.Context(), "UPDATE platform.access_keys SET permission='read' WHERE id IN ($1,$2)", readOnly.ID, readOnlyService.ID); err != nil {
			t.Fatal(err)
		}
		for _, credential := range []struct {
			name   string
			key    accessKey
			denied bool
		}{
			{"personal global", personal, false},
			{"personal project", scopedPersonal, false},
			{"service", service, false},
			{"read only personal", readOnly, true},
			{"read only service", readOnlyService, true},
			{"other project personal", otherPersonal, true},
			{"other project service", otherService, true},
		} {
			for _, route := range []string{"multi-org MCP", "per-org MCP", "CLI root", "CLI per-org", "header API"} {
				t.Logf("credential=%s route=%s", credential.name, route)
				func() {
					requestID := strings.ReplaceAll(credential.name+"-"+route, " ", "-")
					before := len(store.Requests())
					var uploaded attachment.Metadata
					var uploadErr error
					var failed bool
					switch route {
					case "multi-org MCP", "per-org MCP":
						path := "/organizations/org_alpha/mcp"
						args := map[string]any{"project_id": project, "request_id": requestID, "name": "evidence.png", "content_type": "image/png", "content_base64": base64.StdEncoding.EncodeToString(pixels.Bytes())}
						if route == "multi-org MCP" {
							path = "/mcp"
							args["organization_id"] = "org_alpha"
						}
						call := harness.MCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion, path)
						raw, refused := call(operatortool.UploadAttachment, args)
						failed = refused
						if !failed {
							uploadErr = json.Unmarshal(raw, &uploaded)
						}
					case "header API":
						response := harness.Request(t, anonymous, http.MethodPost, "/api/v2/projects/"+project+"/attachments", bytes.NewReader(pixels.Bytes()), map[string]string{"Authorization": "Bearer " + credential.key.Token, "X-Detent-Organization": "org_alpha", "Content-Type": "image/png", "X-Attachment-Name": "evidence.png"})
						failed = response.Code != http.StatusCreated
						if !failed {
							uploadErr = json.Unmarshal(response.Body.Bytes(), &uploaded)
						}
					default:
						base := harness.PublicURL
						if route == "CLI per-org" {
							base += "/organizations/org_alpha"
						}
						client, err := hubclient.New(hubclient.Config{URL: base, TokenSource: func() string { return credential.key.Token }, HTTPClient: &http.Client{Transport: entrytest.HandlerTransport{Handler: f.Service.Handler()}}})
						if err != nil {
							t.Fatal(err)
						}
						native, err := client.Native("org_alpha", tracker.ProjectID(project))
						if err != nil {
							t.Fatal(err)
						}
						uploaded, uploadErr = native.UploadAttachment(t.Context(), bytes.NewReader(pixels.Bytes()), "evidence.png", "image/png")
						failed = uploadErr != nil
					}
					if credential.denied {
						if !failed || len(store.Requests()) != before {
							t.Fatalf("unauthorized upload refused=%v storage requests=%d want=%d", failed, len(store.Requests()), before)
						}
						return
					}
					if failed || uploadErr != nil || uploaded.ID == "" || uploaded.Reference != uploaded.Markdown("org_alpha") || uploaded.Validate() != nil {
						t.Fatalf("upload failed=%v err=%v metadata=%+v", failed, uploadErr, uploaded)
					}
					objectKey, err := attachment.Key("org_alpha", uploaded.ID)
					if err != nil {
						t.Fatal(err)
					}
					store.Mu.Lock()
					stored := bytes.Equal(store.Objects[objectKey].Content, pixels.Bytes())
					store.Mu.Unlock()
					if !stored {
						t.Fatal("uploaded object does not contain the PNG")
					}
					readCall := harness.AttachmentMCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion)
					readArguments := map[string]any{"project_id": project, "attachment_id": uploaded.ID}
					if route == "multi-org MCP" {
						readCall = harness.MCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion, "/mcp")
						readArguments["organization_id"] = "org_alpha"
					}
					for _, name := range []string{operatortool.ReadAttachmentMetadata, operatortool.ReadAttachment} {
						raw, refused := readCall(name, readArguments)
						var result struct {
							ID            string              `json:"id"`
							Metadata      attachment.Metadata `json:"metadata"`
							ContentBase64 string              `json:"content_base64"`
						}
						if refused || json.Unmarshal(raw, &result) != nil {
							t.Fatalf("%s=%s refused=%v", name, raw, refused)
						}
						if name == operatortool.ReadAttachmentMetadata && result.ID != uploaded.ID || name == operatortool.ReadAttachment && (result.Metadata.ID != uploaded.ID || result.ContentBase64 != base64.StdEncoding.EncodeToString(pixels.Bytes())) {
							t.Fatalf("%s returned different attachment content: %s", name, raw)
						}
					}
					call := harness.MCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion, "/mcp")
					draft := map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": requestID + "-issue", "title": "Attachment evidence", "description": uploaded.Reference + "\n\n" + harness.IssueContractSections}
					raw, failed := call(operatortool.FileIssue, draft)
					var challenge struct {
						Data harness.DuplicateChallenge `json:"data"`
					}
					if failed || json.Unmarshal(raw, &challenge) != nil {
						t.Fatalf("file fixture=%s failed=%v", raw, failed)
					}
					if challenge.Data.ChallengeToken != "" {
						var numbers []int
						for _, match := range challenge.Data.PossibleDuplicates {
							numbers = append(numbers, match.Number)
						}
						draft["challenge_token"], draft["not_duplicate_of"] = challenge.Data.ChallengeToken, numbers
						raw, failed = call(operatortool.FileIssue, draft)
					}
					var issue struct {
						Data tracker.NativeIssue `json:"data"`
					}
					if failed || json.Unmarshal(raw, &issue) != nil || issue.Data.WorkItemID == "" || issue.Data.Body != uploaded.Reference+"\n\n"+harness.IssueContractSections {
						t.Fatalf("file_issue=%s failed=%v", raw, failed)
					}
					raw, failed = call(operatortool.AddComment, map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": requestID + "-comment", "identifier": string(issue.Data.WorkItemID), "body": uploaded.Reference})
					var comment struct {
						Data tracker.NativeComment `json:"data"`
					}
					if failed || json.Unmarshal(raw, &comment) != nil || comment.Data.Body != uploaded.Reference {
						t.Fatalf("add_comment=%s failed=%v", raw, failed)
					}
					for _, body := range []string{issue.Data.Body, comment.Data.Body} {
						refs := attachment.References(body, "org_alpha", project)
						found := false
						for _, ref := range refs {
							if ref.ID != uploaded.ID || !ref.Image {
								continue
							}
							read := harness.Request(t, owner, http.MethodGet, ref.URL, nil, nil)
							if read.Code != http.StatusOK || !bytes.Equal(read.Body.Bytes(), pixels.Bytes()) {
								t.Fatalf("embedded PNG read=%d", read.Code)
							}
							found = true
						}
						if !found {
							t.Fatalf("missing image reference in %s", body)
						}
					}
				}()
			}
		}
	})
}

func TestReporterAttachmentMCPRoundTrip(t *testing.T) {
	t.Parallel()
	f, _ := newStorageFixture(t)

	f.Provider.Member("user_alice", "porg_alpha", "reporter")
	browser := harness.NewBrowser(t, f.Service.Handler())
	browser.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	for _, statement := range []string{`INSERT INTO public.hosted_project_grants(organization_id,user_id,project_id,can_write,manage_runner,explicit) SELECT organization_id,'user_alice',id,true,false,true FROM public.projects WHERE organization_id='org_alpha' AND deleted_at IS NULL`, `INSERT INTO public.token_grants(organization_id,token_id,project_id) SELECT p.organization_id,m.principal_id,p.id FROM public.projects p JOIN public.hosted_members m ON m.organization_id=p.organization_id AND m.user_id='user_alice' WHERE p.organization_id='org_alpha' AND p.deleted_at IS NULL ON CONFLICT DO NOTHING`} {
		if _, err := f.Fixtures["org_alpha"].Database.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	project := harness.Project(t, browser, "org_alpha")
	key := createTestAccessKey(t, browser, "project", []harness.KeyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{project}}}}, "")
	anonymous := harness.NewBrowser(t, f.Service.Handler())
	call := harness.AttachmentMCPClient(t, anonymous, key.Token, mcp.ProtocolVersion)
	var pixels bytes.Buffer
	if err := png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	har := []byte(`{"log":{"version":"1.2","entries":[{"request":{"method":"GET","url":"https://app.example.test/api"},"response":{"status":500}}]}}`)
	consoleLog := []byte("12:00:01 [error] Uncaught TypeError: settings is undefined\n")
	uploads := []struct {
		name, contentType, media string
		content                  []byte
	}{
		{"screenshot.png", "", "image/png", pixels.Bytes()},
		{"session.har", "", attachment.HARMediaType, har},
		{"network.har", "application/har+json", attachment.HARMediaType, har},
		{"console.log", "", "text/plain", consoleLog},
	}
	records := make([]attachment.Metadata, len(uploads))
	var description strings.Builder
	description.WriteString("## Steps to reproduce\nOpen settings and save.\n## Expected\nSettings persist.\n## Actual\nSettings revert.\n## Where\nSettings screen.\n")
	for index, upload := range uploads {
		arguments := map[string]any{"request_id": "reporter-" + upload.name, "project_id": project, "name": upload.name, "content_base64": base64.StdEncoding.EncodeToString(upload.content)}
		if upload.contentType != "" {
			arguments["content_type"] = upload.contentType
		}
		raw, failed := call(operatortool.UploadAttachment, arguments)
		record := &records[index]
		if failed || json.Unmarshal(raw, record) != nil || record.ID == "" || record.ContentType != upload.media || record.Size != int64(len(upload.content)) {
			t.Fatalf("%s upload=%s failed=%v", upload.name, raw, failed)
		}
		description.WriteString("\n[" + upload.name + "](attachment:" + record.ID + ")")
	}
	if raw, failed := call(operatortool.UploadAttachment, map[string]any{"request_id": "reporter-html", "project_id": project, "name": "page.html", "content_type": "text/html", "content_base64": base64.StdEncoding.EncodeToString([]byte("<!doctype html><html>secret</html>"))}); !failed {
		t.Fatalf("HTML upload accepted: %s", raw)
	}
	raw, failed := call(operatortool.FileIssue, map[string]any{"request_id": "reporter-file", "project_id": project, "type": "bug", "title": "Screenshot report", "description": description.String(), "report_context": map[string]any{"browser": "Chrome 130", "app_build": "v1.4.2"}, "state": "Todo"})
	var filed struct {
		Data tracker.NativeIssue `json:"data"`
	}
	if failed || json.Unmarshal(raw, &filed) != nil || filed.Data.State != "Triage" || filed.Data.Type != "bug" || filed.Data.WorkItemID == "" || filed.Data.ReportContext == nil || filed.Data.ReportContext.Browser != "Chrome 130" {
		t.Fatalf("file=%s failed=%v", raw, failed)
	}
	for index, upload := range uploads {
		raw, failed = call(operatortool.ReadAttachment, map[string]any{"project_id": project, "attachment_id": records[index].ID})
		var read struct {
			Content  string              `json:"content_base64"`
			Metadata attachment.Metadata `json:"metadata"`
			Returned int                 `json:"returned_bytes"`
			EOF      bool                `json:"eof"`
		}
		if failed || json.Unmarshal(raw, &read) != nil {
			t.Fatalf("%s read=%s failed=%v", upload.name, raw, failed)
		}
		content, err := base64.StdEncoding.DecodeString(read.Content)
		if err != nil || !bytes.Equal(content, upload.content) || read.Returned != len(upload.content) || !read.EOF || read.Metadata.ContentType != upload.media {
			t.Fatalf("%s readback differs: bytes=%d err=%v", upload.name, len(content), err)
		}
		if len(read.Metadata.ReferencedBy) != 1 || read.Metadata.ReferencedBy[0].WorkItemID != string(filed.Data.WorkItemID) {
			t.Fatalf("%s issue reference=%+v", upload.name, read.Metadata.ReferencedBy)
		}
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
			t.Parallel()
			f, store := newStorageFixture(t)
			store.FailPutResponse = test.storageLoss

			baseTransport := f.Seams.TransportFunc()
			f.Seams.SetTransport(func(org cloudentry.Organization) (http.RoundTripper, error) {
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
			})
			alice := harness.NewBrowser(t, f.Service.Handler())
			alice.Login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			_, body := alice.Get("/organizations/org_alpha/app/bootstrap")
			csrf := harness.CSRF(t, body)
			project := harness.Project(t, alice, "org_alpha")
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
					responses <- harness.Request(t, alice, http.MethodPost, path, strings.NewReader("settlement data"), headers)
				}()
			}
			response := harness.Request(t, alice, http.MethodPost, path, strings.NewReader("settlement data"), headers)
			if concurrent != nil {
				other := <-concurrent
				if other.Code != http.StatusCreated || other.Body.String() != response.Body.String() {
					t.Fatalf("concurrent upload=%d %s original=%s", other.Code, other.Body.String(), response.Body.String())
				}
			}
			if test.keyed {
				replay := harness.Request(t, alice, http.MethodPost, path, strings.NewReader("settlement data"), headers)
				if replay.Code != http.StatusCreated || replay.Body.String() != response.Body.String() {
					t.Fatalf("replay=%d %s original=%s", replay.Code, replay.Body.String(), response.Body.String())
				}
			}
			if response.Code != test.status {
				t.Fatalf("settlement=%d %s", response.Code, response.Body.String())
			}
			store.Mu.Lock()
			count := len(store.Objects)
			store.Mu.Unlock()
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
			read := harness.Request(t, alice, http.MethodGet, path+"/"+record.ID, nil, nil)
			if read.Code != 200 || read.Body.String() != "settlement data" {
				t.Fatalf("settled read=%d %s", read.Code, read.Body.String())
			}
		})
	}
}

func TestMain(m *testing.M) { entryfixture.Main(m) }
func newStorageFixture(t *testing.T) (entryfixture.Fixture, *harness.Spaces) {
	t.Helper()
	store := harness.NewSpaces(t, false)
	cfg := store.Config()
	return entryfixture.New(t, func(c *cloudentry.Config) {
		c.Attachments = &cfg
		c.AttachmentTransport = store.Transport
	}), store
}

type accessKey struct {
	ID    string
	Token string
}

func createTestAccessKey(t *testing.T, b *harness.Browser, context string, organizations []harness.KeyOrganization, serviceOrg string, permissions ...string) accessKey {
	t.Helper()
	var key accessKey
	harness.CreateAccessKey(t, b, context, organizations, serviceOrg, &key, permissions...)
	return key
}
