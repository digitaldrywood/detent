package stoptest

import (
	"context"
	"errors"
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
	for _, batch := range []string{"never arrived", "before stop", "before checkpoint"} {
		for _, source := range []string{"removed", "unreadable", "verifiable", "first handoff", "recapture failed"} {
			t.Run("source/"+batch+"/"+source, func(t *testing.T) {
				runGate(t, fixture(t), batch, source, "", "", "")
			})
		}
	}
	for _, finish := range []string{"prepared", "unprepared"} {
		for _, gateReply := range []string{"lost", "checkpoint never arrived"} {
			for _, interruption := range []string{"committed", "undelivered"} {
				t.Run("interruption/"+finish+"/"+gateReply+"/"+interruption, func(t *testing.T) {
					runGate(t, fixture(t), "acknowledged", "", finish, gateReply, interruption)
				})
			}
		}
	}
	t.Run("interruption/repeat outage", func(t *testing.T) {
		runGate(t, fixture(t), "acknowledged", "", "unprepared", "checkpoint never arrived", "outage")
	})
}

func runGate(t *testing.T, f GateFixture, batch, proof, finish, reply, interruption string) {
	t.Helper()
	lease := f.Recovery().Lease
	if !lease.RunDetailBatching {
		t.Fatal("run detail batching is disabled")
	}
	if err := f.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	head, digest := strings.Repeat("b", 40), strings.Repeat("d", 64)
	diff := func(context.Context) (tracker.AttemptDiffRequest, bool) {
		return tracker.AttemptDiffRequest{BaseSHA: strings.Repeat("a", 40), HeadSHA: head, WorkspaceDigest: digest, Files: []tracker.AttemptDiffFile{{Path: "changed.go", Status: tracker.DiffStatusModified, Additions: 1, Patch: "@@ -1 +1 @@"}}}, true
	}
	source := tracker.NativeCheckpoint{Resume: "resume_session", Storage: "local_only", Availability: "available", WorktreeState: "dirty", HeadSHA: head, WorkspaceDigest: digest, ExternalEffect: "none", EffectState: "none"}
	if proof == "recapture failed" {
		source.WorktreeState = "clean"
	}
	if proof == "recapture failed" || interruption != "" {
		f.SetDiffSource(diff)
	}
	if proof != "first handoff" {
		if err := f.Checkpoint(t.Context(), source); err != nil {
			t.Fatal(err)
		}
	}
	f.SetDiffSource(diff)
	if finish == "prepared" {
		if err := f.PrepareFinish(t.Context(), "interrupted", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	worker, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	ctx := api.WithMutationAuthority(worker, f.Scope, lease, func() error { return context.Cause(worker) })
	data := f.CompletionData()
	for range 2 {
		if err := f.Observe(ctx); err != nil {
			t.Fatal(err)
		}
	}
	trace := testsupport.TraceStoppedCheckpoint(t, f.Transport, batch, reply, data.AttemptID, data.Sequence, worker, nil, nil)
	result := gate.CommandResult{Command: "make check", HeadSHA: head, ExitCode: 1}
	err := f.RecordStep(ctx, tracker.NativeStepReceipt{Step: "validation", Source: source, CompletedAt: time.Now().UTC(), Validation: &result})
	if !api.NativeTransportUnavailable(err) {
		t.Fatalf("original request error = %v", err)
	}
	cancel(runner.ErrOperatorStopped)
	switch proof {
	case "removed":
		f.SetDiffSource(nil)
	case "unreadable":
		f.SetDiffSource(func(context.Context) (tracker.AttemptDiffRequest, bool) { return tracker.AttemptDiffRequest{}, false })
	case "recapture failed":
		f.SetChangeSource(func(context.Context, string, string) (tracker.ChangeSourceCapture, error) {
			return tracker.ChangeSourceCapture{}, errors.New("workspace unavailable")
		})
	}
	if interruption == "" && (len(trace.Sequences) != 0 || trace.DiffPosts != 0) {
		t.Fatal("gate reached Hub before source transition")
	}
	fault := interruptDelivery(t, f.Transport, interruption)
	err = f.Release(runner.FinalStateOperatorStopped)
	if interruption == "outage" {
		attempt, readErr := f.Attempt()
		if err == nil || readErr != nil || attempt.ClaimReleasedAt != nil || attempt.Outcome != "" || len(attempt.Steps) != 1 {
			t.Fatalf("outage released %v, outcome %q, steps %d, errors %v / %v", attempt.ClaimReleasedAt, attempt.Outcome, len(attempt.Steps), err, readErr)
		}
		fault.restore()
		err = f.Release("released")
	}
	if err != nil {
		t.Fatalf("release with checkpoint POSTs %v: %v", trace.Sequences, err)
	}
	wantGate := proof == "verifiable" || proof == "first handoff" || interruption != ""
	attempt, err := f.Attempt()
	wantSeq := data.Sequence + 1
	if batch != "never arrived" {
		wantSeq += 2
	}
	if wantGate {
		wantSeq++
	}
	if err != nil || attempt.ClaimReleasedAt == nil || attempt.Outcome != "interrupted" || attempt.Sequence != wantSeq || !reflect.DeepEqual(attempt.Checkpoint, &source) {
		t.Fatalf("released %v, outcome %q, sequence %d want %d, checkpoint %+v, error %v", attempt.ClaimReleasedAt, attempt.Outcome, attempt.Sequence, wantSeq, attempt.Checkpoint, err)
	}
	if wantGate {
		if len(attempt.Steps) != 1 || attempt.Steps[0].Step != "validation" || !reflect.DeepEqual(attempt.Steps[0].Validation, &result) || !reflect.DeepEqual(attempt.Steps[0].Source, source) {
			t.Fatalf("retained gate = %+v", attempt.Steps)
		}
	} else if len(attempt.Steps) != 0 || trace.DiffPosts != 0 || len(trace.Sequences) != 0 {
		t.Fatalf("unverified gate: steps %v, diff POSTs %d, checkpoints %v", attempt.Steps, trace.DiffPosts, trace.Sequences)
	}
	if interruption == "committed" || interruption == "undelivered" {
		wantPosts := 1
		if interruption == "undelivered" {
			wantPosts++
		}
		if fault.posts != wantPosts || fault.reads == 0 {
			t.Fatalf("interruption POSTs %d, fenced reads after loss %d", fault.posts, fault.reads)
		}
	}
}
