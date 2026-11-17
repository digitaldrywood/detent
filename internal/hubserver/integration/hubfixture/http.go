package hubfixture

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
)

func NewRequest(t *testing.T, method, path, token string, payload any) *http.Request {
	t.Helper()
	var body []byte
	if payload != nil {
		var err error
		if body, err = json.Marshal(payload); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequestWithContext(context.Background(), method, path, bytes.NewReader(body))
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	return request
}

func Request(t *testing.T, handler http.Handler, method, path, token string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, NewRequest(t, method, path, token, payload))
	return response
}

func WorkCall(t *testing.T, handler http.Handler, path, token, name string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(string(raw)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Mcp-Protocol-Version", MCPProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", name)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func RequireStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d: %s", response.Code, want, response.Body.String())
	}
}

func RequireCode(t *testing.T, response *httptest.ResponseRecorder, want int, code string) hubapi.NativeError {
	t.Helper()
	RequireStatus(t, response, want)
	var failure hubapi.NativeError
	Decode(t, response, &failure)
	if failure.Code != code {
		t.Fatalf("code = %q, want %q: %s", failure.Code, code, response.Body.String())
	}
	return failure
}

func Decode(t *testing.T, response *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}
