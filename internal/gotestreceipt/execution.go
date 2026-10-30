package gotestreceipt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

type executionRequest struct {
	Args        []string `json:"args"`
	Directory   string   `json:"directory"`
	Root        string   `json:"root"`
	Environment []string `json:"environment"`
}

func (r executionRequest) lookup(name string) (string, bool) {
	for i := len(r.Environment) - 1; i >= 0; i-- {
		if value, ok := strings.CutPrefix(r.Environment[i], name+"="); ok {
			return value, true
		}
	}
	return "", false
}

type executionFrame struct {
	Output []byte `json:"output,omitempty"`
	Stderr bool   `json:"stderr,omitempty"`
	Code   *int   `json:"code,omitempty"`
}

type executionOutput struct {
	mu      *sync.Mutex
	encoder *json.Encoder
	flush   http.Flusher
	stderr  bool
}

func (o executionOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	size := len(data)
	for len(data) > 0 {
		n := min(len(data), 32768)
		if err := o.encoder.Encode(executionFrame{Output: data[:n], Stderr: o.stderr}); err != nil {
			return size - len(data), err
		}
		o.flush.Flush()
		data = data[n:]
	}
	return size, nil
}

func (s *Session) execute(w http.ResponseWriter, r *http.Request) {
	s.handlers.Add(1)
	defer s.handlers.Done()
	var request executionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRecordBytes)).Decode(&request); err != nil || len(request.Args) == 0 || !isTestBinary(request.Args[0]) {
		http.Error(w, "invalid execution", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	var mu sync.Mutex
	output := executionOutput{mu: &mu, encoder: encoder, flush: w.(http.Flusher)}
	stderr := output
	stderr.stderr = true
	code := executeWrapper(r.Context(), request, executionStore{session: s}, output, stderr)
	mu.Lock()
	defer mu.Unlock()
	if err := encoder.Encode(executionFrame{Code: &code}); err != nil {
		s.logger.Warn("write test execution result", "error", err)
	}
}

func requestExecution(socket string, request executionRequest, stdout, stderr io.Writer) int {
	client := newExecutionClient(socket)
	defer client.CloseIdleConnections()
	reader, writer := io.Pipe()
	encoded := make(chan error, 1)
	go func() { err := json.NewEncoder(writer).Encode(request); _ = writer.CloseWithError(err); encoded <- err }()
	response, err := client.Post(clientBaseURL+"/execute", "application/json", reader)
	_ = reader.Close()
	encodeErr := <-encoded
	if err != nil || encodeErr != nil {
		fmt.Fprintln(stderr, "test execution service unavailable")
		return 1
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		fmt.Fprintln(stderr, "test execution service refused request")
		return 1
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var frame executionFrame
		if err := decoder.Decode(&frame); err != nil {
			fmt.Fprintln(stderr, "test execution service interrupted")
			return 1
		}
		if frame.Code != nil {
			return *frame.Code
		}
		target := stdout
		if frame.Stderr {
			target = stderr
		}
		if _, err := target.Write(frame.Output); err != nil {
			return 1
		}
	}
}

func newExecutionClient(socket string) *http.Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: transport}
}
