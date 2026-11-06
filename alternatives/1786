package hubserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/mcp"
	"github.com/digitaldrywood/detent/internal/operatortool"
)

type hostedContextReply struct {
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

func hostedContextProtocol(t *testing.T, service *Service, ctx context.Context, transport string) func(string, string, any) hostedContextReply {
	t.Helper()
	var exchange func([]byte) []byte
	executor := hostedOperatorExecutor{service}
	if transport == "stdio" {
		input, inputWriter := io.Pipe()
		outputReader, output := io.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- mcp.NewServer(executor, "test", mcp.WithLogger(service.config.Logger)).Serve(context.WithoutCancel(ctx), input, output)
		}()
		t.Cleanup(func() {
			if err := inputWriter.Close(); err != nil {
				t.Error(err)
			}
			if err := <-done; err != nil {
				t.Error(err)
			}
			if err := output.Close(); err != nil {
				t.Error(err)
			}
			if err := outputReader.Close(); err != nil {
				t.Error(err)
			}
		})
		reader := bufio.NewReader(outputReader)
		exchange = func(frame []byte) []byte {
			if _, err := inputWriter.Write(append(frame, '\n')); err != nil {
				t.Fatal(err)
			}
			raw, err := reader.ReadBytes('\n')
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}
	} else {
		handler := mcp.NewHTTPHandler(executor, "test", mcp.HTTPConfig{Logger: service.config.Logger, Principal: func(r *http.Request) operatortool.Identity { return operatortool.ConnectionIdentity(r.Context()) }})
		t.Cleanup(func() {
			if err := handler.Shutdown(context.WithoutCancel(ctx)); err != nil {
				t.Error(err)
			}
		})
		exchange = func(frame []byte) []byte {
			var message struct {
				Method string `json:"method"`
				Params struct {
					Name string `json:"name"`
				} `json:"params"`
			}
			if err := json.Unmarshal(frame, &message); err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "http://detent.example/mcp", strings.NewReader(string(frame))).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Mcp-Protocol-Version", "2026-07-28")
			r.Header.Set("Mcp-Method", message.Method)
			if message.Params.Name != "" {
				r.Header.Set("Mcp-Name", message.Params.Name)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			return response.Body.Bytes()
		}
	}
	return func(method, name string, args any) hostedContextReply {
		params := map[string]any{"_meta": json.RawMessage(fleetProtocolMeta)}
		if method == "tools/call" {
			params["name"], params["arguments"] = name, args
		} else if args != nil {
			params["cursor"] = args
		}
		frame, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		raw := exchange(frame)
		for _, secret := range []string{"csrf_token", "form_token", "session_owner", "refresh_token", "token_hash", "credential-sensitive-value-sentinel", "private-worktree-sentinel"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("protocol leaked %s", secret)
			}
		}
		var reply hostedContextReply
		if err := json.Unmarshal(raw, &reply); err != nil {
			t.Fatalf("protocol response: %s: %v", raw, err)
		}
		return reply
	}
}

func hostedContextData(t *testing.T, reply hostedContextReply, denied bool) json.RawMessage {
	t.Helper()
	var result struct {
		IsError bool            `json:"isError"`
		Data    json.RawMessage `json:"structuredContent"`
	}
	if len(reply.Result) > 0 {
		if err := json.Unmarshal(reply.Result, &result); err != nil {
			t.Fatal(err)
		}
	}
	failed := result.IsError || len(reply.Error) > 0
	refusal := len(result.Data) == 0
	if denied && !refusal {
		var body map[string]json.RawMessage
		_ = json.Unmarshal(result.Data, &body)
		_, hasCode := body["code"]
		_, hasMessage := body["message"]
		refusal = hasCode && hasMessage && len(body) == 2
	}
	if failed != denied || denied && !refusal || !denied && len(result.Data) == 0 {
		t.Fatalf("denied=%t response=%s %s", denied, reply.Result, reply.Error)
	}
	return result.Data
}

func seedHostedContextRunner(t *testing.T, f hostedSecurityFixture, id, version string, heartbeat time.Time, revoked bool) {
	t.Helper()
	now := formatHubTime(f.service.config.now())
	token := "tok_" + id
	operatorSQL(t, f, `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at,native_only) VALUES(?,?,?,?,'worker',?,?,1)`, token, token, sha256Hex(token), id, now, now)
	operatorSQL(t, f, `INSERT INTO machines(id,hostname,display_name,capacity,version,last_heartbeat_at,registered_at,updated_at,organization_id,token_id) VALUES(?,?,?,1,?,?,?,?,'org_security',?)`, "machine_"+id, id, id, version, formatHubTime(heartbeat), now, now, token)
	operatorSQL(t, f, `INSERT INTO runner_enrollments(id,organization_id,runner_id,machine_id,token_hash,operations_json,created_at,expires_at,created_by,redeemed_at) VALUES(?,'org_security',?,?,?,'["claim"]',?,?,?,?)`, "enr_"+id, id, "machine_"+id, sha256Hex("enr_"+id), now, formatHubTime(f.service.config.now().Add(time.Hour)), token, now)
	operatorSQL(t, f, `INSERT INTO runner_identities(id,organization_id,machine_id,token_id,enrollment_id,operations_json,created_at,display_name,capacity_limit,reported_capacity,os,architecture,last_heartbeat_at) VALUES(?,'org_security',?,?,?,'["claim"]',?,?,1,1,'linux','arm64',?)`, id, "machine_"+id, token, "enr_"+id, now, id, formatHubTime(heartbeat))
	if revoked {
		operatorSQL(t, f, "UPDATE api_tokens SET revoked_at=? WHERE id=?", now, token)
	}
}
