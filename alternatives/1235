package conversation

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func TestNormalizeExecution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   ExecutionStatus
		want ExecutionStatus
	}{
		{ExecutionIdle, ExecutionIdle},
		{ExecutionWaitingForRunner, ExecutionWaitingForRunner},
		{ExecutionStarting, ExecutionUnknown},
		{ExecutionRunning, ExecutionUnknown},
		{ExecutionWaitingInput, ExecutionUnknown},
		{ExecutionInterrupting, ExecutionUnknown},
		{ExecutionCompleted, ExecutionCompleted},
		{ExecutionInterrupted, ExecutionInterrupted},
		{ExecutionFailed, ExecutionFailed},
		{ExecutionUnknown, ExecutionUnknown},
		{ExecutionStatus(""), ExecutionStatus("")},
	}
	for _, test := range tests {
		t.Run(string(test.in), func(t *testing.T) {
			t.Parallel()
			if got := NormalizeExecution(test.in); got != test.want {
				t.Fatalf("NormalizeExecution(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestNormalizeDelivery(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   Delivery
		want Delivery
	}{
		{DeliverySaved, DeliverySaved},
		{DeliveryQueued, DeliveryQueued},
		{DeliverySending, DeliveryUnknown},
		{DeliverySent, DeliveryUnknown},
		{DeliveryDelivered, DeliveryDelivered},
		{DeliveryResponding, DeliveryUnknown},
		{DeliveryCompleted, DeliveryCompleted},
		{DeliveryInterrupted, DeliveryInterrupted},
		{DeliveryRejected, DeliveryRejected},
		{DeliveryFailed, DeliveryFailed},
		{DeliveryUnknown, DeliveryUnknown},
	}
	for _, test := range tests {
		t.Run(string(test.in), func(t *testing.T) {
			t.Parallel()
			if got := NormalizeDelivery(test.in); got != test.want {
				t.Fatalf("NormalizeDelivery(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestNormalizeQuestion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   QuestionStatus
		want QuestionStatus
	}{
		{QuestionPending, QuestionExpired},
		{QuestionSending, QuestionUnknown},
		{QuestionSent, QuestionSent},
		{QuestionAnswered, QuestionAnswered},
		{QuestionExpired, QuestionExpired},
		{QuestionUnknown, QuestionUnknown},
	}
	for _, test := range tests {
		t.Run(string(test.in), func(t *testing.T) {
			t.Parallel()
			if got := NormalizeQuestion(test.in); got != test.want {
				t.Fatalf("NormalizeQuestion(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

func TestNormalizeExecutionValue(t *testing.T) {
	t.Parallel()
	before := Execution{Status: ExecutionRunning, Owner: Owner{AttemptID: "att_1"}, Capabilities: Capabilities{Steer: true}}
	after := NormalizeExecutionValue(before)
	if after.Status != ExecutionUnknown {
		t.Fatalf("status = %q, want unknown", after.Status)
	}
	if after.Owner != before.Owner {
		t.Fatalf("owner must be preserved: %#v", after.Owner)
	}
	if after.Capabilities != (Capabilities{}) {
		t.Fatalf("capabilities must be cleared when execution is unknown: %#v", after.Capabilities)
	}
	idle := NormalizeExecutionValue(Execution{Status: ExecutionIdle, Capabilities: Capabilities{Answer: true}})
	if idle.Status != ExecutionIdle || !idle.Capabilities.Answer {
		t.Fatalf("idle execution must be untouched: %#v", idle)
	}
}

// TestExecutionResumeRoundTrip covers the resume field the hub stores in
// execution_json and reports on the execution resource: it records where a
// bound runner continued from (decisions section 10.4).
func TestExecutionResumeRoundTrip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		want string
	}{
		{name: "thread", want: ResumeThread},
		{name: "transcript", want: ResumeTranscript},
		{name: "none", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(Execution{Status: ExecutionRunning, Resume: test.want})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if !strings.Contains(string(encoded), `"resume":`+strconv.Quote(test.want)) {
				t.Fatalf("execution json = %s, want a resume of %q", encoded, test.want)
			}
			var decoded Execution
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			if decoded.Resume != test.want {
				t.Fatalf("decoded resume = %q, want %q", decoded.Resume, test.want)
			}
		})
	}
}

// TestNormalizeExecutionValueClearsResume covers a restart: the execution
// state that could not be established becomes unknown and no longer claims
// to have resumed anything.
func TestNormalizeExecutionValueClearsResume(t *testing.T) {
	t.Parallel()
	after := NormalizeExecutionValue(Execution{Status: ExecutionRunning, Resume: ResumeTranscript})
	if after.Status != ExecutionUnknown || after.Resume != "" {
		t.Fatalf("normalized = %#v, want unknown with no resume", after)
	}
}
