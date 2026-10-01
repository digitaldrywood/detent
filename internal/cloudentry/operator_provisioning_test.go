//go:build !windows

package cloudentry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/chat"
	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatoradmin"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/labstack/echo/v4"
)

// Catches allocation payload leaks, creator/session bypasses, material effects
// before approval, and repeated resume resetting a later failed attempt.
func TestEntryProvisioningAdministration(t *testing.T) {
	for _, tc := range []struct{ name, state, step, code string }{
		{"requested", "requested", "", ""},
		{"allocating", "allocating", "owner_membership", ""},
		{"retryable", "failed", "tenant_files", "tenant_start_failed"},
		{"capacity", "failed", "", "capacity"},
		{"non-retryable", "failed", "admission", "quota"},
		{"ready", "ready", "publish", ""},
		{"approval", "failed", "tenant_files", "tenant_start_failed"},
		{"rejection", "failed", "", "capacity"},
		{"stale preview", "failed", "", "capacity"},
		{"revoked session", "failed", "", "capacity"},
		{"revoked provider session", "failed", "", "capacity"},
		{"revoked allocation", "failed", "", "capacity"},
		{"revoked service", "failed", "", "capacity"},
		{"foreign creator", "failed", "", "capacity"},
		{"unmanaged", "failed", "", "capacity"},
		{"deleted", "deleted", "", "capacity"},
		{"unavailable", "failed", "", "capacity"},
		{"oversized result", "failed", "", "capacity"},
		{"YOLO replay", "failed", "", "capacity"},
		{"cached authority", "failed", "", "capacity"},
		{"stdio", "failed", "", "capacity"},
		{"http", "failed", "", "capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProvisioningFixture(t, 1, nil)
			// Freeze only this fixture's allocator; existing journey tests exercise
			// its capacity/admission/retry processing after the shared command wakes it.
			f.service.stopAllocator()
			<-f.service.allocatorDone
			b := newBrowser(t, f.service.Handler())
			b.login("/organizations", "user_dana:")
			var ctx context.Context
			f.service.echo.POST("/provisioning-administration-test", func(c echo.Context) error {
				ctx = operatortool.BindConnection(c.Request().Context(), "provisioning-test", "fixture")
				return c.NoContent(http.StatusOK)
			}, f.service.administrationAuthority)
			if response := b.do(http.MethodPost, "/provisioning-administration-test", nil, nil); response.StatusCode != http.StatusOK {
				t.Fatalf("entry=%d %s", response.StatusCode, response.Body)
			}
			session, err := f.service.currentAdministrationSession(ctx, operatortool.ConnectionIdentity(ctx))
			if err != nil {
				t.Fatal(err)
			}
			creator := session
			if tc.name == "foreign creator" {
				creator.Subject = "user_eve"
			}
			id, err := f.service.recordIntent(ctx, creator, "provisioning_test", "fixture", "Saved organization")
			if err != nil {
				t.Fatal(err)
			}
			update := func(query string, args ...any) {
				t.Helper()
				if _, err := f.service.registry.store.db.ExecContext(ctx, query, args...); err != nil {
					t.Fatal(err)
				}
			}
			const sensitive = "allocation-secret-sentinel"
			update("UPDATE organizations SET state=?,step=?,error_code=?,error_detail=?,attempts=3,next_attempt_at='later' WHERE id=?", tc.state, tc.step, tc.code, sensitive, id)
			var audit bytes.Buffer
			f.service.config.Logger = slog.New(slog.NewJSONHandler(&audit, nil))
			e := f.service.administration
			if err := e.OpenConnection(ctx); err != nil {
				t.Fatal(err)
			}
			defs, err := e.ListTools(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{operatortool.ProvisioningPage, operatortool.ResumeProvisioning} {
				found := false
				for _, d := range defs {
					if d.Name == name {
						found = true
					}
				}
				if !found {
					t.Fatalf("missing provisioning catalog tool %s", name)
				}
			}
			if tc.name == "unmanaged" {
				update("UPDATE organizations SET managed=0 WHERE id=?", id)
			}
			if tc.name == "oversized result" {
				update("UPDATE organizations SET step=? WHERE id=?", strings.Repeat("x", 65), id)
			}
			if tc.name == "unavailable" {
				allocation := f.service.config.Allocation
				defer func() { f.service.config.Allocation = allocation }()
				f.service.config.Allocation = nil
			}
			read := operatortool.Call{Name: operatortool.ProvisioningPage, Arguments: json.RawMessage(`{"organization_id":"` + id + `"}`)}
			status, err := e.Execute(ctx, read)
			if tc.name == "foreign creator" || tc.name == "unmanaged" || tc.name == "deleted" || tc.name == "unavailable" || tc.name == "oversized result" {
				want := operatortool.ErrAccessDenied
				if tc.name == "unavailable" || tc.name == "oversized result" {
					want = operatoradmin.ErrUnavailable
				}
				if !errors.Is(err, want) {
					t.Fatalf("status error=%v want %v", err, want)
				}
				resume := operatortool.Call{Name: operatortool.ResumeProvisioning, Arguments: json.RawMessage(`{"request_id":"resume","organization_id":"` + id + `"}`)}
				if _, err := e.Execute(ctx, resume); !errors.Is(err, want) {
					t.Fatalf("resume error=%v want %v", err, want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var body struct {
				Data provisioningResult `json:"data"`
			}
			if err := json.Unmarshal(status.Content, &body); err != nil {
				t.Fatal(err)
			}
			retryable := tc.state == "failed" && (tc.code == "capacity" || strings.HasSuffix(tc.code, "_failed"))
			if body.Data.ID != id || body.Data.Name != "Saved organization" || body.Data.State != tc.state || body.Data.Step != tc.step || body.Data.CanResume != retryable {
				t.Fatalf("status=%s", status.Content)
			}
			if strings.Contains(string(status.Content), sensitive) || strings.Contains(string(status.Content), "error_detail") || strings.Contains(string(status.Content), "endpoint") {
				t.Fatalf("unsafe status=%s", status.Content)
			}
			if tc.state == "ready" && body.Data.Next != f.service.organizationHome(id) {
				t.Fatalf("ready destination=%q", body.Data.Next)
			}
			// The browser JSON and MCP use the same safe fields and creator lookup.
			browserStatus := b.do(http.MethodGet, "/api/cloud/organizations/"+id+"/provisioning", nil, nil)
			var browserBody provisioningResult
			if browserStatus.StatusCode != http.StatusOK || json.Unmarshal([]byte(browserStatus.Body), &browserBody) != nil || browserBody != body.Data {
				t.Fatalf("browser status differs: %s", browserStatus.Body)
			}
			if tc.name == "stdio" || tc.name == "http" {
				provisioningTransport(t, tc.name, ctx, e, id)
				return
			}
			human := chat.WithOperatorApproval(ctx, operatortool.ConnectionIdentity(ctx))
			approval := tc.name == "approval" || tc.name == "rejection" || tc.name == "stale preview" || strings.HasPrefix(tc.name, "revoked")
			if !approval {
				if err := e.Chat.SetConnectionMode(human, operatortool.CurrentConnection(ctx).ID, chat.YOLOMode); err != nil {
					t.Fatal(err)
				}
			}
			call := operatortool.Call{Name: operatortool.ResumeProvisioning, Arguments: json.RawMessage(`{"request_id":"resume","organization_id":"` + id + `"}`)}
			initial, err := e.Execute(ctx, call)
			if err != nil {
				t.Fatal(err)
			}
			var reply struct {
				Action chat.Action `json:"action"`
			}
			if err := json.Unmarshal(initial.Content, &reply); err != nil {
				t.Fatal(err)
			}
			if approval {
				if reply.Action.Status != chat.ActionPending {
					t.Fatalf("resume bypassed confirmation: %s", initial.Content)
				}
				current, err := f.service.registry.Organization(ctx, id)
				if err != nil || current.State != tc.state || current.Attempts != 3 || current.ErrorDetail != sensitive {
					t.Fatalf("preview changed allocation: %+v %v", current, err)
				}
				if tc.name == "rejection" {
					if _, err := e.Chat.RejectConnectionAction(human, operatortool.CurrentConnection(ctx).ID, reply.Action.ID); err != nil {
						t.Fatal(err)
					}
					current, err = f.service.registry.Organization(ctx, id)
					if err != nil || current.State != tc.state || current.Attempts != 3 {
						t.Fatal("rejected action changed allocation")
					}
					return
				}
				switch tc.name {
				case "stale preview":
					update("UPDATE organizations SET step='provider_organization' WHERE id=?", id)
				case "revoked service":
					allocation := f.service.config.Allocation
					defer func() { f.service.config.Allocation = allocation }()
					f.service.config.Allocation = nil
				case "revoked allocation":
					update("UPDATE organizations SET state='deleted' WHERE id=?", id)
				case "revoked session":
					if _, err := f.service.auth.store.db.ExecContext(ctx, "UPDATE sessions SET revoked_at='revoked'"); err != nil {
						t.Fatal(err)
					}
				case "revoked provider session":
					f.provider.mu.Lock()
					delete(f.provider.sessions, session.Identity.SessionID)
					f.provider.mu.Unlock()
				}
				_, err = e.Chat.Confirm(human, operatortool.CurrentConnection(ctx).ID, reply.Action.ID)
				if tc.name != "approval" {
					if err == nil {
						t.Fatal("stale authority/preview resumed allocation")
					}
					current, err = f.service.registry.Organization(ctx, id)
					if err != nil || current.State != tc.state && tc.name != "revoked allocation" || current.Attempts != 3 {
						t.Fatal("refused action changed allocation")
					}
					if strings.HasPrefix(tc.name, "revoked") {
						want := operatortool.ErrAccessDenied
						if tc.name == "revoked service" {
							want = operatoradmin.ErrUnavailable
						}
						if _, err := e.Execute(ctx, read); !errors.Is(err, want) {
							t.Fatalf("revoked read=%v", err)
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			current, err := f.service.registry.Organization(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if retryable {
				wantState := "allocating"
				if tc.step == "" {
					wantState = "requested"
				}
				if current.State != wantState || current.Attempts != 0 || current.ErrorCode != "" || current.ErrorDetail != "" || current.NextAttemptAt != "" {
					t.Fatalf("resume=%+v", current)
				}
				select {
				case <-f.service.wake:
				default:
					t.Fatal("resume did not wake allocator")
				}
			} else if current.State != tc.state || current.Attempts != 3 {
				t.Fatal("non-retryable resume changed allocation")
			}
			if tc.name == "cached authority" {
				update("UPDATE organizations SET state='deleted' WHERE id=?", id)
				for _, retry := range []operatortool.Call{call, {Name: operatortool.ActionResult, Arguments: json.RawMessage(`{"action_id":"` + reply.Action.ID + `"}`)}} {
					if _, err := e.Execute(ctx, retry); !errors.Is(err, operatortool.ErrAccessDenied) {
						t.Fatalf("cached result after creator loss=%v", err)
					}
				}
				return
			}
			if tc.name == "YOLO replay" {
				// A later allocator failure must survive the same request in memory and
				// on a freshly authenticated connection using the durable entry receipt.
				update("UPDATE organizations SET state='failed',attempts=9,error_code='tenant_start_failed',error_detail=? WHERE id=?", sensitive, id)
				for _, retryCtx := range []context.Context{ctx, operatortool.BindConnection(ctx, "reconnected", "fixture")} {
					if err := e.OpenConnection(retryCtx); err != nil {
						t.Fatal(err)
					}
					if err := e.Chat.SetConnectionMode(human, operatortool.CurrentConnection(retryCtx).ID, chat.YOLOMode); err != nil {
						t.Fatal(err)
					}
					if _, err := e.Execute(retryCtx, call); err != nil {
						t.Fatal(err)
					}
					current, err = f.service.registry.Organization(ctx, id)
					if err != nil || current.Attempts != 9 || current.State != "failed" {
						t.Fatal("resume receipt reran command")
					}
				}
				update("UPDATE organizations SET state='ready' WHERE id=?", id)
				fresh, err := e.Execute(ctx, read)
				if err != nil || !strings.Contains(string(fresh.Content), f.service.organizationHome(id)) {
					t.Fatalf("stale ready destination: %s %v", fresh.Content, err)
				}
			}
			var events string
			if err := f.service.auth.store.db.QueryRowContext(ctx, "SELECT coalesce(group_concat(event),'') FROM audit").Scan(&events); err != nil {
				t.Fatal(err)
			}
			conversation, _ := json.Marshal(e.Chat.Conversation(operatortool.CurrentConnection(ctx).ID))
			if strings.Contains(events, sensitive) || strings.Contains(audit.String(), sensitive) || strings.Contains(string(conversation), sensitive) {
				t.Fatal("allocation diagnostics leaked into audit or approval")
			}
		})
	}
}

// Catches transport-specific dispatch/catalog differences using the real entry
// adapter; both transports execute status and submit the material resume preview.
func provisioningTransport(t *testing.T, transport string, ctx context.Context, e *operatoradmin.Executor, id string) {
	t.Helper()
	type response struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	var send func(string) response
	if transport == "stdio" {
		input, writeInput := io.Pipe()
		output, writeOutput := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- mcp.NewServer(e, "fixture").Serve(ctx, input, writeOutput) }()
		t.Cleanup(func() {
			_ = writeInput.Close()
			if err := <-done; err != nil {
				t.Error(err)
			}
			_ = input.Close()
			_ = output.Close()
			_ = writeOutput.Close()
		})
		decoder := json.NewDecoder(output)
		send = func(frame string) response {
			t.Helper()
			if _, err := io.WriteString(writeInput, frame+"\n"); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(frame, `"id":`) {
				return response{}
			}
			var reply response
			if err := decoder.Decode(&reply); err != nil {
				t.Fatal(err)
			}
			return reply
		}
	} else {
		handler := mcp.NewHTTPHandler(e, "fixture", mcp.HTTPConfig{Principal: func(r *http.Request) operatortool.Identity { return operatortool.ConnectionIdentity(r.Context()) }})
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			if err := handler.Shutdown(cleanup); err != nil {
				t.Error(err)
			}
		})
		sessionID := ""
		send = func(frame string) response {
			t.Helper()
			request := httptest.NewRequest(http.MethodPost, "http://detent.example/mcp", strings.NewReader(frame)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
			if sessionID != "" {
				request.Header.Set("Mcp-Session-Id", sessionID)
				request.Header.Set("Mcp-Protocol-Version", mcp.LegacyProtocolVersion)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if sessionID == "" {
				sessionID = recorder.Header().Get("Mcp-Session-Id")
			}
			if !strings.Contains(frame, `"id":`) {
				return response{}
			}
			var reply response
			if err := json.Unmarshal(recorder.Body.Bytes(), &reply); err != nil {
				t.Fatalf("decode %s: %v", recorder.Body.String(), err)
			}
			return reply
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"fixture","version":"1"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var catalog strings.Builder
	cursor := ""
	for {
		params := map[string]string{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		reply := send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":` + string(raw) + `}`)
		if len(reply.Error) > 0 {
			t.Fatalf("catalog error: %s", reply.Error)
		}
		catalog.Write(reply.Result)
		var page struct {
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(reply.Result, &page); err != nil {
			t.Fatal(err)
		}
		cursor = page.NextCursor
		if cursor == "" {
			break
		}
	}
	for _, name := range []string{operatortool.ProvisioningPage, operatortool.ResumeProvisioning} {
		if !strings.Contains(catalog.String(), `"name":"`+name+`"`) {
			t.Fatalf("%s missing catalog tool %s", transport, name)
		}
	}
	for _, tc := range []struct{ name, arguments, want string }{
		{operatortool.ProvisioningPage, `{"organization_id":"` + id + `"}`, `"can_resume":true`},
		{operatortool.ResumeProvisioning, `{"organization_id":"` + id + `","request_id":"transport"}`, `"status":"pending"`},
	} {
		reply := send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"` + tc.name + `","arguments":` + tc.arguments + `}}`)
		if len(reply.Error) > 0 || strings.Contains(string(reply.Result), `"isError":true`) || !strings.Contains(string(reply.Result), tc.want) || strings.Contains(string(reply.Result), "allocation-secret-sentinel") {
			t.Fatalf("%s %s result=%s error=%s", transport, tc.name, reply.Result, reply.Error)
		}
	}
	// Missing deployment services stay opaque on direct calls over either transport.
	app := e.App.(entryAdministration)
	allocation := app.service.config.Allocation
	defer func() { app.service.config.Allocation = allocation }()
	app.service.config.Allocation = nil
	for _, name := range []string{operatortool.ProvisioningPage, operatortool.ResumeProvisioning} {
		arguments := `{"organization_id":"` + id + `"}`
		if name == operatortool.ResumeProvisioning {
			arguments = `{"organization_id":"` + id + `","request_id":"unavailable"}`
		}
		reply := send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"` + name + `","arguments":` + arguments + `}}`)
		if len(reply.Error) == 0 && !strings.Contains(string(reply.Result), `"isError":true`) {
			t.Fatal("missing service accepted provisioning call")
		}
		if strings.Contains(string(reply.Result)+string(reply.Error), "allocation-secret-sentinel") {
			t.Fatal("missing service leaked diagnostics")
		}
	}

}
