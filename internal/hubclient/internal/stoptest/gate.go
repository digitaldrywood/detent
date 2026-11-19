package stoptest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type GateExecution interface {
	runner.Execution
	runner.CompletionExecution
	runner.DiffExecution
	runner.ChangeSourceExecution
	CompletionData() tracker.NativeRunData
	RecordStep(context.Context, tracker.NativeStepReceipt) error
}

type GateFixture struct {
	GateExecution
	Scope     string
	Transport *testsupport.Transport
	Observe   func(context.Context) error
	Release   func(string) error
	Attempt   func() (tracker.NativeAttempt, error)
}

func GateMatrix(t *testing.T, fixture func(*testing.T) GateFixture) {
	for _, exit := range []int{0, 1} {
		for _, delivery := range []string{"accepted", "undelivered", "original wins", "cleanup wins"} {
			for _, proof := range []string{"current", "removed", "unreadable", "changed base", "first handoff"} {
				t.Run(fmt.Sprintf("exit%d/%s/%s", exit, delivery, proof), func(t *testing.T) { runGate(t, fixture(t), exit, delivery, proof) })
			}
		}
		for _, proof := range []string{"removed before diff", "unreadable before diff", "recapture failed", "recapture unavailable", "changed before event"} {
			t.Run(fmt.Sprintf("exit%d/%s", exit, proof), func(t *testing.T) { runGate(t, fixture(t), exit, "before diff", proof) })
		}
	}
}

func runGate(t *testing.T, f GateFixture, exit int, delivery, proof string) {
	t.Helper()
	if !f.Recovery().Lease.RunDetailBatching {
		t.Fatal("run detail batching is disabled")
	}
	if err := f.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	head, digest := strings.Repeat("b", 40), strings.Repeat("d", 64)
	baseHead := strings.Repeat("a", 40)
	live := func(context.Context) (tracker.AttemptDiffRequest, bool) {
		return tracker.AttemptDiffRequest{BaseSHA: baseHead, HeadSHA: head, WorkspaceDigest: digest, Files: []tracker.AttemptDiffFile{{Path: "changed.go", Status: tracker.DiffStatusModified, Additions: 1, Patch: "@@ -1 +1 @@"}}}, true
	}
	source := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: head, WorkspaceDigest: digest, ExternalEffect: "none", EffectState: "none"}
	recapture := proof == "recapture failed" || proof == "recapture unavailable"
	if recapture {
		source.WorktreeState = "clean"
	}
	if delivery != "before diff" || recapture || proof == "changed before event" {
		f.SetDiffSource(live)
	}
	if proof != "first handoff" {
		if err := f.Checkpoint(t.Context(), source); err != nil {
			t.Fatal(err)
		}
	}
	f.SetDiffSource(live)
	originalSeq := f.CompletionData().Sequence + 3
	worker, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	ctx := api.WithMutationAuthority(worker, f.Scope, f.Recovery().Lease, func() error { return context.Cause(worker) })
	for range 2 {
		if err := f.Observe(ctx); err != nil {
			t.Fatal(err)
		}
	}
	stopped := false
	var held *http.Request
	var original, standalone, diffs int
	next := f.Transport.Load()
	deliver := func(req *http.Request, status int) {
		res, err := next.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != status {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("delayed POST status %d want %d: %s", res.StatusCode, status, body)
		}
	}
	f.Transport.Store(testsupport.RoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			return next.RoundTrip(req)
		}
		if strings.HasSuffix(req.URL.Path, "/diff") {
			if stopped {
				diffs++
				res, err := next.RoundTrip(req)
				if proof == "changed before event" {
					baseHead = strings.Repeat("c", 40)
				}
				return res, err
			}
			return next.RoundTrip(req)
		}
		if !strings.HasSuffix(req.URL.Path, "/events") {
			return next.RoundTrip(req)
		}
		event := ReadEvent(t, req)
		if !stopped {
			original++
			if event.Type != "run.checkpointed" || event.Data.Sequence != originalSeq || event.IdempotencyKey != fmt.Sprintf("%s:%d", event.Data.AttemptID, originalSeq) || len(event.PriorEvents) != 2 || event.PriorEvents[0].Data.Sequence != originalSeq-2 || event.PriorEvents[1].Data.Sequence != originalSeq-1 || event.PriorEvents[0].Type != "run.observed" || event.PriorEvents[1].Type != "run.observed" || len(event.Data.Steps) != 1 {
				t.Fatalf("active POST %s seq %d, buffered %d steps %d", event.Type, event.Data.Sequence, len(event.PriorEvents), len(event.Data.Steps))
			}
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			held = req.Clone(t.Context())
			held.Body = io.NopCloser(bytes.NewReader(body))
			if delivery == "accepted" {
				deliver(held, http.StatusOK)
				held = nil
			}
			return nil, io.ErrUnexpectedEOF
		}
		if event.Type == "run.observed" || len(event.PriorEvents) != 0 {
			t.Fatal("stop replayed an ordinary observation")
		}
		if delivery == "original wins" && held != nil {
			deliver(held, http.StatusOK)
			held = nil
		}
		if event.Type == "run.checkpointed" {
			standalone++
		}
		return next.RoundTrip(req)
	}))
	if delivery == "before diff" {
		cancel(runner.ErrOperatorStopped)
		stopped = true
		switch proof {
		case "removed before diff":
			f.SetDiffSource(nil)
		case "unreadable before diff":
			f.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) { return tracker.AttemptDiffRequest{}, false })
		case "recapture failed", "recapture unavailable":
			f.SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error) {
				if proof == "recapture unavailable" {
					return tracker.ChangeSourceCapture{}, api.ErrUnavailable
				}
				return tracker.ChangeSourceCapture{}, errors.New("workspace unavailable")
			})
		}
		ctx = context.WithoutCancel(ctx)
	}
	result := gate.CommandResult{Command: "make check", HeadSHA: head, ExitCode: exit}
	err := f.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: source, CompletedAt: time.Now().UTC(), Validation: &result})
	if delivery != "before diff" && (!api.NativeTransportUnavailable(err) || original != 1) {
		t.Fatalf("original posts %d, error %v", original, err)
	}
	stopped = true
	cancel(runner.ErrOperatorStopped)
	switch proof {
	case "removed":
		f.SetDiffSource(nil)
	case "unreadable":
		f.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) { return tracker.AttemptDiffRequest{}, false })
	case "changed base":
		baseHead = strings.Repeat("c", 40)
	}
	if err := f.Release(runner.FinalStateOperatorStopped); err != nil {
		t.Fatalf("release: %v", err)
	}
	if held != nil && delivery == "cleanup wins" {
		deliver(held, http.StatusConflict)
	}
	attempt, err := f.Attempt()
	wantOriginal := delivery == "accepted" || delivery == "original wins"
	wantGate := wantOriginal || proof == "current" || proof == "first handoff"
	wantSequence := originalSeq - 2
	if wantOriginal {
		wantSequence = originalSeq + 1
	} else if wantGate {
		wantSequence++
	}
	if err != nil || attempt.ClaimReleasedAt == nil || attempt.Outcome != "interrupted" || attempt.Sequence != wantSequence {
		t.Fatalf("sequence %d want %d, outcome %s, released %v error %v", attempt.Sequence, wantSequence, attempt.Outcome, attempt.ClaimReleasedAt, err)
	}
	if wantGate {
		if len(attempt.Steps) != 1 || !reflect.DeepEqual(attempt.Steps[0].Validation, &result) || !reflect.DeepEqual(attempt.Steps[0].Source, source) || !reflect.DeepEqual(attempt.Checkpoint, &source) {
			t.Fatalf("gate %+v checkpoint %+v", attempt.Steps, attempt.Checkpoint)
		}
	} else if len(attempt.Steps) != 0 || standalone != 0 || !reflect.DeepEqual(attempt.Checkpoint, &source) {
		t.Fatalf("unverified gate %+v, standalone posts %d, checkpoint %+v", attempt.Steps, standalone, attempt.Checkpoint)
	}
	if delivery == "accepted" && (standalone != 0 || diffs != 0) {
		t.Fatalf("accepted gate replayed: checkpoints %d diffs %d", standalone, diffs)
	}
	if proof == "changed before event" && diffs != 1 {
		t.Fatalf("source changed between writes after %d diff POSTs", diffs)
	}
}
