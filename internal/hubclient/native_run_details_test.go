package hubclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type archivedRunDetails struct {
	mu       sync.Mutex
	archives map[string][]byte
}

func (s *archivedRunDetails) StoreDetail(_ context.Context, organization string, data []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(data))
	if err != nil {
		return ref, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.archives == nil {
		s.archives = make(map[string][]byte)
	}
	s.archives[ref.Key] = bytes.Clone(compressed)
	return ref, nil
}

func (s *archivedRunDetails) ReadDetail(_ context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ref.Validate(organization) != nil {
		return nil, attachment.ErrInvalid
	}
	s.mu.Lock()
	compressed := bytes.Clone(s.archives[ref.Key])
	s.mu.Unlock()
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(reader)
	if err := errors.Join(err, reader.Close()); err != nil {
		return nil, err
	}
	if int64(len(data)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return data, nil
}

func TestNativeArchiveBackedUsagePublication(t *testing.T) {
	for _, test := range []struct {
		name                         string
		buffered, lostAcknowledgment bool
	}{
		{name: "empty observation batch"},
		{name: "buffered observation then empty batch", buffered: true},
		{name: "lost usage acknowledgment", lostAcknowledgment: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			details := &archivedRunDetails{}
			h := newNativeChangeHubConfigured(t, "In Review", []tracker.NativeState{
				{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}},
				{Name: "In Progress", Dispatchable: true, Transitions: []string{"In Review", "Done"}},
				{Name: "In Review", Transitions: []string{"In Progress", "Done"}},
				{Name: "Done", Terminal: true},
			}, true, false, hubserver.Config{DatabasePath: hubDatabasePath(t), InitialAdminToken: []byte(nativeChangeAdminToken), RunDetails: details})
			issue := h.createInProgress(t, "Record provider usage")
			h.claim(t, issue.ID)
			execution := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			execution.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) {
				return tracker.AttemptDiffRequest{BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("c", 40), Files: []tracker.AttemptDiffFile{{Path: "README.md", Status: tracker.DiffStatusModified, Additions: 1, Patch: "@@ -1 +1 @@\n-source\n+updated source\n"}}}, true
			})
			if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: "implement", Backend: "codex", Model: "gpt-6-astra"}); err != nil {
				t.Fatal(err)
			}
			checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", HeadSHA: strings.Repeat("c", 40), WorkspaceDigest: strings.Repeat("b", 64), ExternalEffect: "none", EffectState: "none"}
			if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatal(err)
			}
			if test.buffered {
				if err := execution.ObserveRuntime(t.Context(), tracker.NativeRuntimeObservation{Phase: "validation", HeartbeatAt: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				if len(execution.observations) == 0 {
					t.Fatal("fixture did not buffer a runtime observation")
				}
			}
			transport := &executionTransport{next: h.native.client.httpClient.Transport}
			h.native.client.httpClient.Transport = transport
			transport.drop.Store(test.lostAcknowledgment)
			paid := int64(1000)
			entry := tracker.NativeUsage{Provider: "codex", Model: "gpt-6-astra", Input: 100, Output: 20, CostEstimate: 0.01, ReportedCostMicros: &paid, CostSource: "runner_report", Currency: "USD"}
			usage := runner.UsageExecution(execution)
			for update := int64(1); update <= 2; update++ {
				if err := usage.RecordUsage(t.Context(), entry); err != nil {
					t.Fatal(err)
				}
				running, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), execution.data.AttemptID)
				if err != nil {
					t.Fatal(err)
				}
				if running.Attempt == nil || running.Attempt.Status != "running" || len(running.Attempt.Usage) != 1 {
					t.Fatalf("usage update %d did not reach the running attempt: %+v", update, running.Attempt)
				}
				stored := running.Attempt.Usage[0]
				if stored.Input != 100*update || stored.Output != 20*update || stored.CostEstimate <= 0 || stored.ReportedCostMicros == nil || *stored.ReportedCostMicros != paid*update {
					t.Fatalf("usage update %d lost or duplicated spend: %+v", update, stored)
				}
			}
			checkpoint.WorkspaceDigest = strings.Repeat("d", 64)
			if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
				t.Fatalf("usage prevented checkpoint publication: %v", err)
			}
			running, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), execution.data.AttemptID)
			if err != nil || running.Attempt == nil || running.Attempt.Checkpoint == nil || running.Attempt.Checkpoint.WorkspaceDigest != checkpoint.WorkspaceDigest {
				t.Fatalf("checkpoint was not retained after usage: %+v, %v", running.Attempt, err)
			}
			if err := execution.Finish(t.Context(), "succeeded"); err != nil {
				t.Fatalf("usage prevented completion: %v", err)
			}
			finished, err := h.admin.RuntimeEvidence(t.Context(), tracker.NativeWorkItemID(issue.ID), execution.data.AttemptID)
			if err != nil || finished.Attempt == nil || finished.Attempt.Status != "succeeded" || len(finished.Attempt.Usage) != 1 || finished.Attempt.Usage[0].Input != 200 || finished.Attempt.Usage[0].ReportedCostMicros == nil || *finished.Attempt.Usage[0].ReportedCostMicros != 2000 {
				t.Fatalf("completion lost usage: %+v, %v", finished.Attempt, err)
			}
			if execution.pending != nil || len(execution.observations) != 0 {
				t.Fatal("completed execution retained unpublished events")
			}
		})
	}
}

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
