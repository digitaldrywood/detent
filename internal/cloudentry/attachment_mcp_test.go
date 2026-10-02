package cloudentry

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func attachmentMCPClient(t *testing.T, browser *browser, token, protocol string) func(string, any) (json.RawMessage, bool) {
	t.Helper()
	path := "/organizations/org_alpha/mcp"
	headers := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + token, "Mcp-Protocol-Version": protocol}
	initialized := attachmentRequest(t, browser, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+protocol+`","capabilities":{},"clientInfo":{"name":"attachment-operations","version":"1"}}}`), headers)
	if initialized.Code != http.StatusOK {
		t.Fatalf("initialize=%d %s", initialized.Code, initialized.Body.String())
	}
	headers["Mcp-Session-Id"] = initialized.Header().Get("Mcp-Session-Id")
	attachmentRequest(t, browser, http.MethodPost, path, strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), headers)
	return func(name string, arguments any) (json.RawMessage, bool) {
		t.Helper()
		method, params := "tools/call", map[string]any{"name": name, "arguments": arguments}
		if name == "tools/list" {
			method, params = name, map[string]any{}
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
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured json.RawMessage `json:"structuredContent"`
		}
		if json.Unmarshal(frame.Result, &result) != nil || len(result.Content) != 1 {
			t.Fatalf("tool response=%s", response.Body.String())
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
	selector := map[string]any{"project_id": project, "attachment_id": record.ID}
	for _, protocol := range []string{"2024-11-05", "2025-11-25"} {
		t.Run("attachment operations "+protocol, func(t *testing.T) {
			call := attachmentMCPClient(t, anonymous, readToken, protocol)
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
		})
	}
	call := attachmentMCPClient(t, anonymous, writeToken, "2025-11-25")
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
	foreign := attachmentMCPClient(t, anonymous, foreignToken, "2025-11-25")
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
	issue, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "attachment-operations-issue"}, Title: "Attachment operations", State: "Todo"})
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
	_, page := browser.get("/organizations/org_alpha/organization")
	csrf := csrfFrom(t, page)
	foreignIssue := attachmentRequest(t, browser, http.MethodPost, "/api/v2/organizations/org_alpha/projects/"+otherProject+"/work-items", strings.NewReader(`{"idempotency_key":"foreign-binding-target","title":"Foreign target","state":"Todo"}`), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
	var foreignItem tracker.NativeIssue
	if foreignIssue.Code != http.StatusOK || json.Unmarshal(foreignIssue.Body.Bytes(), &foreignItem) != nil {
		t.Fatalf("foreign item=%s", foreignIssue.Body.String())
	}
	otherItem, err := native.CreateIssue(t.Context(), tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "other-comment-owner"}, Title: "Other comment owner", State: "Todo"})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []attachment.SourceReference{{WorkItemID: "wi_missing"}, {WorkItemID: string(issue.WorkItemID), CommentID: "missing"}, {WorkItemID: string(foreignItem.WorkItemID)}, {WorkItemID: string(foreignItem.WorkItemID), CommentID: comment.ID}, {WorkItemID: string(otherItem.WorkItemID), CommentID: comment.ID}} {
		input := map[string]any{"project_id": project, "attachment_id": record.ID, "request_id": "missing-target", "work_item_id": target.WorkItemID, "comment_id": target.CommentID}
		if raw, failed := call(operatortool.ReferenceAttachment, input); !failed {
			t.Fatalf("foreign binding=%s", raw)
		}
	}
	for _, decision := range []string{"reject", "confirm", "stale", "revoked", "yolo", "storage unavailable"} {
		t.Run("attachment deletion "+decision, func(t *testing.T) {
			keyRaw, err := json.Marshal(map[string]any{"name": "attachment-delete-" + decision, "scope": "write", "expires_days": 1, "project_ids": []string{project}})
			if err != nil {
				t.Fatal(err)
			}
			created := attachmentRequest(t, browser, http.MethodPost, "/api/v2/organizations/org_alpha/api-keys", bytes.NewReader(keyRaw), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf})
			var credential struct {
				ID    string `json:"id"`
				Token string `json:"token"`
			}
			if created.Code != 201 || json.Unmarshal(created.Body.Bytes(), &credential) != nil {
				t.Fatalf("key=%s", created.Body.String())
			}
			invoke := attachmentMCPClient(t, anonymous, credential.Token, "2025-11-25")
			raw, failed := invoke(operatortool.UploadAttachment, map[string]string{"project_id": project, "name": "delete.txt", "content_base64": base64.StdEncoding.EncodeToString([]byte("delete evidence"))})
			var uploaded attachment.Metadata
			if failed || json.Unmarshal(raw, &uploaded) != nil {
				t.Fatalf("upload=%s", raw)
			}
			input := map[string]string{"project_id": project, "attachment_id": uploaded.ID, "request_id": "delete-" + decision}
			raw, failed = invoke(operatortool.DeleteAttachment, input)
			var receipt struct {
				Preview        chat.Action `json:"preview"`
				Status         string      `json:"status"`
				ObjectDeletion string      `json:"object_deletion"`
				ApprovalURL    string      `json:"approval_url"`
			}
			if failed || json.Unmarshal(raw, &receipt) != nil || receipt.Status != "pending" {
				t.Fatalf("preview=%s", raw)
			}
			objectKey, err := attachment.Key("org_alpha", uploaded.ID)
			if err != nil {
				t.Fatal(err)
			}
			store.mu.Lock()
			_, retained := store.objects[objectKey]
			store.mu.Unlock()
			if !retained {
				t.Fatal("pending deletion removed bytes")
			}
			if decision == "stale" {
				if raw, failed := invoke(operatortool.ReferenceAttachment, map[string]string{"project_id": project, "attachment_id": uploaded.ID, "request_id": "stale-bind", "work_item_id": string(issue.WorkItemID)}); failed {
					t.Fatalf("stale bind=%s", raw)
				}
			}
			if decision == "revoked" {
				revoked := attachmentRequest(t, browser, http.MethodDelete, "/api/v2/organizations/org_alpha/api-keys/"+credential.ID, nil, map[string]string{"X-CSRF-Token": csrf})
				if revoked.Code != 204 {
					t.Fatalf("revoke=%d %s", revoked.Code, revoked.Body.String())
				}
			}
			approvalPath := "/organizations/org_alpha/chat/approval"
			response, html := browser.get(approvalPath + "?connection_id=" + receipt.Preview.ConnectionID)
			if response.StatusCode != 200 {
				t.Fatalf("approval=%d %s", response.StatusCode, html)
			}
			formToken := ""
			for _, form := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(html, -1) {
				if strings.Contains(form, `name="action_id" value="`+receipt.Preview.ID+`"`) {
					match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(form)
					if len(match) == 2 {
						formToken = match[1]
					}
				}
			}
			choice := "confirm"
			if decision == "reject" {
				choice = "reject"
			}
			form := url.Values{"csrf": {csrfFrom(t, html)}, "connection_id": {receipt.Preview.ConnectionID}, "action_id": {receipt.Preview.ID}, "form_token": {formToken}, "decision": {choice}}
			if decision == "yolo" {
				form.Set("decision", "mode")
				form.Set("mode", "yolo")
				form.Del("action_id")
				for _, fragment := range regexp.MustCompile(`<form[^>]*>[\s\S]*?</form>`).FindAllString(html, -1) {
					if strings.Contains(fragment, `name="decision" value="mode"`) {
						match := regexp.MustCompile(`name="form_token" value="([^"]+)"`).FindStringSubmatch(fragment)
						if len(match) == 2 {
							form.Set("form_token", match[1])
						}
					}
				}
			}
			confirmed := browser.do(http.MethodPost, approvalPath, form, nil)
			if !slices.Contains([]int{303, 403, 409}, confirmed.StatusCode) {
				t.Fatalf("decision=%d %s", confirmed.StatusCode, confirmed.Body)
			}
			if decision == "yolo" {
				input["request_id"] = "delete-yolo-authorized"
			}
			if decision == "storage unavailable" {
				store.mu.Lock()
				store.failDelete = true
				store.mu.Unlock()
			}
			if decision == "confirm" {
				raw, failed = invoke(operatortool.ActionResult, map[string]string{"action_id": receipt.Preview.ID})
			} else {
				raw, failed = invoke(operatortool.DeleteAttachment, input)
			}
			if decision == "revoked" {
				if !failed {
					t.Fatalf("revoked replay=%s", raw)
				}
				before := len(store.requests())
				if raw, failed := invoke(operatortool.ReadAttachment, map[string]string{"project_id": project, "attachment_id": uploaded.ID}); !failed || len(store.requests()) != before {
					t.Fatalf("revoked read=%s", raw)
				}
			} else {
				receipt.ApprovalURL, receipt.ObjectDeletion = "", ""
				want := "succeeded"
				if decision == "reject" {
					want = "rejected"
				}
				if decision == "stale" {
					want = "failed"
				}
				if failed || json.Unmarshal(raw, &receipt) != nil || receipt.Status != want || receipt.ApprovalURL != "" {
					t.Fatalf("receipt=%s", raw)
				}
				if decision == "storage unavailable" {
					if receipt.ObjectDeletion != "pending" {
						t.Fatalf("failed storage deletion=%s", raw)
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
						t.Fatalf("maintenance receipt=%s", raw)
					}
				}
				if want == "succeeded" && receipt.ObjectDeletion != "confirmed" {
					t.Fatalf("object deletion=%s", raw)
				}
				if want == "succeeded" {
					if raw, failed := invoke(operatortool.ReadAttachmentMetadata, map[string]string{"project_id": project, "attachment_id": uploaded.ID}); !failed {
						t.Fatalf("deleted metadata read=%s", raw)
					}
				}
				before := len(store.requests())
				if raw, failed := invoke(operatortool.ActionResult, map[string]string{"action_id": receipt.Preview.ID}); failed || len(store.requests()) != before {
					t.Fatalf("result replay=%s touched storage=%v", raw, len(store.requests()) != before)
				}
			}
			store.mu.Lock()
			_, retained = store.objects[objectKey]
			store.mu.Unlock()
			if retained == (decision == "confirm" || decision == "yolo" || decision == "storage unavailable") {
				t.Fatalf("retained=%v decision=%s", retained, decision)
			}
		})
	}
}
