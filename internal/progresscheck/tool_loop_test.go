package progresscheck

import (
	"slices"
	"testing"
)

func TestToolLoop(t *testing.T) {
	for _, test := range []struct {
		name     string
		calls    string
		ackDelay int
		actions  []int
		stopAt   int
	}{
		{name: "exact", calls: "aaaaaa", actions: []int{3, 6}},
		{name: "alternating", calls: "abababababab", actions: []int{6, 12}},
		{name: "four calls", calls: "abcdabcdabcdabcdabcdabcd", actions: []int{12, 24}},
		{name: "four calls with a repeated suffix", calls: "aaabaaabaaabaaabaaabaaab", actions: []int{3, 7, 11, 12, 24}, stopAt: 24},
		{name: "unacknowledged", calls: "aaaaaaaaaaa", ackDelay: -1, actions: []int{3}},
		{name: "late acknowledgement", calls: "aaaaaaaaaa", ackDelay: 2, actions: []int{3, 8}},
		{name: "outcome changed", calls: "aaabbbbbb", actions: []int{3, 6, 9}},
		{name: "previous outcome returns in a new streak", calls: "aaabaaaaaa", actions: []int{3, 7, 10}},
		{name: "unknown breaks cycle", calls: "aaa aa aa aa", actions: []int{3}},
		{name: "distinct experiments", calls: "abcdefghijklmn"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var loop ToolLoop
			var actions []int
			var pending string
			var ackAt, stoppedAt int
			for i, call := range test.calls {
				index := i + 1
				fingerprint := string(call)
				if call == ' ' {
					fingerprint = ""
				}
				evidence := loop.Observe(uint64(index), fingerprint)
				if evidence.Action != "" {
					actions = append(actions, index)
				}
				if evidence.Action == ToolLoopSteer {
					loop.SteerSent(evidence)
					pending, ackAt = evidence.Fingerprint, index+test.ackDelay
				}
				if index == ackAt {
					loop.Acknowledge(pending, uint64(index))
				}
				if duplicate := loop.Observe(uint64(index), fingerprint); duplicate.Action != "" {
					t.Fatalf("duplicate at %d produced %+v", index, duplicate)
				}
				if evidence.Action == ToolLoopStop {
					stoppedAt = index
					break
				}
			}
			if !slices.Equal(actions, test.actions) {
				t.Fatalf("interventions on calls %v, want %v", actions, test.actions)
			}
			if test.stopAt != 0 && stoppedAt != test.stopAt {
				t.Fatalf("stop at call %d, want call %d", stoppedAt, test.stopAt)
			}
		})
	}
}

func TestToolExperimentFingerprint(t *testing.T) {
	base := ToolExperiment{Tool: "example", Status: "completed", Arguments: []byte(`{"a":1,"b":2}`), Result: []byte(`{"ok":true}`)}
	key := base.Fingerprint()
	if key == "" {
		t.Fatal("complete experiment has no fingerprint")
	}
	for _, test := range []struct {
		name             string
		change           func(*ToolExperiment)
		same, incomplete bool
	}{
		{name: "canonical keys", change: func(e *ToolExperiment) { e.Arguments = []byte(`{ "b":2, "a":1 }`) }, same: true},
		{name: "argument value", change: func(e *ToolExperiment) { e.Arguments = []byte(`{"a":2,"b":1}`) }},
		{name: "working directory", change: func(e *ToolExperiment) { e.WorkspaceDir = "subdir" }},
		{name: "outcome", change: func(e *ToolExperiment) { e.Result = []byte(`{"ok":false}`) }},
		{name: "exit code", change: func(e *ToolExperiment) { e.ExitCode = new(1) }},
		{name: "missing result", change: func(e *ToolExperiment) { e.Result = nil }, incomplete: true},
		{name: "partial result", change: func(e *ToolExperiment) { e.Result = []byte(`{"ok":`) }, incomplete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			experiment := base
			test.change(&experiment)
			got := experiment.Fingerprint()
			if (got == key) != test.same || (got == "") != test.incomplete {
				t.Fatalf("fingerprint %q, baseline %q", got, key)
			}
		})
	}
}
