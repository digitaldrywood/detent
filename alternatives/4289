package operatortool

import (
	"errors"
	"testing"

	"github.com/digitaldrywood/detent/internal/explain"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNumericReceiptChecksPersistedCorrelation(t *testing.T) {
	for _, test := range []struct {
		name    string
		local   int64
		runtime *tracker.NativeRuntimeObservation
		item    tracker.NativeWorkItemID
		want    error
	}{
		{"typed terminal identity", 42, nil, "wi_example", nil},
		{"older runtime identity", 0, &tracker.NativeRuntimeObservation{LocalAttemptID: 42}, "wi_example", nil},
		{"missing identity", 0, nil, "wi_example", explain.ErrNotFound},
		{"wrong typed identity", 43, &tracker.NativeRuntimeObservation{LocalAttemptID: 42}, "wi_example", explain.ErrNotFound},
		{"wrong item", 42, nil, "wi_other", ErrAccessDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := tracker.NativeRuntimeEvidence{Issue: tracker.NativeIssue{NativeReference: tracker.NativeReference{WorkItemID: test.item}}, Attempt: &tracker.NativeAttempt{LocalAttemptID: test.local, NativeRunData: tracker.NativeRunData{AttemptID: "attempt_example", Runtime: test.runtime}}}
			_, err := NativeRuntimeResult(WorkAttemptReceipt, WorkReadRequest{ProjectID: "prj_example", Reference: "wi_example", AttemptID: 42}, evidence)
			if !errors.Is(err, test.want) {
				t.Fatalf("receipt result = %v, want %v", err, test.want)
			}
		})
	}
}
