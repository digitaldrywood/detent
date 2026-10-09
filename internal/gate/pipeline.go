package gate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
)

type pipelineRecorderKey struct{}

type pipelineRecorder struct {
	stage  string
	record func(PipelineTiming)
}

func WithPipelineRecorder(ctx context.Context, stage string, record func(PipelineTiming)) context.Context {
	return context.WithValue(ctx, pipelineRecorderKey{}, pipelineRecorder{stage: stage, record: record})
}

func ObservePipelineCommand(ctx context.Context, result *CommandResult) {
	if recorder, ok := ctx.Value(pipelineRecorderKey{}).(pipelineRecorder); ok && !result.StartedAt.IsZero() {
		result.TimingStage = recorder.stage
		recorder.record(result.PipelineTiming(recorder.stage))
	}
}

var pipelineCommand = regexp.MustCompile(`^(make|go|npm|pnpm|yarn|bun|cargo|just|task|pytest|python3?|bash|sh)( |$)|^\./scripts/`)

var pipelineReceiptID = regexp.MustCompile(`^timing_[a-f0-9]{32}$`)

type PipelineEvidence struct {
	Timings []PipelineTiming `json:"timings"`
	Dropped int              `json:"dropped,omitempty"`
}

func (p *PipelineEvidence) IsZero() bool { return p == nil || len(p.Timings) == 0 && p.Dropped == 0 }

type PipelineTiming struct {
	ReceiptID       string    `json:"receipt_id"`
	Stage           string    `json:"stage"`
	Command         string    `json:"command,omitempty"`
	HeadSHA         string    `json:"head_sha,omitempty"`
	TreeSHA         string    `json:"tree_sha,omitempty"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	ExitCode        *int      `json:"exit_code,omitempty"`
	Execution       string    `json:"execution,omitempty"`
	ReusedReceiptID string    `json:"reused_receipt_id,omitempty"`
	Outcome         string    `json:"outcome,omitempty"`
}

func (p *PipelineEvidence) Valid() bool {
	return p == nil || p.Dropped >= 0 && len(p.Timings) <= 1024 && !slices.ContainsFunc(p.Timings, func(t PipelineTiming) bool { return !t.Valid() })
}

func (r CommandResult) ValidPipeline(stage string) bool {
	return r.Pipeline.Valid() && (r.StartedAt.IsZero() && r.FinishedAt.IsZero() && r.TimingStage == "" || r.PipelineTiming(stage).Valid())
}

func (r CommandResult) PipelineTiming(stage string) PipelineTiming {
	if r.TimingStage != "" {
		stage = r.TimingStage
	}
	execution := r.Execution
	if execution == "" {
		execution = "executed"
	}
	t := PipelineTiming{ReceiptID: r.ReceiptID, Stage: stage, Command: r.Command, HeadSHA: r.HeadSHA, TreeSHA: r.TreeSHA, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, ExitCode: new(r.ExitCode), Execution: execution, ReusedReceiptID: r.ReusedReceiptID}
	if t.ReceiptID == "" {
		t.ReceiptID = t.ID()
	}
	return t.Public()
}

func (r CommandResult) Reused(stage string, at time.Time) CommandResult {
	source := r.PipelineTiming(r.TimingStage)
	r.TimingStage, r.Execution, r.ReusedReceiptID = stage, "reused", source.ReceiptID
	r.StartedAt, r.FinishedAt, r.ReceiptID = at, at, ""
	r.ReceiptID = r.PipelineTiming(stage).ReceiptID
	return r
}

func (t PipelineTiming) ID() string {
	t.ReceiptID = ""
	if t.Command != "" {
		t.Stage = ""
	}
	raw, err := json.Marshal(t)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return "timing_" + hex.EncodeToString(digest[:16])
}

func (t PipelineTiming) Public() PipelineTiming {
	if t.Command != "" {
		public := pipelineCommand.MatchString(t.Command) && publicCommand.MatchString(t.Command) && !privateCommandArgument.MatchString(t.Command)
		for _, argument := range strings.Fields(t.Command) {
			public = public && (!strings.Contains(argument, "=") || strings.HasPrefix(argument, "-")) && !strings.HasPrefix(argument, "/") && !strings.Contains(argument, ":/") && !strings.Contains(argument, "=/") && !strings.Contains(argument, "../")
		}
		if !public {
			t.Command = "[redacted]"
		}
	}
	return t
}

func (t PipelineTiming) Valid() bool {
	return pipelineReceiptID.MatchString(t.ReceiptID) && len(t.Command) <= 4096 &&
		slices.Contains([]string{"worker_check_land", "finalization", "landing_validation", "barrier", "repair", "rebase", "conflict_resolution", "merging_queue", "merging_to_landed", "barrier_claim"}, t.Stage) &&
		!t.StartedAt.IsZero() && !t.FinishedAt.Before(t.StartedAt) &&
		(t.HeadSHA == "" || evidenceSHA.MatchString(t.HeadSHA)) && (t.TreeSHA == "" || evidenceSHA.MatchString(t.TreeSHA)) &&
		(t.Execution == "" || t.Execution == "executed" || t.Execution == "reused") &&
		(t.Execution != "reused" || pipelineReceiptID.MatchString(t.ReusedReceiptID)) &&
		(t.Outcome == "" || slices.Contains([]string{"clean", "conflict", "agent-resolved", "rework"}, t.Outcome)) &&
		(t.ExitCode == nil || *t.ExitCode >= -1 && *t.ExitCode <= 255)
}

func PublicPipeline(timings []PipelineTiming) []PipelineTiming {
	result := make([]PipelineTiming, 0, len(timings))
	for _, timing := range timings {
		if timing.Valid() {
			result = append(result, timing.Public())
		}
	}
	return result
}

func Interval(stage string, started, finished time.Time, outcome string) PipelineTiming {
	timing := PipelineTiming{Stage: stage, StartedAt: started.UTC(), FinishedAt: finished.UTC(), Outcome: outcome}
	timing.ReceiptID = timing.ID()
	return timing
}

func MergePipeline(existing, incoming []PipelineTiming, dropped int) ([]PipelineTiming, int) {
	out := make([]PipelineTiming, 0, min(1024, len(existing)+len(incoming)))
	seen := map[string]bool{}
	for _, batch := range [][]PipelineTiming{existing, incoming} {
		for _, timing := range batch {
			if seen[timing.ReceiptID] {
				continue
			}
			seen[timing.ReceiptID] = true
			if !timing.Valid() || len(out) == 1024 {
				dropped++
				continue
			}
			out = append(out, timing.Public())
		}
	}
	return out, dropped
}
