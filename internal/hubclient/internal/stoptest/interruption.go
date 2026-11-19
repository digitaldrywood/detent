package stoptest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func InterruptionFault(t *testing.T, transport *testsupport.Transport, runtime runner.RuntimeExecution, mode string) func(tracker.NativeAttempt) {
	next := transport.Load()
	var held atomic.Pointer[http.Request]
	var observations, posts, reads, missingReads, conflicts atomic.Int32
	var first []byte
	transport.Store(testsupport.RoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && path.Base(path.Dir(r.URL.Path)) == "attempts" {
			reads.Add(1)
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/events") {
			return next.RoundTrip(r)
		}
		event := ReadEvent(t, r)
		if mode == "stop with late observation" && event.Type == "run.observed" {
			if observations.Add(1) != 1 {
				return next.RoundTrip(r)
			}
			original := r.Clone(t.Context())
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			original.Body = io.NopCloser(bytes.NewReader(raw))
			held.Store(original)
			return nil, io.ErrUnexpectedEOF
		}
		if event.Type != "run.finished" || event.Data.Outcome != "interrupted" {
			return next.RoundTrip(r)
		}
		n := posts.Add(1)
		if reads.Swap(0) == 0 {
			missingReads.Add(1)
		}
		if original := held.Swap(nil); original != nil {
			res, err := next.RoundTrip(original)
			if err != nil {
				return nil, err
			}
			res.Body.Close()
			if res.StatusCode != http.StatusOK {
				return nil, errors.New("held observation did not commit")
			}
		}
		if mode != "stop with late observation" {
			body, err := json.Marshal(event)
			if err != nil {
				return nil, err
			}
			if n == 1 {
				first = body
			} else if !bytes.Equal(body, first) {
				return nil, errors.New("interruption retry changed payload or key")
			}
			if mode == "stop with two undelivered interruptions" && n <= 2 {
				return nil, io.ErrUnexpectedEOF
			}
		}
		res, err := next.RoundTrip(r)
		if err == nil && res.StatusCode == http.StatusConflict {
			conflicts.Add(1)
		}
		if err == nil && res.StatusCode == http.StatusOK && mode == "stop with lost interruption reply" && n == 1 {
			res.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
		return res, err
	}))
	if mode == "stop with late observation" {
		for i := 0; i < 10 && held.Load() == nil; i++ {
			phase := []string{"implementation", "validation"}[i%2]
			if err := runtime.ObserveRuntime(context.WithoutCancel(t.Context()), tracker.NativeRuntimeObservation{Phase: phase, HeartbeatAt: time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
		}
		if held.Load() == nil {
			t.Fatal("worker did not send its observation before stop")
		}
	}
	return func(attempt tracker.NativeAttempt) {
		t.Helper()
		want := int32(1)
		if mode == "stop with late observation" {
			want = 2
		} else if mode == "stop with two undelivered interruptions" {
			want = 3
		}
		if attempt.ClaimReleasedAt == nil || attempt.Outcome != "interrupted" || posts.Load() != want || missingReads.Load() != 0 || mode == "stop with late observation" && (conflicts.Load() != 1 || observations.Load() != 1) || mode == "stop with lost interruption reply" && reads.Load() == 0 {
			t.Fatalf("interruption POSTs %d, reads %d, missing reads %d, observations %d, conflicts %d, outcome %s, released %v", posts.Load(), reads.Load(), missingReads.Load(), observations.Load(), conflicts.Load(), attempt.Outcome, attempt.ClaimReleasedAt)
		}
	}
}
