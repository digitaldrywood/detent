package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestMCPCommandUsesDaemonBridgeAndProtocolOnlyStdout(t *testing.T) {
	t.Parallel()

	for _, modern := range []bool{false, true} {
		for _, commands := range []bool{true, false} {
			t.Run("modern="+strconv.FormatBool(modern)+"/commands="+strconv.FormatBool(commands), func(t *testing.T) {

				requested := make(chan struct{}, 1)
				handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Method == http.MethodPost && request.URL.Path == "/api/v1/operator-connections" {
						if !commands {
							writer.WriteHeader(http.StatusNotFound)
							return
						}
						_, _ = io.WriteString(writer, `{"connection_id":"stdio-test"}`)
						return
					}
					expectedID := ""
					if commands {
						expectedID = "stdio-test"
					}
					if request.Header.Get("X-Detent-Connection-ID") != expectedID {
						t.Errorf("stdio request lost connection binding")
					}
					if request.Method == http.MethodGet && request.URL.Path == "/api/v1/operator-tools" {
						_, _ = io.WriteString(writer, `{"tools":[]}`)
						return
					}
					if request.Method != http.MethodPost || request.URL.Path != "/api/v1/operator-tools/fleet_health" {
						t.Errorf("request = %s %s", request.Method, request.URL.Path)
					}
					requested <- struct{}{}
					_, _ = io.WriteString(writer, `{"generated_at":"2026-08-08T02:30:00Z","freshness":"live","counts":{}}`)
				})
				opts := dashboardClientOptions(func(request *http.Request) (*http.Response, error) {
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					return response.Result(), nil
				}, "", "")
				port := 0
				opts.read = func(string) (globalconfig.Config, error) {
					return globalconfig.Config{Port: &port}, nil
				}
				opts.version = "v-test"
				configPath := "/config/global.yaml"
				host := "127.0.0.1"
				configuredPort := port
				cmd := newMCPCommand(&configPath, &host, &configuredPort, opts)
				reader, writer := io.Pipe()
				output := newProtocolWriter()
				var stderr bytes.Buffer
				cmd.SetIn(reader)
				cmd.SetOut(output)
				cmd.SetErr(&stderr)
				cmd.SetArgs(nil)
				done := make(chan error, 1)
				go func() {
					done <- cmd.ExecuteContext(t.Context())
				}()

				meta := `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`
				if modern {
					writeProtocolRequest(t, writer, `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{`+meta+`}}`)
					readProtocolResponse(t, output.frames)
					writeProtocolRequest(t, writer, `{"jsonrpc":"2.0","id":9,"method":"tools/list","params":{`+meta+`}}`)
				} else {
					writeProtocolRequest(t, writer, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
					readProtocolResponse(t, output.frames)
					writeProtocolRequest(t, writer, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
					writeProtocolRequest(t, writer, `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`)
				}
				discovery := readProtocolResponse(t, output.frames)
				if !bytes.Contains(discovery, []byte(`"tools":[]`)) {
					t.Fatalf("discovery did not use daemon authority: %s", discovery)
				}
				call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fleet_health","arguments":{}`
				if modern {
					call += `,` + meta
				}
				writeProtocolRequest(t, writer, call+`}}`)
				response := readProtocolResponse(t, output.frames)
				select {
				case <-requested:
				case <-time.After(5 * time.Second):
					t.Fatal("daemon bridge was not called")
				}
				if err := writer.Close(); err != nil {
					t.Fatalf("Close() error = %v", err)
				}
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("ExecuteContext() error = %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("timed out waiting for MCP command shutdown")
				}

				var envelope struct {
					Result struct {
						Content           []json.RawMessage          `json:"content"`
						StructuredContent map[string]json.RawMessage `json:"structuredContent"`
					} `json:"result"`
				}
				if err := json.Unmarshal(response, &envelope); err != nil {
					t.Fatalf("decode call response: %v", err)
				}
				if len(envelope.Result.Content) != 1 || string(envelope.Result.StructuredContent["freshness"]) != `"live"` {
					t.Fatalf("call response = %s", response)
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q, want empty", stderr.String())
				}
				for _, frame := range output.allFrames() {
					if !json.Valid(bytes.TrimSpace(frame)) {
						t.Fatalf("stdout contains non-protocol data: %q", frame)
					}
				}
			})
		}
	}
}

type protocolWriter struct {
	mu     sync.Mutex
	frames chan []byte
	all    [][]byte
}

func newProtocolWriter() *protocolWriter {
	return &protocolWriter{frames: make(chan []byte, 8)}
}

func (w *protocolWriter) Write(frame []byte) (int, error) {
	copyOfFrame := append([]byte(nil), frame...)
	w.mu.Lock()
	w.all = append(w.all, copyOfFrame)
	w.mu.Unlock()
	w.frames <- copyOfFrame
	return len(frame), nil
}

func (w *protocolWriter) allFrames() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	frames := make([][]byte, len(w.all))
	for index := range w.all {
		frames[index] = append([]byte(nil), w.all[index]...)
	}
	return frames
}

func writeProtocolRequest(t *testing.T, writer io.Writer, request string) {
	t.Helper()
	if _, err := io.WriteString(writer, request+"\n"); err != nil {
		t.Fatalf("write request: %v", err)
	}
}

func readProtocolResponse(t *testing.T, frames <-chan []byte) []byte {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for protocol response")
		return nil
	}
}
