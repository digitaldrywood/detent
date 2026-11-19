package stoptest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func ReadEvent(t *testing.T, request *http.Request) tracker.NativeRunEvent {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	var event tracker.NativeRunEvent
	if err := json.Unmarshal(body, &event); err != nil {
		t.Fatal(err)
	}
	return event
}

type interruptionTrace struct {
	posts, reads int
	restore      func()
}

func interruptDelivery(t *testing.T, tr *testsupport.Transport, mode string) *interruptionTrace {
	trace := new(interruptionTrace)
	if mode == "" {
		return trace
	}
	next := tr.Load()
	trace.restore = func() { tr.Store(next) }
	var first []byte
	tr.Store(testsupport.RoundTrip(func(r *http.Request) (*http.Response, error) {
		if trace.posts > 0 && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/attempts/") {
			trace.reads++
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/events") || ReadEvent(t, r).Type != "run.finished" {
			return next.RoundTrip(r)
		}
		trace.posts++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if trace.posts > 1 {
			if trace.reads == 0 || !bytes.Equal(first, body) {
				t.Fatal("interruption retry changed the event or preceded the fenced read")
			}
			if mode != "outage" {
				return next.RoundTrip(r)
			}
		}
		first = body
		if mode == "committed" {
			res, err := next.RoundTrip(r)
			if err != nil {
				return nil, err
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				t.Fatalf("interruption response %d", res.StatusCode)
			}
		}
		return nil, io.ErrUnexpectedEOF
	}))
	return trace
}
