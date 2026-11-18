package gotestreceipt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const maxFrameBytes = 32 << 10

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

func (r executionRequest) getenv(name string) string {
	value, _ := r.lookup(name)
	return value
}

type executionFrame struct {
	Output []byte `json:"output,omitempty"`
	Stderr bool   `json:"stderr,omitempty"`
	Code   *int   `json:"code,omitempty"`
	Local  bool   `json:"local,omitempty"`
}

type frameWriter struct {
	mu      *sync.Mutex
	encoder *json.Encoder
	flusher http.Flusher
	stderr  bool
}

func (w frameWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	size := len(data)
	for len(data) > 0 {
		n := min(len(data), maxFrameBytes)
		if err := w.encoder.Encode(executionFrame{Output: data[:n], Stderr: w.stderr}); err != nil {
			return size - len(data), err
		}
		w.flusher.Flush()
		data = data[n:]
	}
	return size, nil
}

func (s *Session) execute(w http.ResponseWriter, r *http.Request) {
	s.handlers.Add(1)
	defer s.handlers.Done()
	var request executionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&request); err != nil || !s.executableTestRequest(request) {
		http.Error(w, "invalid execution", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	var mu sync.Mutex
	encoder := json.NewEncoder(w)
	stdout := frameWriter{mu: &mu, encoder: encoder, flusher: flusher}
	stderr := stdout
	stderr.stderr = true
	code, local := s.executeTest(r.Context(), request, stdout, stderr)
	mu.Lock()
	defer mu.Unlock()
	if err := encoder.Encode(executionFrame{Code: &code, Local: local}); err != nil {
		s.logger.Warn("write go test execution result", "error", err)
	}
}

var (
	errExecutionUnavailable = errors.New("go test receipt session unavailable")
	errRunLocally           = errors.New("go test receipt session cannot certify this test binary")
)

func requestExecution(socket string, request executionRequest, stdout, stderr io.Writer) (int, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		var conn net.Conn
		err := withSocketPath(socket, func(path string) error {
			var dialErr error
			conn, dialErr = dialer.DialContext(ctx, "unix", path)
			return dialErr
		})
		if err != nil && conn != nil {
			return nil, errors.Join(err, conn.Close())
		}
		return conn, err
	}}
	defer transport.CloseIdleConnections()
	body, err := json.Marshal(request)
	if err != nil {
		return 0, errors.Join(errExecutionUnavailable, err)
	}
	httpRequest, err := http.NewRequestWithContext(context.Background(), http.MethodPost, clientBaseURL+executePath, bytes.NewReader(body))
	if err != nil {
		return 0, errors.Join(errExecutionUnavailable, err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Transport: transport}).Do(httpRequest)
	if err != nil {
		return 0, errors.Join(errExecutionUnavailable, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: %s", errExecutionUnavailable, response.Status)
	}
	decoder := json.NewDecoder(response.Body)
	for {
		var frame executionFrame
		if err := decoder.Decode(&frame); err != nil {
			return 1, fmt.Errorf("go test receipt session interrupted: %w", err)
		}
		if frame.Local {
			return 0, errRunLocally
		}
		if frame.Code != nil {
			return *frame.Code, nil
		}
		target := stdout
		if frame.Stderr {
			target = stderr
		}
		if _, err := target.Write(frame.Output); err != nil {
			return 1, fmt.Errorf("write test output: %w", err)
		}
	}
}
