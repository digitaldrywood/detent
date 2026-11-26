package securityaudit

import (
	"strings"
	"testing"
	"time"
)

func TestBuildPromptCheckoutAvailable(t *testing.T) {
	t.Parallel()
	base := Snapshot{
		ProjectID:  "prj_example",
		IssueID:    "wi_example",
		Repository: "owner/repository",
		BaseSHA:    "base",
		HeadSHA:    "head",
		Diff:       "diff --git a/handler.go b/handler.go\n+ok\n",
		DiffBytes:  len("diff --git a/handler.go b/handler.go\n+ok\n"),
	}
	withoutCheckout, err := BuildPrompt(base, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withoutCheckout, "Do not use tools") {
		t.Fatalf("prompt without checkout should forbid tools")
	}

	withCheckoutSnap := base
	withCheckoutSnap.CheckoutAvailable = true
	withCheckout, err := BuildPrompt(withCheckoutSnap, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(withCheckout, "Do not use tools") {
		t.Fatalf("prompt with checkout still forbids tools")
	}
	if !strings.Contains(withCheckout, "read_repo_file and search_repo") {
		t.Fatalf("prompt with checkout missing read-only commit-tree tool guidance")
	}
	if !strings.Contains(ToolInstructionsForCheckout(true), "read_repo_file, search_repo") {
		t.Fatalf("ToolInstructionsForCheckout(true) = %q", ToolInstructionsForCheckout(true))
	}
	if ToolInstructionsForCheckout(false) != ToolInstructions() {
		t.Fatalf("ToolInstructionsForCheckout(false) diverged from ToolInstructions()")
	}

	now := time.Now().UTC()
	priorRun := Run{
		ID:                 42,
		InvocationID:       "inv_1",
		ProjectID:          "prj_example",
		Repository:         "owner/repository",
		BaseSHA:            "base",
		HeadSHA:            "head",
		ReviewerVersion:    "3",
		ReviewerDigest:     "a787e62910e1313879b866070b718f1e472101b1e7694f4345d4cf0a18814c5c",
		AuthenticationMode: AuthenticationSubscription,
		WorkerPID:          4200,
		WorkerPGID:         4200,
		WorkerStartedAt:    now.Add(time.Second),
		ProviderThreadID:   "thread-42",
		ProviderSessionID:  "session-42",
		ExitStatus:         ExitStatusSuccess,
		OutputBytes:        18,
		OutputDigest:       OutputDigest(`{"verdict":"pass"}`),
		Verdict:            VerdictPass,
		Summary:            "passed under prior v3 digest",
		StartedAt:          now,
		CompletedAt:        now.Add(2 * time.Second),
	}
	key := Key{ProjectID: "prj_example", Repository: "owner/repository", BaseSHA: "base", HeadSHA: "head"}
	if eval := EvaluateLandedSource(priorRun, key); !eval.Allowed {
		t.Fatalf("EvaluateLandedSource rejected prior v3 digest: %+v", eval)
	}
	if eval := Evaluate(priorRun, nil, key, "", []string{"p1", "p2"}); eval.Allowed || eval.Reason != ReasonReviewerMismatch {
		t.Fatalf("Evaluate unexpectedly accepted prior v3 digest: %+v", eval)
	}
}
