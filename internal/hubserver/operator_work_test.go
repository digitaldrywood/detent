package hubserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/conversation"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestHubMCPWorkCommands(t *testing.T) {
	t.Parallel()
	f := linkedFixture(t)
	scope := analyticsTestScope(t, f)
	updateProject := func(ctx context.Context, mutate func(*projectstore.Record)) error {
		return f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			mutate(&project)
			_, err = w.UpdateProjectRecord(ctx, project)
			return err
		})
	}
	setVisual := func(ctx context.Context, visual bool) error {
		return f.service.config.TenantRepository.WriteTenant(ctx, scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			project, err := w.ProjectRecord(ctx, scope.Project)
			if err != nil {
				return err
			}
			return w.UpdateProjectContent(ctx, scope.Project, project.Core.ContentSettingsRevision, visual)
		})
	}
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
	invoke := func(name, key string, fields map[string]any) (json.RawMessage, bool, error) {
		if fields["project_id"] == nil {
			fields["project_id"] = f.project.ID
		}
		if key != "" {
			fields["request_id"] = key
		}
		body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": fields, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}}
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, false, err
		}
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+f.token)
		request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
		request.Header.Set("Mcp-Method", "tools/call")
		request.Header.Set("Mcp-Name", name)
		response := httptest.NewRecorder()
		f.service.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			return nil, false, fmt.Errorf("MCP request status=%d: %s", response.Code, response.Body.String())
		}
		var envelope struct {
			Result struct {
				IsError bool            `json:"isError"`
				Content json.RawMessage `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			return nil, false, err
		}
		if name == operatortool.CreateChange || name == operatortool.PublishChangeVersion || envelope.Result.IsError {
			return envelope.Result.Content, envelope.Result.IsError, nil
		}
		var content struct {
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(envelope.Result.Content, &content); err != nil {
			return nil, false, err
		}
		return content.Data, envelope.Result.IsError, nil
	}
	call := func(t *testing.T, name, key string, fields map[string]any) (json.RawMessage, bool) {
		t.Helper()
		raw, failed, err := invoke(name, key, fields)
		if err != nil {
			t.Fatal(err)
		}
		return raw, failed
	}
	raw, failed := call(t, "file_issue", "create", map[string]any{"title": "Created", "description": "Keep body" + issueContractTestSections, "state": "Todo"})
	var created tracker.NativeIssue
	if err := json.Unmarshal(raw, &created); err != nil || failed || created.WorkItemID == "" {
		t.Fatalf("create=%s %v failed=%t", raw, err, failed)
	}
	var createdFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &createdFields); err != nil || string(createdFields["revision"]) != `"1"` {
		t.Fatalf("creation revision=%s err=%v", raw, err)
	}
	for _, tool := range []string{operatortool.OrderItem, operatortool.SetQueuePriority} {
		fields := map[string]any{"identifier": created.WorkItemID, "queue_scope": "project", "state": "Todo"}
		if tool == operatortool.OrderItem {
			fields["rank"] = "1"
		} else {
			fields["queue_priority"] = "high"
		}
		refused, failed := call(t, tool, "native-"+tool, fields)
		var refusal operatortool.RequestError
		if !failed || json.Unmarshal(refused, &refusal) != nil || refusal.Code != "invalid_request" || refusal.Message != "Queue ordering and priority overrides require a GitHub-compatible project; native projects use issue priority" {
			t.Fatalf("%s native refusal=%s failed=%t", tool, refused, failed)
		}
	}
	replay, failed := call(t, "file_issue", "create", map[string]any{"title": "Created", "description": "Keep body" + issueContractTestSections, "state": "Todo"})
	if failed || string(raw) != string(replay) {
		t.Fatalf("creation replay=%s", replay)
	}
	t.Run("concurrent replication writer", func(t *testing.T) {
		admin := nativeAttemptTestDatabase(t, f)
		var contended tracker.NativeIssue
		for _, tool := range []string{operatortool.FileIssue, operatortool.AddComment} {
			t.Run(tool, func(t *testing.T) {
				conn, err := admin.Conn(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				deadline, ok := t.Deadline()
				if !ok {
					t.Fatal("test deadline is required")
				}
				finished := make(chan struct{})
				started := false
				defer func() {
					ctx, cancel := context.WithDeadline(context.WithoutCancel(t.Context()), deadline)
					defer cancel()
					_, rollbackErr := conn.ExecContext(ctx, "ROLLBACK")
					if err := errors.Join(rollbackErr, conn.Close()); err != nil {
						t.Error(err)
					}
					if started {
						<-finished
					}
				}()
				if _, err := conn.ExecContext(t.Context(), "BEGIN"); err != nil {
					t.Fatal(err)
				}
				var locked tracker.ProjectID
				if err := conn.QueryRowContext(t.Context(), "SELECT id FROM projects WHERE organization_id=$1 AND id=$2 FOR UPDATE", scope.Organization, scope.Project).Scan(&locked); err != nil {
					t.Fatal(err)
				}
				var blockerPID int
				if err := conn.QueryRowContext(t.Context(), "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
					t.Fatal(err)
				}
				fields := map[string]any{"title": "Contended", "description": "Written while replication holds the lock" + issueContractTestSections, "state": "Todo"}
				if tool == operatortool.AddComment {
					fields = map[string]any{"identifier": contended.WorkItemID, "body": "Written while replication holds the lock"}
				}
				type result struct {
					raw    json.RawMessage
					failed bool
					err    error
				}
				results := make(chan result, 1)
				started = true
				go func() {
					defer close(finished)
					raw, failed, err := invoke(tool, "replication-contention-"+tool, fields)
					results <- result{raw, failed, err}
				}()
				blocked := false
				testenv.WaitUntil(t, t.Context(), func() error {
					select {
					case <-finished:
						return nil
					default:
					}
					if err := conn.QueryRowContext(t.Context(), "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))", blockerPID).Scan(&blocked); err != nil {
						return err
					}
					if blocked {
						return nil
					}
					return fmt.Errorf("%s has not reached the held project lock", tool)
				})
				if !blocked {
					observed := <-results
					if observed.err != nil {
						t.Fatal(observed.err)
					}
					t.Fatalf("%s did not wait on the held replication lock", tool)
				}
				if _, err := conn.ExecContext(t.Context(), "ROLLBACK"); err != nil {
					t.Fatal(err)
				}
				<-finished
				observed := <-results
				if observed.err != nil {
					t.Fatal(observed.err)
				}
				raw, failed := observed.raw, observed.failed
				if failed || !strings.Contains(string(raw), "Written while replication holds the lock") {
					t.Fatalf("%s under a held replication lock = %s failed=%t", tool, raw, failed)
				}
				if tool == operatortool.FileIssue {
					if err := json.Unmarshal(raw, &contended); err != nil || contended.WorkItemID == "" {
						t.Fatalf("contended issue = %s: %v", raw, err)
					}
				}
			})
		}
	})
	t.Run("reused attachment contract", func(t *testing.T) {
		record := attachment.Metadata{ID: conversation.NewAttachmentID(), ProjectID: string(f.project.ID), Uploader: scope.Credential.ID, Name: "file.txt", ContentType: "text/plain", Size: 5, SHA256: artifact.Digest([]byte("hello")), CreatedAt: time.Now().UTC()}
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAttachment(t.Context(), attachmentstore.Record{OrganizationID: scope.Organization, Kind: "attachment", Metadata: record})
		}); err != nil {
			t.Fatal(err)
		}
		body := record.Markdown(string(f.project.OrganizationID)) + issueContractTestSections
		var items []tracker.NativeIssue
		for _, key := range []string{"first-attachment", "second-attachment"} {
			fields := map[string]any{"title": key, "description": body, "state": "Todo"}
			raw, failed := call(t, operatortool.FileIssue, key, fields)
			var item tracker.NativeIssue
			if err := json.Unmarshal(raw, &item); err != nil || failed || item.WorkItemID == "" || item.Body != body {
				t.Fatalf("%s result=%s isError=%t err=%v", key, raw, failed, err)
			}
			items = append(items, item)
			replay, failed := call(t, operatortool.FileIssue, key, fields)
			if failed || string(replay) != string(raw) {
				t.Fatalf("%s replay=%s isError=%t", key, replay, failed)
			}
		}
		if items[0].WorkItemID == items[1].WorkItemID {
			t.Fatal("attachment reuse returned the first issue")
		}
		raw, failed := call(t, operatortool.AddComment, "attachment-comment", map[string]any{"identifier": items[1].WorkItemID, "body": body})
		var comment tracker.NativeComment
		if err := json.Unmarshal(raw, &comment); err != nil || failed || comment.ID == "" || comment.Body != body {
			t.Fatalf("comment result=%s isError=%t err=%v", raw, failed, err)
		}
		receiptBody := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.AddComment, "arguments": map[string]any{"project_id": f.project.ID, "request_id": "attachment-comment", "identifier": items[1].WorkItemID, "body": body}, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}}
		var receipt struct {
			Result struct {
				Content struct {
					CommentID string `json:"comment_id"`
					URL       string `json:"url"`
				} `json:"structuredContent"`
			} `json:"result"`
		}
		decodeHubResponse(t, performHubWorkCall(t, f.service, path, f.token, operatortool.AddComment, receiptBody), &receipt)
		if receipt.Result.Content.CommentID != comment.ID || !strings.HasSuffix(receipt.Result.Content.URL, "/comments#"+comment.ID) {
			t.Fatalf("add_comment receipt = %+v, want comment_id %s", receipt.Result.Content, comment.ID)
		}
		metadata, err := f.service.readCloudAttachment(t.Context(), scope, record.ID)
		if err != nil || len(metadata.ReferencedBy) != 3 {
			t.Fatalf("references=%v err=%v", metadata.ReferencedBy, err)
		}
		for _, ref := range []attachment.SourceReference{
			{WorkItemID: string(items[0].WorkItemID)},
			{WorkItemID: string(items[1].WorkItemID)},
			{WorkItemID: string(items[1].WorkItemID), CommentID: comment.ID},
		} {
			if !slices.Contains(metadata.ReferencedBy, ref) {
				t.Fatalf("missing reference=%v in %v", ref, metadata.ReferencedBy)
			}
		}
	})
	for rank := 1; rank <= 4; rank++ {
		fields := map[string]any{"title": "Priority", "description": "Exact rank" + issueContractTestSections, "priority": rank}
		raw, failed := call(t, "file_issue", "rank-"+strconv.Itoa(rank), fields)
		var item tracker.NativeIssue
		if err := json.Unmarshal(raw, &item); err != nil || failed || item.Priority == nil || *item.Priority != rank-1 || item.State != "Todo" {
			t.Fatalf("rank %d=%s %v", rank, raw, err)
		}
	}
	largeSnapshot := linkedSnapshot()
	largeSnapshot.Body = strings.Repeat("source body ", 20000)
	sourceComment := largeSnapshot.Comments[0]
	largeSnapshot.Comments = nil
	for index := range 6 {
		sourceComment.Body = strings.Repeat("source comment ", 4000)
		sourceComment.Provenance.ExternalID = "comment-" + strconv.Itoa(index)
		largeSnapshot.Comments = append(largeSnapshot.Comments, sourceComment)
	}
	f.service.config.ImportBackend = linkedTestImporter{snapshot: largeSnapshot}
	linkedArgs := map[string]any{"github_issue_url": largeSnapshot.URL, "state": "Todo"}
	linkedRaw, failed := call(t, "file_issue", "linked", linkedArgs)
	var linked tracker.NativeIssue
	if err := json.Unmarshal(linkedRaw, &linked); err != nil || failed || linked.State != "Triage" || linked.LinkedSource == nil || linked.LinkedSource.URL != "https://github.com/acme/orders/issues/12" {
		t.Fatalf("linked creation=%s %v", linkedRaw, err)
	}
	for _, key := range []string{"linked", "duplicate-link"} {
		replay, failed := call(t, "file_issue", key, linkedArgs)
		if failed || string(replay) != string(linkedRaw) {
			t.Fatalf("linked retry=%s", replay)
		}
	}
	for _, tool := range []string{operatortool.WorkItem, operatortool.EditItem} {
		fields := map[string]any{"reference": linked.WorkItemID}
		key := ""
		if tool == operatortool.EditItem {
			fields = map[string]any{"identifier": linked.WorkItemID, "expected_revision": strconv.FormatInt(int64(linked.Revision), 10), "title": "Updated large item"}
			key = "large-edit"
		}
		bounded, failed := call(t, tool, key, fields)
		var item struct {
			tracker.NativeIssue
			ContentRead struct {
				TruncatedFields []string `json:"truncated_fields"`
				FullURL         string   `json:"full_url"`
			} `json:"content_read"`
		}
		if failed || json.Unmarshal(bounded, &item) != nil || item.WorkItemID != linked.WorkItemID || item.State != "Triage" || item.Revision < linked.Revision || len(item.ContentRead.TruncatedFields) == 0 || item.ContentRead.FullURL == "" {
			t.Fatalf("large %s failed=%t bytes=%d", tool, failed, len(bounded))
		}
		response := performHubAPIRequest(t, f.service, http.MethodGet, item.ContentRead.FullURL, f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var full tracker.NativeIssue
		decodeHubResponse(t, response, &full)
		if full.Body != largeSnapshot.Body || full.LinkedSource.Snapshot.Body != largeSnapshot.Body {
			t.Fatal("REST content was truncated by MCP shaping")
		}
		if tool == operatortool.WorkItem {
			remaining := map[string]string{}
			for _, comment := range largeSnapshot.Comments {
				remaining[comment.Provenance.ExternalID] = comment.Body
			}
			commentsURL := item.ContentRead.FullURL + "/comments"
			for nextURL := commentsURL; ; {
				response := performHubAPIRequest(t, f.service, http.MethodGet, nextURL, f.token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var comments tracker.Page[tracker.NativeComment]
				decodeHubResponse(t, response, &comments)
				if len(comments.Items) == 0 {
					t.Fatal("imported comments page was empty")
				}
				for _, comment := range comments.Items {
					body, ok := remaining[comment.Provenance.ExternalID]
					if !ok {
						t.Fatal("REST comment was unexpected or duplicated")
					}
					if comment.Body != body {
						t.Fatal("REST comment content was truncated")
					}
					delete(remaining, comment.Provenance.ExternalID)
				}
				if comments.NextCursor == "" {
					break
				}
				nextURL = commentsURL + "?cursor=" + url.QueryEscape(comments.NextCursor)
			}
			if len(remaining) != 0 {
				t.Fatal("imported comments were lost")
			}
		}
	}
	f.service.config.ImportBackend = linkedTestImporter{snapshot: linkedSnapshot()}
	if _, failed := call(t, "file_issue", "linked", map[string]any{"github_issue_url": "github.com/acme/orders/issues/13", "state": "Todo"}); !failed {
		t.Fatal("changed linked creation retry was accepted")
	}
	for _, link := range []string{"https://github.com/acme/private/issues/12", "https://github.com/acme/orders/pull/12", "https://example.com/acme/orders/issues/12"} {
		if _, failed := call(t, "file_issue", link, map[string]any{"github_issue_url": link}); !failed {
			t.Fatalf("foreign linkage accepted: %s", link)
		}
	}
	t.Run("repository association diagnostics", func(t *testing.T) {
		if err := updateProject(t.Context(), func(project *projectstore.Record) {
			project.Settings.Repository, project.Settings.CheckoutRepository = "", ""
			project.Settings.RepositoryEnabled = false
		}); err != nil {
			t.Fatal(err)
		}
		raw, failed := call(t, "file_issue", "unbound-link", map[string]any{"github_issue_url": "github.com/acme/orders/issues/13", "state": "Todo"})
		var refusal struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || !failed || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, "get_project_integration") || !strings.Contains(refusal.Message, "bind_native_repository") {
			t.Fatalf("missing association refusal=%s failed=%t err=%v", raw, failed, err)
		}
		raw, failed = call(t, "file_issue", "unbound-native", map[string]any{"description": nativeContractTestBody, "title": "Native without repository", "state": "Todo"})
		var native tracker.NativeIssue
		if err := json.Unmarshal(raw, &native); err != nil || failed || native.LinkedSource != nil || native.Body != nativeContractTestBody || native.Number != linked.Number+1 {
			t.Fatalf("unbound native creation=%s failed=%t err=%v", raw, failed, err)
		}
		if err := updateProject(t.Context(), func(project *projectstore.Record) {
			project.Settings.CheckoutRepository = "acme/orders"
			project.States = append(project.States, tracker.NativeState{Name: "Backlog", Transitions: []string{"Todo"}})
		}); err != nil {
			t.Fatal(err)
		}
		args := map[string]any{"github_issue_url": "github.com/Acme/Orders/issues/13", "state": "Backlog"}
		snapshot := linkedSnapshot()
		snapshot.URL = "https://github.com/acme/orders/issues/13"
		snapshot.Provenance.ExternalID = "I_13"
		snapshot.Comments[0].Provenance.ExternalID = "C_13"
		f.service.config.ImportBackend = linkedTestImporter{snapshot: snapshot}
		raw, failed = call(t, "file_issue", "checkout-link", args)
		var item tracker.NativeIssue
		if err := json.Unmarshal(raw, &item); err != nil || failed || item.State != "Triage" || item.LinkedSource == nil || item.LinkedSource.Status != "complete" {
			t.Fatalf("checkout link=%s failed=%t err=%v", raw, failed, err)
		}
		for _, key := range []string{"checkout-link", "checkout-duplicate"} {
			if replay, failed := call(t, "file_issue", key, args); failed || string(raw) != string(replay) {
				t.Fatalf("checkout replay=%s failed=%t", replay, failed)
			}
		}
		raw, failed = call(t, "file_issue", "checkout-foreign", map[string]any{"github_issue_url": "github.com/acme/private/issues/13", "state": "Backlog"})
		if err := json.Unmarshal(raw, &refusal); err != nil || !failed || refusal.Code != "invalid_request" || refusal.Message != "GitHub issue must belong to this native project's attached repository" {
			t.Fatalf("foreign association refusal=%s failed=%t err=%v", raw, failed, err)
		}
	})
	for _, test := range []struct {
		name, state string
		failed      bool
	}{
		{"default with Triage first", "", false},
		{"explicit unlinked Triage", "Triage", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, failed := call(t, "file_issue", test.name, map[string]any{"description": nativeContractTestBody, "title": test.name, "state": test.state})
			if failed != test.failed {
				t.Fatalf("creation=%s failed=%t", raw, failed)
			}
			if failed {
				return
			}
			var item tracker.NativeIssue
			if err := json.Unmarshal(raw, &item); err != nil || item.State != "Todo" || item.LinkedSource != nil {
				t.Fatalf("creation=%s err=%v", raw, err)
			}
		})
	}
	id := string(created.WorkItemID)
	identifier := string(f.project.ID) + "#" + strconv.Itoa(created.Number)
	for _, test := range []struct {
		name, tool, message string
		fields              map[string]any
	}{
		{"missing revision", operatortool.MoveItem, "expected_revision: is required", map[string]any{"target_state": "Todo"}},
		{"rejected revision", operatortool.MoveItem, "expected_revision: must match pattern ^[1-9][0-9]*$", map[string]any{"target_state": "Todo", "expected_revision": "0"}},
		{"missing body", operatortool.AddComment, "body: is required", map[string]any{}},
		{"rejected target", operatortool.AddComment, "target: must be one of: issue, pr", map[string]any{"body": "hello", "target": "other"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.fields["identifier"] = identifier
			raw, failed := call(t, test.tool, test.name, test.fields)
			var detail operatortool.RequestError
			if err := json.Unmarshal(raw, &detail); err != nil || !failed || detail.Code != "invalid_request" || detail.Message != test.message {
				t.Fatalf("validation = %s, failed=%t, err=%v", raw, failed, err)
			}
		})
	}
	commentBody := strings.Repeat("Discuss `approval` at /chat/approval. ", 25)
	var comment tracker.NativeComment
	raw, failed = call(t, "add_comment", "comment", map[string]any{"identifier": identifier, "body": commentBody, "target": "issue"})
	if err := json.Unmarshal(raw, &comment); err != nil || failed || comment.ID == "" || comment.Body != commentBody {
		t.Fatalf("comment=%s %v", raw, err)
	}
	var commentFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &commentFields); err != nil || string(commentFields["revision"]) != `"1"` {
		t.Fatalf("comment revision=%s err=%v", raw, err)
	}
	replay, failed = call(t, "add_comment", "comment", map[string]any{"identifier": id, "body": commentBody, "target": "issue"})
	if failed || string(raw) != string(replay) {
		t.Fatal("comment replay changed identity")
	}
	if _, failed := call(t, "add_comment", "comment", map[string]any{"identifier": identifier, "body": "Changed retry"}); !failed {
		t.Fatal("changed comment retry was accepted")
	}
	t.Run("visual reference refusals", func(t *testing.T) {
		if err := setVisual(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
			defer cancel()
			if err := setVisual(ctx, false); err != nil {
				t.Error(err)
			}
		})
		const visual = "https://figma.com/design/example?token=private"
		for _, test := range []struct {
			tool   string
			fields map[string]any
		}{
			{operatortool.FileIssue, map[string]any{"title": "Visual", "description": visual + issueContractTestSections, "state": "Todo"}},
			{operatortool.CreateChange, map[string]any{"work_item_id": id, "title": "Visual", "body": visual}},
			{operatortool.EditItem, map[string]any{"identifier": id, "body": visual + issueContractTestSections, "expected_revision": createdFields["revision"]}},
			{operatortool.AddComment, map[string]any{"identifier": id, "body": visual}},
			{operatortool.EditComment, map[string]any{"identifier": id, "comment_id": comment.ID, "body": visual, "expected_revision": commentFields["revision"]}},
		} {
			raw, failed := call(t, test.tool, "visual-"+test.tool, test.fields)
			var refusal operatortool.RequestError
			if !failed || json.Unmarshal(raw, &refusal) != nil || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, attachment.VisualReferenceMessage) || strings.Contains(refusal.Message, "private") {
				t.Fatalf("%s refusal=%s failed=%v", test.tool, raw, failed)
			}
		}
	})
	for _, tt := range []struct {
		name, tool string
		fields     map[string]any
		failed     bool
	}{
		{"edit", "edit_item", map[string]any{"title": "Edited", "priority": 2, "expected_revision": createdFields["revision"]}, false},
		{"stale", "edit_item", map[string]any{"title": "Stale", "expected_revision": "1"}, true},
		{"direct body replacement", "edit_item", map[string]any{"body": nativeContractTestBody, "expected_revision": 2}, false},
		{"comment edit", "edit_comment", map[string]any{"comment_id": comment.ID, "body": "Edited comment", "expected_revision": commentFields["revision"]}, false},
		{"stale comment", "edit_comment", map[string]any{"comment_id": comment.ID, "body": "Stale", "expected_revision": "1"}, true},
		{"foreign item", "add_comment", map[string]any{"identifier": "wi_foreign", "body": "Foreign"}, true},
		{"foreign project prefix", "add_comment", map[string]any{"identifier": "prj_foreign#" + strconv.Itoa(created.Number), "body": "Foreign"}, true},
		{"no native delete", "delete_comment", map[string]any{"comment_id": comment.ID}, true},
		{"no hub lane writer", "move_item", map[string]any{"target_state": "Todo", "expected_revision": "2"}, true},
		{"no hub archive approval", "archive_item", map[string]any{"expected_revision": "2"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fields["identifier"] == nil {
				tt.fields["identifier"] = identifier
			}
			_, failed := call(t, tt.tool, tt.name, tt.fields)
			if failed != tt.failed {
				t.Fatalf("failed=%t want=%t", failed, tt.failed)
			}
		})
	}
	current := readWorkItem(t, f, created.WorkItemID, "")
	if current.Title != "Edited" || current.Revision != 3 || current.Body != nativeContractTestBody || current.Priority == nil || *current.Priority != 2 {
		t.Fatalf("mutation result=%+v", current)
	}
	for _, reference := range []string{identifier, strconv.Itoa(created.Number), id} {
		raw, failed = call(t, "list_comments", "", map[string]any{"identifier": reference, "limit": 1})
		var comments tracker.Page[tracker.NativeComment]
		if err := json.Unmarshal(raw, &comments); err != nil || failed || len(comments.Items) != 1 || comments.Items[0].Body != "Edited comment" || comments.NextCursor != "" {
			t.Fatalf("discussion=%s %v", raw, err)
		}
	}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	raw, failed = call(t, operatortool.CreateChange, "mcp-change", map[string]any{"work_item_id": id, "title": "Operator source"})
	var change operatortool.ChangeResult
	if err := json.Unmarshal(raw, &change); err != nil || failed || change.ChangeID == "" {
		t.Fatalf("MCP Change creation=%s %v", raw, err)
	}
	input := changeTestInput()
	sourceDir := t.TempDir()
	gitAt := func(directory string, args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("source fixture git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	git := func(args ...string) string { return gitAt(sourceDir, args...) }
	git("init", "-b", "main")
	sourcePath := filepath.Join(sourceDir, "source.txt")
	if err := os.WriteFile(sourcePath, []byte("base source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "base")
	input.BaseSHA = git("rev-parse", "HEAD")
	remote := filepath.Join(t.TempDir(), "remote.git")
	git("clone", "--bare", sourceDir, remote)
	if err := os.WriteFile(sourcePath, []byte("operator source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "operator work")
	input.HeadSHA, input.MergeBaseSHA = git("rev-parse", "HEAD"), input.BaseSHA
	capture, err := workspace.CaptureChangeSource(t.Context(), sourceDir, input.BaseSHA, input.HeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	bundlePath := filepath.Join(t.TempDir(), "operator.bundle")
	git("bundle", "create", bundlePath, "main", "^"+input.BaseSHA)
	capture.Bundle, err = os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	capture.Source.BundleSHA256 = tracker.ChangeSourceDigest(capture.Bundle)
	capture.Source.Bytes = int64(len(capture.Bundle))
	input.Code.Availability = "unverified"
	fields := map[string]any{"work_item_id": id, "change_id": change.ChangeID, "expected_version_id": "", "base_sha": input.BaseSHA, "head_sha": input.HeadSHA, "merge_base_sha": input.MergeBaseSHA, "repository": input.Repository, "policy_id": input.PolicyID, "code": input.Code, "artifacts": input.Artifacts, "source_capture": capture.Source, "source_bundle": capture.Bundle}
	sourceOther := newChangeFixture(t, f.service)
	foreignArguments := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		foreignArguments[key] = value
	}
	foreignArguments["project_id"], foreignArguments["request_id"] = f.project.ID, "foreign-source-tenant"
	foreignCall := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.PublishChangeVersion, "arguments": foreignArguments, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}}
	foreignResponse := performHubWorkCall(t, f.service, path, sourceOther.token, operatortool.PublishChangeVersion, foreignCall)
	requireNativeStatus(t, foreignResponse, http.StatusOK)
	var foreignResult struct {
		Result struct {
			IsError bool `json:"isError"`
		} `json:"result"`
	}
	decodeHubResponse(t, foreignResponse, &foreignResult)
	if !foreignResult.Result.IsError {
		t.Fatal("foreign tenant source publication succeeded")
	}
	setSourceBundle := func(bundle []byte) func(map[string]any) {
		return func(arguments map[string]any) {
			metadata := capture.Source
			metadata.BundleSHA256 = tracker.ChangeSourceDigest(bundle)
			metadata.Bytes = int64(len(bundle))
			arguments["source_capture"], arguments["source_bundle"] = metadata, bundle
		}
	}
	for _, denied := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"source additional refs", setSourceBundle(bytes.Replace(capture.Bundle, []byte("\n\n"), []byte("\n"+input.HeadSHA+" HEAD\n\n"), 1))},
		{"source wrong advertised head", setSourceBundle(bytes.Replace(capture.Bundle, []byte(input.HeadSHA+" refs/heads/main"), []byte(input.BaseSHA+" refs/heads/main"), 1))},
		{"source missing advertised head", setSourceBundle(bytes.Replace(capture.Bundle, []byte(input.HeadSHA+" refs/heads/main\n"), nil, 1))},
		{"source malformed bundle", setSourceBundle([]byte("sentinel-private-source"))},
		{"source invalid pack", setSourceBundle(append(bytes.Clone(bytes.SplitN(capture.Bundle, []byte("\n\n"), 2)[0]), []byte("\n\nretained source")...))},
		{"source truncated pack", setSourceBundle(capture.Bundle[:len(capture.Bundle)-1])},
		{"source bundle inflation budget", setSourceBundle(append(append(bytes.Clone(bytes.SplitN(capture.Bundle, []byte("\n\n"), 2)[0]), '\n', '\n'), changeTestPack(t, 1, tracker.MaxChangeDiffBytes+1, nil, 1)...))},
		{"source pack missing reviewed commit", func(a map[string]any) {
			fakeHead := strings.Repeat("f", 40)
			changed := bytes.Replace(capture.Bundle, []byte(input.HeadSHA+" refs/heads/main"), []byte(fakeHead+" refs/heads/main"), 1)
			setSourceBundle(changed)(a)
			metadata := a["source_capture"].(tracker.ChangeSource)
			metadata.HeadSHA = fakeHead
			a["source_capture"], a["head_sha"] = metadata, fakeHead
		}},
		{"source unsupported prerequisite", setSourceBundle(bytes.Replace(capture.Bundle, []byte("\n-"+input.BaseSHA+" "), []byte("\n-"+strings.Repeat("f", 40)+" "), 1))},
		{"source unsupported capability", setSourceBundle(bytes.Replace(capture.Bundle, []byte("# v2 git bundle\n"), []byte("# v3 git bundle\n@filter=blob:none\n"), 1))},
		{"missing source metadata", func(a map[string]any) { delete(a, "source_capture") }},
		{"missing source bytes", func(a map[string]any) { delete(a, "source_bundle") }},
		{"source digest", func(a map[string]any) {
			s := capture.Source
			s.BundleSHA256 = strings.Repeat("f", 64)
			a["source_capture"] = s
		}},
		{"source size", func(a map[string]any) { s := capture.Source; s.Bytes++; a["source_capture"] = s }},
		{"source base", func(a map[string]any) {
			s := capture.Source
			s.BaseSHA = strings.Repeat("f", 40)
			a["source_capture"] = s
		}},
		{"source head", func(a map[string]any) {
			s := capture.Source
			s.HeadSHA = strings.Repeat("f", 40)
			a["source_capture"] = s
		}},
		{"source diff digest", func(a map[string]any) { s := capture.Source; s.DiffSHA256 = "invalid"; a["source_capture"] = s }},
		{"source transport bound", func(a map[string]any) { a["source_bundle"] = bytes.Repeat([]byte("x"), operatortool.MaxArgumentBytes) }},
		{"source unreported configuration", func(a map[string]any) { a["policy_id"] = "policy_stale" }},
		{"source stale current version", func(a map[string]any) { a["expected_version_id"] = "version_stale" }},
		{"source foreign project", func(a map[string]any) { a["project_id"] = sourceOther.project.ID }},
		{"source foreign item", func(a map[string]any) { a["work_item_id"] = sourceOther.issue.WorkItemID }},
		{"source foreign change", func(a map[string]any) { a["change_id"] = sourceOther.change.ID }},
	} {
		t.Run(denied.name, func(t *testing.T) {
			arguments := make(map[string]any, len(fields))
			for key, value := range fields {
				arguments[key] = value
			}
			denied.edit(arguments)
			if denied.name == "source transport bound" {
				arguments["project_id"], arguments["request_id"] = f.project.ID, denied.name
				body := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.PublishChangeVersion, "arguments": arguments, "_meta": foreignCall["params"].(map[string]any)["_meta"]}}
				response := performHubWorkCall(t, f.service, path, f.token, operatortool.PublishChangeVersion, body)
				requireNativeStatus(t, response, http.StatusBadRequest)
				var envelope struct {
					Error struct {
						Code int `json:"code"`
					} `json:"error"`
				}
				decodeHubResponse(t, response, &envelope)
				if envelope.Error.Code != -32602 {
					t.Fatal("oversized source arguments escaped the MCP boundary")
				}
				return
			}
			raw, failed := call(t, operatortool.PublishChangeVersion, denied.name, arguments)
			if !failed {
				t.Fatal("invalid source publication succeeded")
			}
			if strings.Contains(denied.name, "advertised") || denied.name == "source additional refs" || strings.Contains(denied.name, "bundle") || strings.Contains(denied.name, "pack") || strings.Contains(denied.name, "unsupported") {
				if bytes.Contains(raw, []byte("sentinel-private-source")) {
					t.Fatalf("source refusal disclosed private bundle content: %s", raw)
				}
				if !bytes.Contains(raw, []byte("git bundle create")) || strings.Contains(denied.name, "budget") && !bytes.Contains(raw, []byte("verification budget")) {
					t.Fatalf("source refusal lacks actionable bundle guidance: %s", raw)
				}
			}
		})
	}
	response := performHubAPIRequest(t, f.service, http.MethodGet, change.URL, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var unpublished tracker.ChangeDetail
	decodeHubResponse(t, response, &unpublished)
	if len(unpublished.Versions) != 0 {
		t.Fatal("source refusals published a version")
	}
	raw, failed = call(t, operatortool.PublishChangeVersion, "mcp-publication", fields)
	var published operatortool.ChangeResult
	if err := json.Unmarshal(raw, &published); err != nil || failed || published.Version == nil || published.Detail == nil || published.Detail.Change.CurrentVersion != published.Version.ID || published.WorkItemState != current.State || published.Version.RunID != "" || published.Version.AttemptID != "" {
		t.Fatalf("MCP operator publication=%s %v", raw, err)
	}
	if published.Version.Source == nil || *published.Version.Source != capture.Source || published.Version.Policy.ID != input.PolicyID || published.Version.Actor.Kind != "human" || published.Version.Code.Availability != "unverified" {
		t.Fatal("MCP source publication changed source, policy or operator provenance")
	}
	t.Run("Change visual reference refusals", func(t *testing.T) {
		if err := setVisual(t.Context(), true); err != nil {
			t.Fatal(err)
		}

		visualVersion := make(map[string]any, len(fields))
		for key, value := range fields {
			visualVersion[key] = value
		}
		visualVersion["expected_version_id"] = published.Version.ID
		visualVersion["artifacts"] = []tracker.ChangeArtifact{{Kind: "artifact", URI: "https://example.test/screen.png", SHA256: input.Code.SHA256, Availability: "available"}}
		for _, test := range []struct {
			tool   string
			fields map[string]any
		}{
			{operatortool.PublishChangeVersion, visualVersion},
			{operatortool.DiscussChange, map[string]any{"work_item_id": id, "change_id": change.ChangeID, "body": "https://figma.com/design/example?secret=hidden"}},
			{operatortool.ReviewChange, map[string]any{"work_item_id": id, "change_id": change.ChangeID, "version_id": published.Version.ID, "expected_revision": published.Detail.Change.Revision, "decision": "commented", "body": "https://figma.com/design/example?secret=hidden"}},
		} {
			raw, failed := call(t, test.tool, "visual-"+test.tool, test.fields)
			var refusal operatortool.RequestError
			if !failed || json.Unmarshal(raw, &refusal) != nil || refusal.Code != "invalid_request" || !strings.Contains(refusal.Message, attachment.VisualReferenceMessage) || strings.Contains(refusal.Message, "hidden") {
				t.Fatalf("%s refusal=%s failed=%v", test.tool, raw, failed)
			}
		}
		if err := setVisual(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	})

	sourceURL := change.URL + "/versions/" + published.Version.ID + "/source"
	response = performHubAPIRequest(t, f.service, http.MethodGet, sourceURL, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), capture.Bundle) {
		t.Fatal("MCP publication did not retain the genuine Git bundle")
	}
	fresh := filepath.Join(t.TempDir(), "fresh")
	git("clone", "--no-local", remote, fresh)
	gitAt(fresh, "remote", "set-url", "origin", input.Repository)
	gitAt(fresh, "config", "url."+remote+".insteadOf", input.Repository)
	backend, err := workspace.NewBackend(workspace.KindLocalGit, workspace.LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: fresh, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	restored, err := backend.Create(t.Context(), workspace.Issue{Identifier: "native#1", NativeRework: true, BaseRef: input.BaseSHA, Source: &workspace.ChangeSource{Version: *published.Version, Bundle: response.Body.Bytes()}})
	if err != nil {
		t.Fatalf("import accepted operator source: %v", err)
	}
	if got := gitAt(restored.Path, "rev-parse", "HEAD"); got != input.HeadSHA {
		t.Fatalf("recovered head=%s want=%s", got, input.HeadSHA)
	}
	if content, err := os.ReadFile(filepath.Join(restored.Path, "source.txt")); err != nil || string(content) != "operator source\n" {
		t.Fatalf("recovered source=%q: %v", content, err)
	}
	foreignSource := sourceOther.path + "/versions/" + published.Version.ID + "/source"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreignSource, sourceOther.token, nil), http.StatusNotFound)
	replay, failed = call(t, operatortool.PublishChangeVersion, "mcp-publication", fields)
	var sourceReplay operatortool.ChangeResult
	if failed || json.Unmarshal(replay, &sourceReplay) != nil || sourceReplay.Version == nil || sourceReplay.Version.ID != published.Version.ID || !bytes.Equal(sourceReplay.Receipt, published.Receipt) {
		t.Fatal("source retry changed the immutable publication receipt")
	}
	input.Source = &capture.Source
	response = performHubAPIRequest(t, f.service, http.MethodPost, change.URL+"/versions", f.token, tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "mcp-publication"}, ChangeVersionInput: input, SourceBundle: capture.Bundle})
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(bytes.TrimSpace(response.Body.Bytes()), published.Receipt) {
		t.Fatal("HTTP and MCP source publication do not share the command receipt")
	}
	if err := updateProject(t.Context(), func(project *projectstore.Record) {
		for i := range project.States {
			if project.States[i].Name == "In Progress" {
				project.States[i].OperatorOnly = true
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	raw, failed = call(t, operatortool.WorkList, "", map[string]any{"query": "Edited"})
	var listed struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil || failed || len(listed.Items) != 1 || string(listed.Items[0]["revision"]) != `"3"` {
		t.Fatalf("listed revision=%s failed=%t err=%v", raw, failed, err)
	}
	move := map[string]any{"project_id": f.project.ID, "request_id": "worker-move", "identifier": id, "expected_revision": listed.Items[0]["revision"], "target_state": "In Progress"}
	worker := f.worker(t, "workflow-worker")
	response = performHubWorkCall(t, f.service, path, worker, operatortool.MoveItem, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operatortool.MoveItem, "arguments": move}})
	requireNativeStatus(t, response, http.StatusForbidden)
	if observed := readWorkItem(t, f, created.WorkItemID, ""); observed.State != current.State || observed.Revision != current.Revision {
		t.Fatal("worker request changed workflow")
	}
	largeMoveBody := strings.Repeat("<", 60000)
	response = performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+id, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "large-move-body"}, ExpectedRevision: current.Revision, Body: &largeMoveBody})
	requireNativeStatus(t, response, http.StatusOK)
	var bodyUpdated tracker.NativeIssue
	decodeHubResponse(t, response, &bodyUpdated)
	if bodyUpdated.Revision != current.Revision+1 || bodyUpdated.Body != largeMoveBody {
		t.Fatalf("large move setup=%+v", bodyUpdated)
	}
	for _, key := range []string{"operator-move", "operator-move"} {
		raw, failed = call(t, operatortool.MoveItem, key, map[string]any{"identifier": id, "expected_revision": bodyUpdated.Revision, "target_state": "In Progress"})
		var moved tracker.NativeIssue
		if err := json.Unmarshal(raw, &moved); err != nil || failed || moved.State != "In Progress" || moved.Revision != bodyUpdated.Revision+1 {
			t.Fatalf("Hub workflow command=%s %v", raw, err)
		}
		if !strings.Contains(string(raw), `"content_read"`) || readWorkItem(t, f, created.WorkItemID, "").Body != largeMoveBody {
			t.Fatal("large move receipt lost its marker or truncated stored body")
		}
	}

	blocker := f.create(t, "blocker")
	for _, dependency := range []struct {
		identifier, related string
		revision            any
	}{
		{identifier, string(f.project.ID) + "#" + strconv.Itoa(blocker.Number), 5},
		{id, string(blocker.WorkItemID), json.RawMessage(`5.0`)},
	} {
		raw, failed = call(t, "set_dependency", "dependency", map[string]any{"identifier": dependency.identifier, "related": dependency.related, "operation": "add", "expected_revision": dependency.revision})
		var item tracker.NativeIssue
		if err := json.Unmarshal(raw, &item); err != nil || failed || item.Revision != 6 || len(item.Dependencies) != 1 || item.Dependencies[0] != blocker.WorkItemID {
			t.Fatalf("dependency alias/replay=%s %v", raw, err)
		}
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "ungranted")
	foreign := other.create(t, "foreign")
	for _, related := range []string{"prj_foreign#" + strconv.Itoa(blocker.Number), string(foreign.WorkItemID)} {
		if _, failed := call(t, "set_dependency", "foreign-"+related, map[string]any{"identifier": identifier, "related": related, "operation": "add", "expected_revision": "6"}); !failed {
			t.Fatalf("foreign dependency accepted: %s", related)
		}
	}
	if current := readWorkItem(t, f, created.WorkItemID, ""); current.Revision != 6 || len(current.Dependencies) != 1 {
		t.Fatalf("foreign dependency changed item=%+v", current)
	}
	for _, test := range []struct {
		name, reference string
		code            string
	}{
		{"stale revision", identifier, "revision_conflict"},
		{"foreign stale revision", string(foreign.WorkItemID), ""},
		{"foreign alias stale revision", "prj_foreign#" + strconv.Itoa(created.Number), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, failed := call(t, operatortool.EditItem, test.name, map[string]any{"identifier": test.reference, "title": "Correction", "expected_revision": "5"})
			var conflict operatortool.ConflictError
			if !failed {
				t.Fatal("stale edit succeeded")
			}
			if test.code != "" {
				if err := json.Unmarshal(raw, &conflict); err != nil || conflict.Code != test.code || conflict.CurrentRevision != 6 {
					t.Fatalf("conflict=%s %v", raw, err)
				}
			} else if len(raw) != 0 {
				t.Fatalf("foreign context leaked: %s", raw)
			}
		})
	}
	for range 2 {
		raw, failed := call(t, operatortool.EditItem, "fresh correction", map[string]any{"identifier": identifier, "title": "Correction", "expected_revision": "6"})
		var corrected tracker.NativeIssue
		if err := json.Unmarshal(raw, &corrected); err != nil || failed || corrected.Revision != 7 || corrected.Title != "Correction" {
			t.Fatalf("fresh edit/replay=%s %v", raw, err)
		}
	}
	t.Run("large body correction", func(t *testing.T) {
		suffix := issueContractTestSections + "\n\nPreserve the final report entry.\n"
		prefix := "Old finding.\n" + strings.Repeat("Archived report text.\n", 1800)
		originalBody := prefix + strings.Repeat("x", 42917-len(prefix)-len(suffix)) + suffix
		correctedBody := "New finding.\n" + originalBody[len("Old finding.\n"):]
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items", f.token, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "large-body-create"}, Title: "Large report", Body: originalBody, State: "Todo"})
		requireNativeStatus(t, response, http.StatusOK)
		var large tracker.NativeIssue
		decodeHubResponse(t, response, &large)
		for range 2 {
			raw, failed := call(t, operatortool.EditItem, "large-body-edit", map[string]any{"identifier": large.WorkItemID, "body": correctedBody, "expected_revision": "1"})
			var edited tracker.NativeIssue
			if err := json.Unmarshal(raw, &edited); err != nil || failed || edited.Body != correctedBody || edited.Revision != 2 {
				t.Fatalf("large body correction bytes=%d revision=%d failed=%t err=%v", len(edited.Body), edited.Revision, failed, err)
			}
		}
		for _, test := range []struct {
			name      string
			reference tracker.NativeWorkItemID
		}{
			{"large stale revision", large.WorkItemID},
			{"large foreign project", foreign.WorkItemID},
		} {
			raw, failed := call(t, operatortool.EditItem, test.name, map[string]any{"identifier": test.reference, "body": originalBody, "expected_revision": "1"})
			if !failed {
				t.Fatalf("%s accepted", test.name)
			}
			if test.reference == large.WorkItemID {
				var conflict operatortool.ConflictError
				if err := json.Unmarshal(raw, &conflict); err != nil || conflict.Code != "revision_conflict" || conflict.CurrentRevision != 2 {
					t.Fatalf("large stale revision conflict=%s err=%v", raw, err)
				}
			} else if len(raw) != 0 {
				t.Fatal("large foreign edit leaked context")
			}
		}
		current := readWorkItem(t, f, large.WorkItemID, "")
		if current.Body != correctedBody || current.Revision != 2 || current.IssueContract == nil || !maps.Equal(current.IssueContract.ConfirmedSections, large.IssueContract.ConfirmedSections) {
			t.Fatal("refused edits or replay changed large body, revision or confirmation")
		}
		if current := readWorkItem(t, other, foreign.WorkItemID, ""); current.Body != foreign.Body || current.Revision != foreign.Revision {
			t.Fatal("large edit changed foreign issue")
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(large.WorkItemID)+"/versions/1", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var original tracker.NativeIssue
		decodeHubResponse(t, response, &original)
		if original.Body != originalBody {
			t.Fatal("large correction changed original report history")
		}
	})
}

// GitHub-compatible relationships, ordering and priority use the same commands
// as the hub dashboard, while lane mutations stay unavailable without their owner.
func TestHubMCPCompatibilityCommands(t *testing.T) {
	t.Parallel()
	const nativeID = "wi_compatible_first"
	const related = "wi_compatible_second"
	newFixture := func(t *testing.T, sourceRepository, integrationRepository string, enabled bool) (nativeFixture, tracker.ProjectID) {
		t.Helper()
		f := newNativeFixture(t, nil, "", "compatibility authority")
		service := f.service
		organization := f.project.OrganizationID
		project := tracker.ProjectID(hubapi.NewID("prj"))
		if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			stored, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: project, Name: "Compatible project", Profile: "github_compatible", RequireDependencies: true}, States: nativeFixtureStates()})
			if err != nil {
				return err
			}
			stored.Settings.Repository, stored.Settings.CheckoutRepository, stored.Settings.RepositoryEnabled = integrationRepository, integrationRepository, enabled
			_, err = w.UpdateProjectRecord(t.Context(), stored)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		source := newCoreNeutralFixture(t, organization, "issues", "workflow_states", "repositories", "queue_entries", "native_comments", "collaboration_versions", "collaboration_events", "issue_dependencies", "change_content_objects", "attachments")
		source.Add("workflow_states", map[string]any{"id": 1, "project_id": project, "detent_state": "Todo"})
		var repositoryID any
		if sourceRepository != "" {
			owner, name, _ := strings.Cut(sourceRepository, "/")
			source.Add("repositories", map[string]any{"id": 1, "github_owner": owner, "github_name": name})
			repositoryID = 1
		}
		const stamp = "2026-10-01T00:00:00Z"
		actor := tracker.Actor{Kind: "human", PrincipalID: "example-owner"}
		for index, id := range []tracker.NativeWorkItemID{nativeID, related} {
			issue := tracker.NativeIssue{NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: project, WorkItemID: id, Number: index + 1, Revision: 1, Profile: "github_compatible"}, Title: "Retained source", Body: "Retained GitHub content", State: "Todo", Actor: actor, CreatedAt: parseTestTime(t, stamp), UpdatedAt: parseTestTime(t, stamp), LastActivityAt: parseTestTime(t, stamp)}
			if sourceRepository != "" {
				issue.ExternalReferences = []tracker.ExternalReference{tracker.GitHubIssueSourceReference(fmt.Sprintf("I_retained_%d", index+1), fmt.Sprintf("https://github.com/%s/issues/%d", sourceRepository, index+1))}
			}
			source.Add("issues", map[string]any{"id": 41 + index, "native_id": id, "project_id": project, "number": index + 1, "revision": 1, "event_sequence": 1, "title": issue.Title, "body": issue.Body, "actor_json": string(historyTestJSON(t, actor)), "archived": 0, "created_at": stamp, "updated_at": stamp, "native_created_at": stamp, "native_updated_at": stamp, "last_activity_at": stamp, "workflow_state_id": 1, "labels_json": "[]", "assignees_json": "[]", "author_login": "example-owner", "repository_id": repositoryID})
			source.Add("collaboration_versions", map[string]any{"project_id": project, "work_item_id": id, "record_id": id, "revision": 1, "encoding": "snapshot", "record_json": string(historyTestJSON(t, issue))})
			source.Add("collaboration_events", map[string]any{"id": "event_" + string(id), "project_id": project, "work_item_id": id, "sequence": 1, "type": "issue.created", "schema_version": 1, "actor_json": string(historyTestJSON(t, actor)), "data_json": string(historyTestJSON(t, tracker.CollaborationData{Revision: 1})), "event_revision": 1, "recorded_at": stamp})
		}
		count, err := ImportCoreWorkItems(t.Context(), service.config.TenantRepository.(*postgres.Repository), CoreWorkItemImportSource{BundlePath: source.Write(), OrganizationID: organization, ProjectID: project, Objects: service.runDetailStore()})
		if err != nil || count != 2 {
			t.Fatalf("retained compatible import=%d error=%v", count, err)
		}
		return f, project
	}
	f, project := newFixture(t, "example-org/example-project", "example-org/example-project", true)
	service, organization := f.service, f.project.OrganizationID
	call := func(t *testing.T, f nativeFixture, project tracker.ProjectID, name, key string, fields map[string]any) bool {
		path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
		t.Helper()
		fields["project_id"] = project
		if fields["identifier"] == nil {
			fields["identifier"] = string(project) + "#1"
		}
		fields["request_id"] = key
		response := performHubWorkCall(t, f.service, path, testHubAdminToken, name, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": fields, "_meta": map[string]any{"io.modelcontextprotocol/protocolVersion": "2026-07-28", "io.modelcontextprotocol/clientCapabilities": map[string]any{}, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"}}}})
		requireNativeStatus(t, response, http.StatusOK)
		return strings.Contains(response.Body.String(), `"isError":true`)
	}
	for _, tt := range []struct {
		name, tool string
		fields     map[string]any
		failed     bool
	}{
		{"dependency", "set_dependency", map[string]any{"related": string(project) + "#2", "operation": "add"}, false},
		{"dependency replay", "set_dependency", map[string]any{"related": related, "operation": "add"}, false},
		{"foreign dependency", "set_dependency", map[string]any{"related": "wi_foreign", "operation": "add"}, true},
		{"foreign related prefix", "set_dependency", map[string]any{"related": "prj_foreign#2", "operation": "add"}, true},
		{"order", "order_item", map[string]any{"queue_scope": "fleet", "state": "Todo", "rank": "0001"}, false},
		{"priority", "set_queue_priority", map[string]any{"queue_scope": "fleet", "state": "Todo", "queue_priority": "high"}, false},
		{"priority replay", "set_queue_priority", map[string]any{"queue_scope": "fleet", "state": "Todo", "queue_priority": "high"}, false},
		{"lane writer absent", "move_item", map[string]any{"target_state": "Todo"}, true},
		{"external content", "edit_item", map[string]any{"title": "External", "expected_revision": "1"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			key := strings.TrimSuffix(tt.name, " replay")
			if failed := call(t, f, project, tt.tool, key, tt.fields); failed != tt.failed {
				t.Fatalf("failed=%t want=%t", failed, tt.failed)
			}
		})
	}
	if !call(t, f, project, operatortool.SetQueuePriority, "priority", map[string]any{"queue_scope": "fleet", "state": "Todo", "queue_priority": "low"}) {
		t.Fatal("changed priority retry was accepted")
	}
	var count int
	admin := nativeAttemptTestDatabase(t, f)
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM issue_dependencies d JOIN issues dependent ON dependent.organization_id=d.organization_id AND dependent.id=d.dependent_issue_id JOIN issues blocker ON blocker.organization_id=d.organization_id AND blocker.id=d.blocker_issue_id WHERE d.organization_id=$1 AND dependent.project_id=$2 AND dependent.native_id=$3 AND blocker.project_id=$2 AND blocker.native_id=$4", organization, project, nativeID, related).Scan(&count); err != nil || count != 1 {
		t.Fatalf("dependency count=%d %v", count, err)
	}
	if err := service.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		entry, err := r.QueueEntry(t.Context(), project, nativeID, "fleet")
		if err != nil {
			return err
		}
		if entry.Rank != "0001" || entry.Priority == nil || *entry.Priority != tracker.QueuePriorityHigh {
			return fmt.Errorf("queue=%+v", entry)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2", organization, project).Scan(&count); err != nil || count != 1 {
		t.Fatalf("outbox count=%d %v", count, err)
	}

	var outbound projectstore.GitHubOutbound
	var key string
	if err := admin.QueryRowContext(t.Context(), "SELECT command_key FROM github_outbound WHERE organization_id=$1 AND project_id=$2", organization, project).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if err := service.config.TenantRepository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
		owner, ok := w.(projectstore.GitHubOutboxWriter)
		if !ok {
			return hubapi.NativeWorkItemStorageUnavailable()
		}
		var err error
		outbound, err = owner.GitHubOutbound(t.Context(), key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	payload, err := eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), outbound.Payload)
	var desired WorkflowLabelDesired
	if err != nil || json.Unmarshal(payload, &desired) != nil || outbound.Repository != "example-org/example-project" || outbound.IssueNumber != 1 || outbound.Kind != string(MutationWorkflowLabel) || outbound.Outcome != "unattempted" || len(outbound.Key) != 79 || !strings.HasPrefix(outbound.Key, "queue-priority:") || desired.Label != "priority:high" || desired.ManagedPrefix != "priority:" || desired.Clear {
		t.Fatalf("priority outbound=%+v desired=%s error=%v", outbound, payload, err)
	}
	for _, scenario := range []struct {
		name, source, repository string
		enabled                  bool
	}{
		{"local source", "", "example-org/example-project", true},
		{"disabled integration", "example-org/example-project", "example-org/example-project", false},
		{"mismatched integration", "example-org/foreign-project", "example-org/example-project", true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, project := newFixture(t, scenario.source, scenario.repository, scenario.enabled)
			for range 2 {
				if call(t, f, project, operatortool.SetQueuePriority, "local-priority", map[string]any{"queue_scope": "fleet", "state": "Todo", "queue_priority": "high"}) {
					t.Fatal("local priority was refused without an eligible remote source")
				}
			}
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				entry, err := r.QueueEntry(t.Context(), project, nativeID, "fleet")
				if err != nil {
					return err
				}
				if entry.Priority == nil || *entry.Priority != tracker.QueuePriorityHigh {
					return fmt.Errorf("local priority=%+v", entry)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := nativeAttemptTestDatabase(t, f).QueryRowContext(t.Context(), "SELECT count(*) FROM github_outbound WHERE organization_id=$1 AND project_id=$2", f.project.OrganizationID, project).Scan(&count); err != nil || count != 0 {
				t.Fatalf("ineligible source outbound count=%d error=%v", count, err)
			}
		})
	}
}

func performHubWorkCall(t *testing.T, service *Service, path, token, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
	response := httptest.NewRecorder()
	service.Handler().ServeHTTP(response, request)
	return response
}

func TestSafeNativeWorkError(t *testing.T) {
	t.Parallel()
	f := newNativeFixture(t, nil, "", "public-filing-errors")
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID}
	now := time.Now().UTC()
	validSource := tracker.Provenance{Provider: "github", ExternalID: "issue", AuthorID: "author", CreatedAt: now, UpdatedAt: now, ObservedAt: now}
	for _, test := range []struct {
		name    string
		request tracker.CreateIssue
		worker  bool
		setup   func(*projectstore.Record)
		message string
	}{
		{name: "Triage", request: tracker.CreateIssue{State: "Triage"}, message: "Triage is reserved for linked GitHub issue intake"},
		{name: "Triage with Backlog", request: tracker.CreateIssue{State: "Triage"}, setup: func(p *projectstore.Record) { p.States = append(p.States, tracker.NativeState{Name: "Backlog"}) }, message: "Triage is reserved for linked GitHub issue intake; use Backlog for drafts"},
		{name: "worker Triage with restricted Backlog", worker: true, request: tracker.CreateIssue{State: "Triage"}, setup: func(p *projectstore.Record) {
			p.States = append(p.States, tracker.NativeState{Name: "Backlog", OperatorOnly: true})
		}, message: "Triage is reserved for linked GitHub issue intake"},
		{name: "title", request: tracker.CreateIssue{Title: strings.Repeat("x", 501)}, message: "Title must contain 1 to 500 bytes and body at most 256 KiB"},
		{name: "priority", request: tracker.CreateIssue{Priority: new(4)}, message: "Priority must be between 0 and 3"},
		{name: "label count", request: tracker.CreateIssue{Labels: make([]string, 101)}, message: "At most 100 labels or assignees are supported"},
		{name: "label value", request: tracker.CreateIssue{Labels: []string{""}}, message: "Labels and assignees must contain 1 to 200 bytes"},
		{name: "worker provenance", request: tracker.CreateIssue{Provenance: &validSource}, worker: true, message: "Imported provenance requires an operator"},
		{name: "import provider", request: tracker.CreateIssue{Provenance: &tracker.Provenance{Provider: "unknown"}}, message: "Import source is invalid"},
		{name: "import author", request: tracker.CreateIssue{Provenance: &tracker.Provenance{Provider: "github"}}, message: "Import source and author are invalid"},
		{name: "import time", request: tracker.CreateIssue{Provenance: &tracker.Provenance{Provider: "github", ExternalID: "issue", AuthorID: "author"}}, message: "Import timestamps are invalid"},
		{name: "compatibility project", setup: func(p *projectstore.Record) { p.Core.Profile = "github_compatible" }, message: "Compatibility project content is externally owned"},
		{name: "unknown lane", request: tracker.CreateIssue{State: "Unknown"}, message: "Workflow state does not exist"},
		{name: "operator lane", worker: true, setup: func(p *projectstore.Record) {
			for i := range p.States {
				if p.States[i].Name == "Todo" {
					p.States[i].OperatorOnly = true
				}
			}
		}, message: "Workflow target requires an operator"},
		{name: "reserved label", request: tracker.CreateIssue{Labels: []string{workspaceItemLabel}}, message: "Label detent:workspace is reserved"},
		{name: "linked provenance", request: tracker.CreateIssue{GitHubIssueURL: "https://github.com/example-org/example-project/issues/1", Provenance: &validSource}, message: "Linked issues obtain provenance during runner intake"},
		{name: "linked repository required", request: tracker.CreateIssue{GitHubIssueURL: "https://github.com/example-org/example-project/issues/1"}, message: linkedIssueRepositoryRequired},
		{name: "linked repository mismatch", setup: func(p *projectstore.Record) { p.Settings.CheckoutRepository = "example-org/other-project" }, request: tracker.CreateIssue{GitHubIssueURL: "https://github.com/example-org/example-project/issues/1"}, message: linkedIssueRepositoryMismatch},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := projectstore.Record{Core: corestore.Project{OrganizationID: scope.Organization, ID: tracker.ProjectID(hubapi.NewID("prj")), Name: "Validation example " + test.name, Profile: "native"}, States: nativeFixtureStates()}
			if test.setup != nil {
				test.setup(&project)
			}
			if err := f.service.config.TenantRepository.WriteTenant(t.Context(), scope.Organization, corestore.Serving, func(w tenantstore.Writer) error {
				stored, err := w.CreateProjectRecord(t.Context(), project)
				if err != nil {
					return err
				}
				if project.Settings.CheckoutRepository != "" {
					stored.Settings.CheckoutRepository = project.Settings.CheckoutRepository
					_, err = w.UpdateProjectRecord(t.Context(), stored)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			request := test.request
			if request.Title == "" {
				request.Title = "Issue validation"
			}
			if request.Body == "" {
				request.Body = issueContractTestSections
			}
			if request.State == "" {
				request.State = "Todo"
			}
			caller := scope
			caller.Project = project.Core.ID
			if test.worker {
				caller.Credential.Scope = hubapi.APIScopeWorker
			}
			_, cause := f.service.loadWorkItemPlan(t.Context(), caller, "", "", request)
			err := safeNativeWorkError(cause)
			var refusal *operatortool.RequestError
			if !errors.As(err, &refusal) || refusal.Code != "invalid_request" || refusal.Message != test.message || errors.Is(err, operatortool.ErrSnapshotUnavailable) {
				t.Fatalf("refusal=%#v, want invalid_request: %s", err, test.message)
			}
			response := httptest.NewRecorder()
			context := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/work-items", nil), response)
			service := &Service{config: Config{Hosted: &hostedplans.HostedConfig{}, Logger: discardLogger()}}
			if err := service.nativeAPIError(context, cause); err != nil {
				t.Fatal(err)
			}
			if refusal := requireNativeCode(t, response, http.StatusUnprocessableEntity, "invalid_request"); refusal.Message != test.message {
				t.Fatalf("hosted refusal=%s, want %s", refusal.Message, test.message)
			}
		})
	}
	admin := nativeAttemptTestDatabase(t, f)
	_, sqlFailure := admin.ExecContext(t.Context(), "SELECT missing_private_column FROM issues")
	if sqlFailure == nil {
		t.Fatal("expected SQL diagnostic")
	}
	for _, test := range []struct {
		name  string
		cause error
	}{
		{"private refusal", hubapi.NativeInvalid("private refusal diagnostic")},
		{"SQL failure", sqlFailure},
		{"internal failure", errors.New("private database diagnostic")},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := safeNativeWorkError(test.cause)
			var refusal *operatortool.RequestError
			if !errors.Is(err, operatortool.ErrSnapshotUnavailable) || !errors.Is(err, test.cause) || errors.As(err, &refusal) {
				t.Fatalf("private failure=%v", err)
			}
			response := httptest.NewRecorder()
			context := echo.New().NewContext(httptest.NewRequest(http.MethodPost, "/work-items", nil), response)
			service := &Service{config: Config{Hosted: &hostedplans.HostedConfig{}, Logger: discardLogger()}, database: f.service.database}
			if err := service.nativeAPIError(context, test.cause); err != nil {
				t.Fatal(err)
			}
			var failure hubapi.APIErrorResponse
			decodeHubResponse(t, response, &failure)
			want := "Native operation could not be completed"
			if test.name == "private refusal" {
				want = "Unprocessable Entity: invalid request"
			}
			if failure.Message != want {
				t.Fatalf("hosted private failure=%s", response.Body.String())
			}
		})
	}
}
