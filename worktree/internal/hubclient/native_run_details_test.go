package hubclient

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeObservationDetailBatches(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name             string
		observations     int
		checkpoint, fail bool
		heartbeat        bool
	}{
		{"checkpoint", 2, true, false, false},
		{"bounded observation batch", 8, false, false, false},
		{"failed boundary retains replay", 2, true, true, false},
		{"heartbeat flushes partial batch", 2, false, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			var requests [][]byte
			transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
				raw, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				requests = append(requests, raw)
				status := http.StatusOK
				if test.fail && len(requests) == 1 {
					status = http.StatusServiceUnavailable
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Request: request}, nil
			})
			client, err := New(Config{URL: "http://hub.example.test", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native("org_example", "prj_example")
			if err != nil {
				t.Fatal(err)
			}
			lease := tracker.NativeLease{RunDetailBatching: true, WorkItemID: "wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ID: "lease", FencingToken: 1}
			claim := nativeClaim{source: &NativeConnector{client: native}, lease: lease, deadline: now.Add(time.Minute)}
			scheduler := &Scheduler{now: func() time.Time { return now }, leaseTTL: time.Minute, nativeClaims: map[string]nativeClaim{string(lease.WorkItemID): claim}}
			execution := &nativeExecution{scheduler: scheduler, claim: claim, runtimePublishedAt: now, data: tracker.NativeRunData{AttemptID: "attempt_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RunID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", LeaseID: lease.ID, FencingToken: lease.FencingToken, Sequence: 1, Runtime: &tracker.NativeRuntimeObservation{Phase: "implementation", HeartbeatAt: now}}}
			for range test.observations {
				if err := execution.append(t.Context(), "run.observed", "", nil); err != nil {
					t.Fatal(err)
				}
				execution.data.Runtime.Phase = "validation"
			}
			if test.checkpoint {
				if len(requests) != 0 {
					t.Fatal("observation uploaded before its boundary")
				}
				var saved nativeCompletionState
				if err := json.Unmarshal(execution.CompletionState(), &saved); err != nil || len(saved.Observations) != 2 {
					t.Fatalf("buffered observations missing from completion custody: %v", err)
				}
				execution.storedSeq = execution.data.Sequence + 1
				err := execution.append(t.Context(), "run.checkpointed", "", &tracker.NativeCheckpoint{Resume: "fresh_checkout"})
				if test.fail {
					if err == nil || execution.pending == nil || len(execution.observations) != 2 {
						t.Fatal("failed upload discarded the pending batch")
					}
					if err := execution.flush(t.Context()); err != nil {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if test.heartbeat {
				if len(requests) != 0 {
					t.Fatal("observation uploaded before heartbeat cadence")
				}
				now = now.Add(20 * time.Second)
				execution.data.Runtime.HeartbeatAt = now
				if err := execution.append(t.Context(), "run.observed", "", nil); err != nil {
					t.Fatal(err)
				}
			}
			wantRequests := 1
			if test.fail {
				wantRequests = 2
			}
			if len(requests) != wantRequests {
				t.Fatalf("requests=%d want %d", len(requests), wantRequests)
			}
			if test.fail && !bytes.Equal(requests[0], requests[1]) {
				t.Fatal("retry changed the immutable batch or retry identity")
			}
			var batch tracker.NativeRunEvent
			if err := json.Unmarshal(requests[len(requests)-1], &batch); err != nil {
				t.Fatal(err)
			}
			wantPrior := test.observations
			if !test.checkpoint && !test.heartbeat {
				wantPrior--
			}
			if len(batch.PriorEvents) != wantPrior || batch.PriorEvents[0].Data.Runtime.Phase != "implementation" {
				t.Fatal("batch lost observations or retained mutable runtime pointers")
			}
			for i, event := range batch.PriorEvents {
				if event.Data.Sequence != int64(i+2) {
					t.Fatal("batch sequence changed")
				}
			}
			if len(execution.observations) != 0 || execution.pending != nil {
				t.Fatal("accepted boundary retained a pending batch")
			}
		})
	}
}
