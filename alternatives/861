package cloudentry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func attachmentMCPClient(t *testing.T, browser *browser, token, protocol string) func(string, any) (json.RawMessage, bool) {
	t.Helper()
	return entryMCPClient(t, browser, token, protocol, "/organizations/org_alpha/mcp")
}

func entryMCPClient(t *testing.T, browser *browser, token, protocol, path string) func(string, any) (json.RawMessage, bool) {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token, "Mcp-Protocol-Version": protocol}
	if protocol != "2026-07-28" {
		initialized := attachmentRequest(t, browser, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+protocol+`","capabilities":{},"clientInfo":{"name":"attachment-operations","version":"1"}}}`), headers)
		if initialized.Code != http.StatusOK {
			t.Fatalf("initialize=%d %s", initialized.Code, initialized.Body.String())
		}
		headers["Mcp-Session-Id"] = initialized.Header().Get("Mcp-Session-Id")
		attachmentRequest(t, browser, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), headers)
	}

	return func(name string, arguments any) (json.RawMessage, bool) {
		t.Helper()
		method, params := "tools/call", map[string]any{"name": name, "arguments": arguments}
		if name == "tools/list" {
			method, params = name, map[string]any{}
		}
		if protocol == "2026-07-28" {
			params["_meta"] = map[string]any{"io.modelcontextprotocol/protocolVersion": protocol, "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]string{"name": "attachment-operations", "version": "1"}}
			headers["Mcp-Method"] = method
			headers["Mcp-Name"] = name
		}
		raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		response := attachmentRequest(t, browser, http.MethodPost, path, bytes.NewReader(raw), headers)
		var frame struct {
			Error  json.RawMessage `json:"error"`
			Result json.RawMessage `json:"result"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &frame) != nil || len(frame.Error) != 0 {
			return response.Body.Bytes(), true
		}
		if method == "tools/list" {
			return frame.Result, false
		}
		var result struct {
			ResultType string                     `json:"resultType"`
			Meta       map[string]json.RawMessage `json:"_meta"`
			IsError    bool                       `json:"isError"`
			Content    []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured json.RawMessage `json:"structuredContent"`
		}
		if json.Unmarshal(frame.Result, &result) != nil || len(result.Content) != 1 {
			t.Fatalf("tool response=%s", response.Body.String())
		}
		if protocol == mcp.ProtocolVersion && (result.ResultType != "complete" || len(result.Meta["io.modelcontextprotocol/serverInfo"]) == 0) {
			t.Fatalf("modern tool result missing protocol envelope=%s", frame.Result)
		}
		if protocol != mcp.ProtocolVersion && (result.ResultType != "" || len(result.Meta) != 0) {
			t.Fatalf("legacy tool result returned modern protocol envelope=%s", frame.Result)
		}
		if protocol == "2024-11-05" && len(result.Structured) != 0 {
			t.Fatal("legacy MCP returned structured content")
		}
		if len(result.Content[0].Text) > operatortool.MaxResultBytes {
			t.Fatal("tool result exceeded its bound")
		}
		if strings.Contains(result.Content[0].Text, "authorized_principal") || strings.Contains(result.Content[0].Text, "spaces.test") || strings.Contains(result.Content[0].Text, "private/") {
			t.Fatal("tool result exposed internal attachment authority or storage")
		}
		if len(result.Structured) != 0 {
			var structured, textual any
			if json.Unmarshal(result.Structured, &structured) != nil || json.Unmarshal([]byte(result.Content[0].Text), &textual) != nil || !reflect.DeepEqual(structured, textual) {
				t.Fatal("structured result differs from text")
			}
		}
		return json.RawMessage(result.Content[0].Text), result.IsError
	}
}

func exerciseAttachmentMCPOperations(t *testing.T, f entryFixture, store *spacesFixture, browser, anonymous *browser, project, otherProject, writeToken, readToken, foreignToken string, record attachment.Metadata, content string) {
	t.Helper()
	personal := createTestAccessKey(t, browser, "project", []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{project}}}}, "")
	if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE access_keys SET permission='read' WHERE id=?", personal.ID); err != nil {
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
				before := len(store.requests())
				raw, failed := call(operatortool.ReadAttachmentMetadata, selector)
				var metadata attachment.Metadata
				if failed || json.Unmarshal(raw, &metadata) != nil || metadata.ID != record.ID || metadata.Size != record.Size || metadata.SHA256 != record.SHA256 || len(store.requests()) != before {
					t.Fatalf("metadata=%s storage=%v", raw, len(store.requests()) != before)
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
					before := len(store.requests())
					if raw, failed := call(name, input); !failed || len(store.requests()) != before {
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
							before := len(store.requests())
							if raw, failed := call(name, input); !failed || len(store.requests()) != before {
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
						before := len(store.requests())
						if raw, failed := call(operatortool.ReadAttachment, input); !failed || len(store.requests()) != before {
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
		before := len(store.requests())
		if raw, failed := call(operatortool.ReadAttachment, input); !failed || len(store.requests()) != before {
			t.Fatalf("invalid or foreign read=%s", raw)
		}
	}
	foreign := attachmentMCPClient(t, anonymous, foreignToken, mcp.ProtocolVersion)
	if raw, failed := foreign(operatortool.ReadAttachmentMetadata, selector); !failed {
		t.Fatalf("ungranted project read=%s", raw)
	}
	betaProject := attachmentProject(t, browser, "org_beta")
	before := len(store.requests())
	if raw, failed := call(operatortool.ReadAttachmentMetadata, map[string]string{"project_id": betaProject, "attachment_id": record.ID}); !failed || len(store.requests()) != before {
		t.Fatalf("foreign organization project=%s", raw)
	}
	objectKey, err := attachment.Key("org_alpha", record.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	stored := store.objects[objectKey]
	store.objects[objectKey] = spacesObject{content: []byte("short"), created: stored.created}
	store.mu.Unlock()
	if raw, failed := call(operatortool.ReadAttachment, selector); !failed {
		t.Fatalf("incomplete content succeeded=%s", raw)
	}
	personalCall := entryMCPClient(t, anonymous, personal.Token, mcp.ProtocolVersion, "/mcp")
	raw, failed := personalCall(operatortool.ReadAttachment, map[string]any{"organization_id": "org_alpha", "project_id": project, "attachment_id": record.ID})
	var refusal apikey.Refusal
	if !failed || json.Unmarshal(raw, &refusal) != nil || refusal.Code != "attachment_unavailable" {
		t.Fatalf("incomplete personal content=%s failed=%v", raw, failed)
	}
	store.mu.Lock()
	store.objects[objectKey] = stored
	store.mu.Unlock()
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
	raw, failed = call(operatortool.ReadAttachmentMetadata, selector)
	var bound attachment.Metadata
	if failed || json.Unmarshal(raw, &bound) != nil || len(bound.ReferencedBy) != 2 {
		t.Fatalf("replayed references=%s", raw)
	}
	_, page := browser.get("/organizations/org_alpha/organization")
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
			keyRaw, _ := json.Marshal(map[string]any{"name": "attachment-delete-" + scenario, "scope": "write", "expires_days": 1, "project_ids": []string{project}})
			created := attachmentRequest(t, browser, http.MethodPost, "/api/v2/organizations/org_alpha/api-keys", bytes.NewReader(keyRaw), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
			var credential struct {
				ID    string `json:"id"`
				Token string `json:"token"`
			}
			if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &credential) != nil {
				t.Fatalf("key=%s", created.Body.String())
			}
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
				revoked := attachmentRequest(t, browser, http.MethodDelete, "/api/v2/organizations/org_alpha/api-keys/"+credential.ID, nil, map[string]string{"X-CSRF-Token": csrf})
				if revoked.Code != 204 {
					t.Fatalf("revoke=%s", revoked.Body.String())
				}
				before := len(store.requests())
				if raw, failed := invoke(operatortool.DeleteAttachment, input); !failed || len(store.requests()) != before {
					t.Fatalf("revoked deletion=%s", raw)
				}
				if _, failed := invoke(operatortool.UploadAttachment, upload); !failed || len(store.requests()) != before {
					t.Fatal("revoked upload replay touched storage")
				}
				return
			}
			store.mu.Lock()
			store.failDelete = scenario == "storage unavailable"
			store.mu.Unlock()
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
				store.mu.Lock()
				_, retained := store.objects[objectKey]
				store.failDelete = false
				store.mu.Unlock()
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
			store.mu.Lock()
			_, retained := store.objects[objectKey]
			store.mu.Unlock()
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

func TestAttachmentEntryKeyUploads(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	owner := newBrowser(t, f.service.Handler())
	owner.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	project := attachmentProject(t, owner, "org_alpha")
	approveEntryFixturePolicy(t, owner, "org_alpha", project)
	anonymous := newBrowser(t, f.service.Handler())
	var pixels bytes.Buffer
	if err := png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	organizations := []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{project}}}}
	personal := createTestAccessKey(t, owner, "global", nil, "")
	scopedPersonal := createTestAccessKey(t, owner, "project", organizations, "")
	service := createTestAccessKey(t, owner, "selected", organizations, "org_alpha")
	readOnlyService := createTestAccessKey(t, owner, "selected", organizations, "org_alpha")
	_, page := owner.get("/organizations/org_alpha/organization")
	other := owner.do(http.MethodPost, "/organizations/org_alpha/projects", url.Values{"name": {"Other evidence project"}, "grant_access": {"true"}, "csrf": {csrfFrom(t, page)}}, nil)
	if other.StatusCode != http.StatusSeeOther {
		t.Fatalf("other project=%d", other.StatusCode)
	}
	otherProject := strings.TrimPrefix(other.Header.Get("Location"), "/organizations/org_alpha/projects/")
	otherOrganizations := []keyOrganization{{OrganizationID: "org_alpha", ProjectContext: apikey.ProjectContext{Access: "project", Projects: []string{otherProject}}}}
	otherPersonal := createTestAccessKey(t, owner, "project", otherOrganizations, "")
	otherService := createTestAccessKey(t, owner, "selected", otherOrganizations, "org_alpha")
	readOnly := createTestAccessKey(t, owner, "project", organizations, "")
	if _, err := f.service.auth.store.db.ExecContext(t.Context(), "UPDATE access_keys SET permission='read' WHERE id IN (?,?)", readOnly.ID, readOnlyService.ID); err != nil {
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
			t.Run(credential.name+"/"+route, func(t *testing.T) {
				requestID := strings.ReplaceAll(credential.name+"-"+route, " ", "-")
				before := len(store.requests())
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
					call := entryMCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion, path)
					raw, refused := call(operatortool.UploadAttachment, args)
					failed = refused
					if !failed {
						uploadErr = json.Unmarshal(raw, &uploaded)
					}
				case "header API":
					response := attachmentRequest(t, anonymous, http.MethodPost, "/api/v2/projects/"+project+"/attachments", bytes.NewReader(pixels.Bytes()), map[string]string{"Authorization": "Bearer " + credential.key.Token, "X-Detent-Organization": "org_alpha", "Content-Type": "image/png", "X-Attachment-Name": "evidence.png"})
					failed = response.Code != http.StatusCreated
					if !failed {
						uploadErr = json.Unmarshal(response.Body.Bytes(), &uploaded)
					}
				default:
					base := testPublicURL
					if route == "CLI per-org" {
						base += "/organizations/org_alpha"
					}
					client, err := hubclient.New(hubclient.Config{URL: base, TokenSource: func() string { return credential.key.Token }, HTTPClient: &http.Client{Transport: handlerTransport{handler: f.service.Handler()}}})
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
					if !failed || len(store.requests()) != before {
						t.Fatalf("unauthorized upload refused=%v storage requests=%d want=%d", failed, len(store.requests()), before)
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
				store.mu.Lock()
				stored := bytes.Equal(store.objects[objectKey].content, pixels.Bytes())
				store.mu.Unlock()
				if !stored {
					t.Fatal("uploaded object does not contain the PNG")
				}
				readCall := attachmentMCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion)
				readArguments := map[string]any{"project_id": project, "attachment_id": uploaded.ID}
				if route == "multi-org MCP" {
					readCall = entryMCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion, "/mcp")
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
				call := entryMCPClient(t, anonymous, credential.key.Token, mcp.ProtocolVersion, "/mcp")
				draft := map[string]any{"organization_id": "org_alpha", "project_id": project, "request_id": requestID + "-issue", "title": "Attachment evidence", "description": uploaded.Reference + "\n\n" + issueContractTestSections}
				raw, failed := call(operatortool.FileIssue, draft)
				var challenge struct {
					Data nativeDuplicateFixture `json:"data"`
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
				if failed || json.Unmarshal(raw, &issue) != nil || issue.Data.WorkItemID == "" || issue.Data.Body != uploaded.Reference+"\n\n"+issueContractTestSections {
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
						read := attachmentRequest(t, owner, http.MethodGet, ref.URL, nil, nil)
						if read.Code != http.StatusOK || !bytes.Equal(read.Body.Bytes(), pixels.Bytes()) {
							t.Fatalf("embedded PNG read=%d", read.Code)
						}
						found = true
					}
					if !found {
						t.Fatalf("missing image reference in %s", body)
					}
				}
			})
		}
	}
}

func TestReporterAttachmentMCPRoundTrip(t *testing.T) {
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	f.provider.member("user_alice", "porg_alpha", "reporter")
	browser := newBrowser(t, f.service.Handler())
	browser.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
	_, body := browser.get("/organizations/org_alpha/organization")
	csrf := csrfFrom(t, body)
	project := attachmentProject(t, browser, "org_alpha")
	keyRaw, err := json.Marshal(map[string]any{"name": "Reporter screenshot key", "scope": "write", "expires_days": 1, "project_ids": []string{project}})
	if err != nil {
		t.Fatal(err)
	}
	created := attachmentRequest(t, browser, http.MethodPost, "/api/v2/organizations/org_alpha/api-keys", bytes.NewReader(keyRaw), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
	var key struct {
		Token string `json:"token"`
	}
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &key) != nil || key.Token == "" {
		t.Fatalf("key=%d %s", created.Code, created.Body.String())
	}
	anonymous := newBrowser(t, f.service.Handler())
	call := attachmentMCPClient(t, anonymous, key.Token, mcp.ProtocolVersion)
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
