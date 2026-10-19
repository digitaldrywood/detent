package hubclient

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRetainChangeValidationWithoutArtifactClient(t *testing.T) {
	head := strings.Repeat("a", 40)
	receipt := func(timings int) gate.CommandResult {
		validation := gate.CommandResult{Command: "make check-land", HeadSHA: head, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Evidence: &gate.CommandEvidence{}, Output: strings.Repeat("validation passed 世界\n", 2500), Pipeline: &gate.PipelineEvidence{}}
		for i := range timings {
			timing := gate.PipelineTiming{Stage: "finalization", Command: "go test -short -timeout=60s ./internal/example", HeadSHA: head, TreeSHA: validation.TreeSHA, StartedAt: time.Unix(1, int64(i)).UTC(), FinishedAt: time.Unix(2, int64(i)).UTC(), ExitCode: new(0), Execution: "executed"}
			timing.ReceiptID = timing.ID()
			validation.Pipeline.Timings = append(validation.Pipeline.Timings, timing)
		}
		return validation
	}
	t.Run("medium receipt is stored bounded inline", func(t *testing.T) {
		validation := receipt(180)
		bounded := tracker.PublicGateResult(validation)
		boundedRaw, _ := json.Marshal(bounded)
		originalRaw, _ := json.Marshal(validation)
		if len(boundedRaw) <= 64<<10 || len(boundedRaw) > operatortool.MaxResultBytes || len(originalRaw) <= len(boundedRaw) {
			t.Fatalf("fixture sizes: bounded=%d original=%d", len(boundedRaw), len(originalRaw))
		}
		input := tracker.ChangeVersionInput{Validation: &validation}
		if err := (&nativeExecution{}).retainChangeValidation(t.Context(), &input); err != nil {
			t.Fatal(err)
		}
		if input.Validation == &validation || !reflect.DeepEqual(*input.Validation, bounded) {
			t.Fatal("inline fallback kept the unbounded receipt")
		}
		stored, err := json.Marshal(input)
		if err != nil || len(stored) > operatortool.MaxResultBytes {
			t.Fatalf("stored input exceeds the Change result limit: bytes=%d error=%v", len(stored), err)
		}
	})
	t.Run("receipt beyond the result limit requires an artifact service", func(t *testing.T) {
		validation := receipt(1024)
		input := tracker.ChangeVersionInput{Validation: &validation}
		if err := (&nativeExecution{}).retainChangeValidation(t.Context(), &input); !errors.Is(err, artifact.ErrMissing) || input.Validation != &validation {
			t.Fatalf("oversized receipt without artifact service: error=%v replaced=%t", err, input.Validation != &validation)
		}
	})
}
