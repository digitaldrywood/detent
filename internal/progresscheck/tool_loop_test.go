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
	}{
		{name: "exact", calls: "aaaaaa", actions: []int{3, 6}},
		{name: "alternating", calls: "abababababab", actions: []int{6, 12}},
		{name: "four calls", calls: "abcdabcdabcdabcdabcdabcd", actions: []int{12, 24}},
		{name: "unacknowledged", calls: "aaaaaaaaaaa", ackDelay: -1, actions: []int{3}},
		{name: "late acknowledgement", calls: "aaaaaaaaaa", ackDelay: 2, actions: []int{3, 8}},
		{name: "source or outcome changed", calls: "aaabbbbbb", actions: []int{3, 6, 9}},
		{name: "unknown breaks cycle", calls: "aaa aa aa aa", actions: []int{3}},
		{name: "distinct experiments", calls: "abcdefghijklmn"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var loop ToolLoop
			var actions []int
			var pending string
			var ackAt int
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
					pending, ackAt = evidence.Fingerprint, index+test.ackDelay
				}
				if index == ackAt {
					loop.Acknowledge(pending, uint64(index))
				}
				if duplicate := loop.Observe(uint64(index), fingerprint); duplicate.Action != "" {
					t.Fatalf("duplicate at %d produced %+v", index, duplicate)
				}
				if evidence.Action == ToolLoopStop {
					break
				}
			}
			if !slices.Equal(actions, test.actions) {
				t.Fatalf("interventions on calls %v, want %v", actions, test.actions)
			}
		})
	}
}

func TestToolExperimentFingerprint(t *testing.T) {
	base := ToolExperiment{Tool: "example", Source: "source-1", Status: "completed", Arguments: []byte(`{"a":1,"b":2}`), Result: []byte(`{"ok":true}`)}
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
		{name: "source", change: func(e *ToolExperiment) { e.Source = "source-2" }},
		{name: "missing source", change: func(e *ToolExperiment) { e.Source = "" }, incomplete: true},
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
