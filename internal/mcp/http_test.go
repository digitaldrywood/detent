package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/operatortool"
)

func TestHTTPTransportUsesSharedProtocolCatalog(t *testing.T) {
	t.Parallel()

	handler := newTestHTTPHandler(nil)
	initialize := performMCPRequest(t, handler, http.MethodPost, initializeRequest, "", "read-key", nil)
	if initialize.Code != http.StatusOK {
		t.Fatalf("initialize status = %d, want %d; body = %s", initialize.Code, http.StatusOK, initialize.Body.String())
	}
	sessionID := initialize.Header().Get(httpSessionHeader)
	if sessionID != "test-session" {
		t.Fatalf("session ID = %q, want test-session", sessionID)
	}

	initialized := performMCPRequest(t, handler, http.MethodPost, initializedNotice, sessionID, "read-key", nil)
	if initialized.Code != http.StatusAccepted || initialized.Body.Len() != 0 {
		t.Fatalf("initialized response = %d %q, want 202 with empty body", initialized.Code, initialized.Body.String())
	}
	list := performMCPRequest(t, handler, http.MethodPost, `{"jsonrpc":"2.0","id":"list","method":"tools/list","params":{}}`, sessionID, "read-key", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("tools/list status = %d, want %d; body = %s", list.Code, http.StatusOK, list.Body.String())
	}
	var response rpcResponse
	if err := json.Unmarshal(list.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode tools/list response: %v", err)
	}
	var result struct {
		Tools []listedTool `json:"tools"`
	}
	decodeResult(t, response, &result)
	want := operatortool.Catalog()
	if len(result.Tools) != len(want) {
		t.Fatalf("HTTP tools = %d, want %d", len(result.Tools), len(want))
	}
	for index, definition := range want {
		got := result.Tools[index]
		if got.Name != definition.Name || got.Description != definition.Description || !jsonEqual(got.InputSchema, definition.InputSchema) {
			t.Fatalf("HTTP tool[%d] = %#v, want %#v", index, got, definition)
		}
	}

	deleted := performMCPRequest(t, handler, http.MethodDelete, "", sessionID, "read-key", nil)
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want %d; body = %s", deleted.Code, http.StatusNoContent, deleted.Body.String())
	}
}

func TestHTTPTransportRequestBoundaries(t *testing.T) {
	t.Parallel()

	handler := newTestHTTPHandler(nil)
	initialize := performMCPRequest(t, handler, http.MethodPost, initializeRequest, "", "read-key", nil)
	sessionID := initialize.Header().Get(httpSessionHeader)
	tests := []struct {
		name       string
		method     string
		body       string
		sessionID  string
		principal  string
		headers    map[string]string
		wantStatus int
	}{
		{name: "GET streaming unsupported", method: http.MethodGet, sessionID: sessionID, principal: "read-key", wantStatus: http.StatusMethodNotAllowed},
		{name: "wrong content type", method: http.MethodPost, body: initializeRequest, principal: "read-key", headers: map[string]string{"Content-Type": "text/plain"}, wantStatus: http.StatusUnsupportedMediaType},
		{name: "request too large", method: http.MethodPost, body: strings.Repeat(" ", MaxHTTPRequestBytes+1), principal: "read-key", wantStatus: http.StatusRequestEntityTooLarge},
		{name: "cross origin", method: http.MethodPost, body: initializeRequest, principal: "read-key", headers: map[string]string{"Origin": "https://attacker.example"}, wantStatus: http.StatusForbidden},
		{name: "missing session", method: http.MethodPost, body: `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, principal: "read-key", wantStatus: http.StatusBadRequest},
		{name: "unknown session", method: http.MethodPost, body: `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sessionID: "missing", principal: "read-key", wantStatus: http.StatusNotFound},
		{name: "session bound to principal", method: http.MethodPost, body: `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sessionID: sessionID, principal: "different-key", wantStatus: http.StatusNotFound},
		{name: "protocol mismatch", method: http.MethodPost, body: `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, sessionID: sessionID, principal: "read-key", headers: map[string]string{httpProtocolHeader: "2024-11-05"}, wantStatus: http.StatusBadRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := performMCPRequest(t, handler, test.method, test.body, test.sessionID, test.principal, test.headers)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestHTTPTransportResultLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		executor      Executor
		wantRPCError  bool
		wantToolError bool
	}{
		{
			name:          "tool result limit",
			executor:      &staticExecutor{result: operatortool.Result{Content: json.RawMessage(`{"value":"` + strings.Repeat("x", operatortool.MaxResultBytes) + `"}`)}},
			wantToolError: true,
		},
		{
			name:         "HTTP envelope limit",
			executor:     &staticExecutor{err: errors.New(strings.Repeat("x", MaxHTTPResponseBytes))},
			wantRPCError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			handler := newTestHTTPHandler(test.executor)
			initialize := performMCPRequest(t, handler, http.MethodPost, initializeRequest, "", "read-key", nil)
			sessionID := initialize.Header().Get(httpSessionHeader)
			performMCPRequest(t, handler, http.MethodPost, initializedNotice, sessionID, "read-key", nil)
			call := performMCPRequest(t, handler, http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_health","arguments":{}}}`, sessionID, "read-key", nil)
			if call.Code != http.StatusOK || call.Body.Len() > MaxHTTPResponseBytes {
				t.Fatalf("call response = %d bytes=%d; body prefix = %.120s", call.Code, call.Body.Len(), call.Body.String())
			}
			var response rpcResponse
			if err := json.Unmarshal(call.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode call response: %v", err)
			}
			if test.wantRPCError {
				if response.Error == nil || response.Error.Code != codeInternalError {
					t.Fatalf("response error = %#v, want code %d", response.Error, codeInternalError)
				}
				return
			}
			var result toolCallResult
			decodeResult(t, response, &result)
			if test.wantToolError != result.IsError {
				t.Fatalf("tool result IsError = %t, want %t", result.IsError, test.wantToolError)
			}
		})
	}
}

func TestHTTPTransportShutdownCancelsCallsAndRejectsSessions(t *testing.T) {
	t.Parallel()

	executor := newBlockingExecutor()
	handler := newTestHTTPHandler(executor)
	initialize := performMCPRequest(t, handler, http.MethodPost, initializeRequest, "", "read-key", nil)
	sessionID := initialize.Header().Get(httpSessionHeader)
	performMCPRequest(t, handler, http.MethodPost, initializedNotice, sessionID, "read-key", nil)

	callDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		callDone <- performMCPRequest(t, handler, http.MethodPost, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_health","arguments":{}}}`, sessionID, "read-key", nil)
	}()
	executor.waitStarted(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	executor.waitCancelled(t)
	select {
	case response := <-callDone:
		if response.Code != http.StatusNotFound && response.Code != http.StatusServiceUnavailable {
			t.Fatalf("in-flight response status = %d, want 404 or 503", response.Code)
		}
	case <-ctx.Done():
		t.Fatal("in-flight HTTP call did not return during shutdown")
	}
	newSession := performMCPRequest(t, handler, http.MethodPost, initializeRequest, "", "read-key", nil)
	if newSession.Code != http.StatusServiceUnavailable {
		t.Fatalf("post-shutdown initialize status = %d, want %d", newSession.Code, http.StatusServiceUnavailable)
	}
}

func newTestHTTPHandler(executor Executor) *HTTPHandler {
	return NewHTTPHandler(executor, "test-version", HTTPConfig{
		Principal: func(req *http.Request) string {
			return req.Header.Get("X-Test-Principal")
		},
		GenerateSessionID: func() (string, error) {
			return "test-session", nil
		},
	})
}

func performMCPRequest(t *testing.T, handler http.Handler, method string, body string, sessionID string, principal string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, "http://detent.example/mcp", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Test-Principal", principal)
	if sessionID != "" {
		request.Header.Set(httpSessionHeader, sessionID)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	handler.ServeHTTP(recorder, request)
	return recorder
}
