package runner

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace"
)

type sshLandingTestLander func(context.Context, []workspace.LandRequest) []workspace.LandOutcome

func (land sshLandingTestLander) LandChanges(ctx context.Context, requests []workspace.LandRequest) []workspace.LandOutcome {
	return land(ctx, requests)
}

func TestSSHNativeLandingBatch(t *testing.T) {
	t.Parallel()
	batch := workspace.NewLandingBatch(t.Context())
	var calls, validated atomic.Int32
	lander := sshLandingTestLander(func(ctx context.Context, requests []workspace.LandRequest) []workspace.LandOutcome {
		calls.Add(1)
		outcomes := make([]workspace.LandOutcome, len(requests))
		for i, request := range requests {
			if err := request.Validate(ctx); err != nil {
				outcomes[i].Err = err
				continue
			}
			validated.Add(1)
			if i == 1 {
				outcomes[i].Err = &workspace.LandRefusal{Kind: workspace.LandRefusalConflict, Reason: "one member conflicts"}
			} else {
				outcomes[i].Result = workspace.LandResult{MergeSHA: request.Options.HeadSHA, Path: "batch_member"}
			}
		}
		return outcomes
	})
	type completion struct {
		i      int
		result workspace.LandResult
		err    error
	}
	done := make(chan completion, 10)
	for i := range 10 {
		head := strings.Repeat(string(rune('a'+i%6)), 40)
		target := NativeLandingTarget{ChangeID: "change", VersionID: "version", HeadSHA: head, Repository: "https://github.com/example/repo", Method: "squash"}
		execution := &landingRunExecution{landingStub: landingStub{target: target}}
		request := RunRequest{Execution: execution, LandingBatch: batch.Add()}
		callbacks := (&Runner{}).SSHRunCallbacks(request)
		left, right := net.Pipe()
		central := NewSSHPeer(t.Context(), left, left, callbacks.Handle)
		sources := &SSHExecutionSources{}
		remote := NewSSHPeer(t.Context(), right, right, sources.Handle)
		callbacks.BindExecutionSources(central, t.TempDir())
		t.Cleanup(func() { central.Close(); remote.Close(); left.Close(); right.Close() })
		wire := NewSSHRunRequest(request)
		wire.Native = true
		raw, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		var transported SSHRunRequest
		if err := json.Unmarshal(raw, &transported); err != nil {
			t.Fatal(err)
		}
		transported.Sources = sources
		bound := transported.Bind(t.Context(), remote)
		remoteExecution := bound.Execution.(BatchLandingExecution)
		if bound.LandingBatch != nil || !remoteExecution.BatchLandingEnabled() {
			t.Fatal("batch did not survive SSH bootstrap")
		}
		go func() {
			result, err := remoteExecution.LandBatch(t.Context(), lander, workspace.LandRequest{Info: workspace.Info{Path: "remote"}, Options: workspace.LandOptions{Native: true, HeadSHA: head, Repository: target.Repository, Method: "squash"}})
			done <- completion{i, result, err}
		}()
	}
	batch.Seal()
	for range 10 {
		outcome := <-done
		if outcome.i == 1 {
			var refusal *workspace.LandRefusal
			if !errors.As(outcome.err, &refusal) || refusal.Kind != workspace.LandRefusalConflict {
				t.Fatalf("remote conflict lost type: %v", outcome.err)
			}
		} else if outcome.err != nil || outcome.result.Path != "batch_member" {
			t.Fatalf("remote member = %#v", outcome)
		}
	}
	if calls.Load() != 1 || validated.Load() != 10 {
		t.Fatalf("remote staging operations %d, authority checks %d", calls.Load(), validated.Load())
	}
}
