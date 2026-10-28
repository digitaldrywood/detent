package hubclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/gate"
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

func TestNativeOversizedStepReceipt(t *testing.T) {
	for _, size := range []int{100, 400000, 500000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			h := newNativeChangeHub(t)
			store, publisher := changeEvidenceArtifactService(t, h)
			var attachmentBody []byte
			if size == 500000 {
				h.scheduler.client.artifactServiceID = ""
				transport := h.native.client.httpClient.Transport
				h.native.client.httpClient.Transport = executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					path := h.native.base() + "/attachments"
					evidence := strings.HasPrefix(request.URL.Path, h.native.base()+"/attempts/") && strings.HasSuffix(request.URL.Path, "/evidence")
					if !strings.HasPrefix(request.URL.Path, path) && !evidence {
						return transport.RoundTrip(request)
					}
					var body []byte
					if request.Method == http.MethodPost && evidence {
						var err error
						attachmentBody, err = io.ReadAll(request.Body)
						if err != nil {
							return nil, err
						}
						if request.Header.Get("Idempotency-Key") == "" {
							t.Error("attachment upload lacks retry identity")
						}
						encoded, err := base64.RawURLEncoding.DecodeString(request.Header.Get("X-Detent-Evidence"))
						var evidenceRequest attachment.EvidenceRequest
						if err != nil || json.Unmarshal(encoded, &evidenceRequest) != nil || evidenceRequest.LeaseID == "" || evidenceRequest.FencingToken <= 0 || evidenceRequest.IdempotencyKey != request.Header.Get("Idempotency-Key") || !strings.HasPrefix(evidenceRequest.IdempotencyKey, evidenceRequest.AttemptID+":step:") || !strings.Contains(request.URL.Path, evidenceRequest.AttemptID) {
							t.Fatal("step attachment lacks fenced attempt evidence authority")
						}
						metadata := attachment.Metadata{ID: "att_" + strings.Repeat("c", 32), ProjectID: string(h.project), Name: "execution-step.json", ContentType: "application/json", Size: int64(len(attachmentBody)), SHA256: artifact.Digest(attachmentBody)}
						metadata.Reference = metadata.Markdown(string(h.organization))
						body, err = json.Marshal(metadata)
						if err != nil {
							return nil, err
						}
					} else if request.Method == http.MethodGet {
						body = attachmentBody
					} else if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/reference") {
						t.Errorf("unexpected attachment request: %s", request.Method)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
				})
			}
			issue := h.createInProgress(t, "Retain validator evidence")
			h.claim(t, issue.ID)
			e := h.scheduler.RunExecution(issue.ID).(*nativeExecution)
			if err := e.PrepareArtifacts(t.Context(), nativeChangeSourceRepo(t)); err != nil {
				t.Fatal(err)
			}
			if err := e.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				t.Fatal(err)
			}
			receipt := tracker.NativeStepReceipt{Step: "validator", CompletedAt: time.Now().UTC(), Source: tracker.NativeCheckpoint{HeadSHA: strings.Repeat("a", 40), WorkspaceDigest: strings.Repeat("b", 64)}, Validator: &gate.ValidatorResult{Submitted: true, Verdict: "pass", Score: 1, Summary: "Checks passed", Commands: []gate.CommandResult{{Command: "go test ./...", Output: strings.Repeat("ok example/package\n", size/19)}}}}
			if err := e.Checkpoint(t.Context(), tracker.NativeCheckpoint{Resume: "fresh_checkout", Availability: "available", Storage: "local_only", WorktreeState: "clean", ExternalEffect: "none", EffectState: "none"}); err != nil {
				t.Fatal(err)
			}
			if size > 100 {
				receipt.Validator.Commands = nil
				for range 8 {
					receipt.Validator.Commands = append(receipt.Validator.Commands, gate.CommandResult{Command: "go test ./...", HeadSHA: receipt.Source.HeadSHA, ExitCode: 0, Output: strings.Repeat("ok example/package\n", 3000)})
				}
				receipt.PolicyID, receipt.ReviewState = e.data.PolicyID, e.stepReviewState()
				e.pending = &tracker.NativeRunEvent{Mutation: tracker.Mutation{IdempotencyKey: e.data.AttemptID + ":legacy-step"}, Type: "run.checkpointed", SchemaVersion: 1, Data: e.data}
				e.pending.Data.Sequence++
				e.pending.Data.Steps = []tracker.NativeStepReceipt{receipt}
				err := h.native.AppendEvent(t.Context(), tracker.NativeWorkItemID(issue.ID), *e.pending)
				var refusal *APIError
				if !errors.As(err, &refusal) || refusal.Rejection == nil || refusal.Rejection.Field != "data.steps.validator" {
					t.Fatalf("unbounded wire receipt was not rejected with field detail: %v", err)
				}
				if err := e.flush(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else if err := e.RecordStep(t.Context(), receipt); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(e.data.Steps[0])
			if err != nil || len(raw) > 256<<10 {
				t.Fatalf("wire receipt bytes=%d error=%v", len(raw), err)
			}
			recovery, err := h.native.Recovery(t.Context(), tracker.NativeWorkItemID(issue.ID))
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PublishPending(t.Context(), publisher); err != nil {
				t.Fatal(err)
			}
			if ref := e.data.Steps[0].PayloadAttachment; ref != nil {
				altered := e.data.Steps[0]
				changed := *ref
				changed.WorkItemID = tracker.NativeWorkItemID("wi_" + strings.Repeat("f", 32))
				altered.PayloadAttachment = &changed
				if _, err := e.restoreStep(t.Context(), altered); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("cross-item attachment accepted: %v", err)
				}
				saved := attachmentBody
				attachmentBody = bytes.Repeat([]byte("x"), len(saved))
				if _, err := e.restoreStep(t.Context(), e.data.Steps[0]); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("corrupt attachment accepted: %v", err)
				}
				attachmentBody = saved
			}
			if ref := e.data.Steps[0].PayloadArtifact; ref != nil {
				altered := e.data.Steps[0]
				changed := *ref
				changed.WorkItemID = "wi_" + strings.Repeat("f", 32)
				altered.PayloadArtifact = &changed
				if _, err := e.restoreStep(t.Context(), altered); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("cross-item payload accepted: %v", err)
				}
				altered = e.data.Steps[0]
				altered.Source.HeadSHA = strings.Repeat("f", 40)
				if _, err := e.restoreStep(t.Context(), altered); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("changed binding accepted: %v", err)
				}
			}
			e.data.Steps = nil
			e.claim.recovery = recovery
			restored, err := e.Step(t.Context(), receipt)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restored.Validator, receipt.Validator) {
				t.Fatal("retry lost validator evidence")
			}
		})
	}
}
