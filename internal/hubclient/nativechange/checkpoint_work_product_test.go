package nativechange

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeCheckpointWithoutChangeIsWorkProduct(t *testing.T) {
	h := newNativeChangeHub(t, true)
	issue := h.createInProgress(t, "Preserve finished source")
	h.claim(t, issue.ID)
	execution := h.scheduler.RunExecution(issue.ID)
	if err := execution.Start(t.Context(), tracker.NativeExecutionIdentity{Role: runner.RoleCode, Backend: "codex", Model: "test"}); err != nil {
		t.Fatal(err)
	}
	repository := nativeChangeSourceRepo(t)
	read := func(ref string) string {
		output, err := exec.CommandContext(t.Context(), "git", "-C", repository, "rev-parse", ref).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(output))
	}
	head := read("HEAD")
	ref := tracker.CheckpointRefPrefix + issue.ID
	nativeChangeGit(t, repository, "update-ref", ref, head)
	checkpoint := tracker.NativeCheckpoint{Resume: "fresh_checkout", Storage: "git_ref", Availability: "available", WorktreeState: "unpushed", HeadSHA: head, CommitSHA: head, TreeSHA: read("HEAD^{tree}"), BaseSHA: head, Ref: ref, ExternalEffect: "none", EffectState: "none"}
	if preserved, err := h.connector.HasPreservedWorkProduct(t.Context(), issue.ID); err != nil || preserved {
		t.Fatalf("missing source counted as work product: %t error=%v", preserved, err)
	}
	if err := execution.Checkpoint(t.Context(), checkpoint); err != nil {
		t.Fatal(err)
	}
	current, err := h.native.Issue(t.Context(), tracker.NativeWorkItemID(issue.ID))
	if err != nil || current.ListChange != nil || current.Change != nil {
		t.Fatalf("fixture unexpectedly has Change: %+v error=%v", current, err)
	}
	preserved, err := h.connector.HasPreservedWorkProduct(t.Context(), issue.ID)
	if err != nil || !preserved {
		t.Fatalf("checkpoint was lost as work product: %t error=%v", preserved, err)
	}
	if checkpoint.HeadSHA != read(ref) || checkpoint.TreeSHA != read(ref+"^{tree}") {
		t.Fatal("checkpoint source binding changed")
	}
}
