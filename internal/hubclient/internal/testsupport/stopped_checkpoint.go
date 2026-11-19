package testsupport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type StoppedCheckpointDelivery struct {
	Batches, DiffPosts, InterruptionConflicts int
	Sequences                                 []int64
	DeliverBatch                              func(int)
}

func TraceStoppedCheckpoint(t *testing.T, transport *Transport, batch, reply, attemptID string, base int64, worker context.Context, stop func(), changeBase func()) *StoppedCheckpointDelivery {
	t.Helper()
	trace := new(StoppedCheckpointDelivery)
	next := transport.Load()
	var inflight, checkpoint *http.Request
	gateCommitted := false
	var payload []byte
	deliver := func(request *http.Request, wantStatus int) {
		t.Helper()
		if request == nil {
			t.Fatal("no request in flight")
		}
		resp, err := next.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != wantStatus {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("delayed request returned %d, want %d: %s", resp.StatusCode, wantStatus, body)
		}
	}
	trace.DeliverBatch = func(status int) {
		t.Helper()
		deliver(inflight, status)
		inflight = nil
	}
	delayCheckpoint := reply == "checkpoint wins interruption" || reply == "interruption wins checkpoint"
	miss := reply == "checkpoint never arrived" || reply == "rebuilt checkpoint never arrived"
	lose := reply == "lost"
	loseConflict := reply == "conflict reply lost"
	transport.Store(RoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/diff") {
			trace.DiffPosts++
			resp, err := next.RoundTrip(r)
			if trace.DiffPosts == 2 && reply == "source changes before rebuilt checkpoint" {
				changeBase()
			}
			return resp, err
		}
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/events") {
			return next.RoundTrip(r)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var event tracker.NativeRunEvent
		if err := json.Unmarshal(body, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "run.observed" {
			trace.Batches++
			if worker.Err() != nil || trace.Batches != 1 || len(event.PriorEvents) != 1 || event.PriorEvents[0].Data.Sequence != base+1 || event.Data.Sequence != base+2 {
				t.Fatalf("unexpected batch at %d, send %d, stop %v", event.Data.Sequence, trace.Batches, context.Cause(worker))
			}
			if batch == "acknowledged" || batch == "stop during batch reply" {
				resp, err := next.RoundTrip(r)
				if batch == "stop during batch reply" {
					stop()
				}
				return resp, err
			}
			inflight = r.Clone(t.Context())
			if batch == "before stop" {
				trace.DeliverBatch(http.StatusOK)
			}
			return nil, io.ErrUnexpectedEOF
		}
		if event.Type == "run.finished" {
			if batch == "before checkpoint" && inflight != nil {
				trace.DeliverBatch(http.StatusOK)
			}
			if checkpoint != nil && reply == "checkpoint wins interruption" {
				deliver(checkpoint, http.StatusOK)
				checkpoint = nil
			}
			resp, err := next.RoundTrip(r)
			if err == nil && resp.StatusCode == http.StatusConflict {
				trace.InterruptionConflicts++
			}
			if err == nil && resp.StatusCode == http.StatusOK {
				if inflight != nil && batch == "after checkpoint" && reply != "batch arrives after second cleanup" {
					trace.DeliverBatch(http.StatusConflict)
				}
				if checkpoint != nil {
					status := http.StatusConflict
					if gateCommitted {
						status = http.StatusOK
					}
					deliver(checkpoint, status)
					checkpoint = nil
				}
			}
			return resp, err
		}
		seq := event.Data.Sequence
		if event.Type != "run.checkpointed" || len(event.PriorEvents) != 0 {
			t.Fatalf("unexpected %s with %d prior events", event.Type, len(event.PriorEvents))
		}
		if batch != "unsent" && event.IdempotencyKey != attemptID+":"+strconv.FormatInt(seq, 10)+":receipt" {
			t.Fatalf("checkpoint %d key = %q", seq, event.IdempotencyKey)
		}
		if len(trace.Sequences) > 0 && trace.Sequences[len(trace.Sequences)-1] == seq && !bytes.Equal(payload, body) {
			t.Fatalf("retry changed checkpoint %d", seq)
		}
		trace.Sequences, payload = append(trace.Sequences, seq), body
		finalSequence := batch != "before checkpoint" || seq == base+3
		if delayCheckpoint && finalSequence {
			delayCheckpoint = false
			checkpoint = r.Clone(t.Context())
			return nil, io.ErrUnexpectedEOF
		}
		if miss && (reply != "rebuilt checkpoint never arrived" || finalSequence) {
			miss = false
			return nil, io.ErrUnexpectedEOF
		}
		if batch == "before checkpoint" && inflight != nil {
			trace.DeliverBatch(http.StatusOK)
		}
		resp, err := next.RoundTrip(r)
		if err == nil && resp.StatusCode == http.StatusConflict && reply == "source changes before rebuilt diff" {
			changeBase()
		}
		if err == nil && resp.StatusCode == http.StatusConflict && loseConflict {
			loseConflict = false
			resp.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
		if err == nil && resp.StatusCode == http.StatusOK {
			gateCommitted = true
			if reply == "stop during checkpoint reply" {
				stop()
			}
			if batch == "after checkpoint" && reply != "batch arrives after second cleanup" && inflight != nil {
				trace.DeliverBatch(http.StatusConflict)
			}
			if lose {
				lose = false
				resp.Body.Close()
				return nil, io.ErrUnexpectedEOF
			}
		}
		return resp, err
	}))
	return trace
}
