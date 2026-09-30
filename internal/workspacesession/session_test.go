package workspacesession_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestValidateID(t *testing.T) {
	t.Parallel()
	generated := workspacesession.NewID()
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "generated", value: generated, valid: true},
		{name: "lower hex", value: "ws_" + strings.Repeat("ab", 16), valid: true},
		{name: "no prefix", value: strings.Repeat("ab", 16)},
		{name: "wrong prefix", value: "conv_" + strings.Repeat("ab", 16)},
		{name: "short", value: "ws_" + strings.Repeat("ab", 15)},
		{name: "long", value: "ws_" + strings.Repeat("ab", 17)},
		{name: "not hex", value: "ws_" + strings.Repeat("zz", 16)},
		{name: "empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := workspacesession.ValidateID(test.value)
			if test.valid && err != nil {
				t.Fatalf("ValidateID(%q) = %v, want nil", test.value, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("ValidateID(%q) = nil, want an error", test.value)
			}
		})
	}
}

// TestTransition covers every legal move of section 18.1 and, by exhausting
// the remaining pairs, every illegal one. A transition table that drifts from
// the contract is the one way a workspace could end up held by two runners, so
// the legal set is written out here independently of the implementation's map.
func TestTransition(t *testing.T) {
	t.Parallel()
	legal := map[string][]string{
		workspacesession.StateRequested: {
			workspacesession.StateStarting,
			workspacesession.StateReady,
			workspacesession.StateRequested,
			workspacesession.StateClosing,
			workspacesession.StateFailed,
		},
		workspacesession.StateStarting: {
			workspacesession.StateReady,
			workspacesession.StateRequested,
			workspacesession.StateClosing,
			workspacesession.StateFailed,
		},
		workspacesession.StateReady: {
			workspacesession.StateIdle,
			workspacesession.StateUnreachable,
			workspacesession.StateRequested,
			workspacesession.StateClosing,
			workspacesession.StateFailed,
		},
		workspacesession.StateIdle: {
			workspacesession.StateReady,
			workspacesession.StateUnreachable,
			workspacesession.StateRequested,
			workspacesession.StateClosing,
			workspacesession.StateFailed,
		},
		workspacesession.StateUnreachable: {
			workspacesession.StateReady,
			workspacesession.StateRequested,
			workspacesession.StateClosed,
			workspacesession.StateFailed,
		},
		workspacesession.StateClosing: {
			workspacesession.StateClosed,
			workspacesession.StateFailed,
		},
		workspacesession.StateClosed: {},
		workspacesession.StateFailed: {},
	}
	for _, from := range workspacesession.States() {
		for _, to := range workspacesession.States() {
			want := slices.Contains(legal[from], to)
			if got := workspacesession.Transition(from, to); got != want {
				t.Errorf("Transition(%q, %q) = %v, want %v", from, to, got, want)
			}
		}
	}
	// A state the vocabulary does not name never transitions, in either
	// direction: an unknown state must not become a way past the machine.
	for _, state := range workspacesession.States() {
		if workspacesession.Transition(state, "elsewhere") {
			t.Errorf("Transition(%q, %q) = true, want false", state, "elsewhere")
		}
		if workspacesession.Transition("elsewhere", state) {
			t.Errorf("Transition(%q, %q) = true, want false", "elsewhere", state)
		}
	}
}

func TestTerminalAndOpen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state    string
		terminal bool
		bound    bool
	}{
		{state: workspacesession.StateRequested},
		{state: workspacesession.StateStarting, bound: true},
		{state: workspacesession.StateReady, bound: true},
		{state: workspacesession.StateIdle, bound: true},
		{state: workspacesession.StateUnreachable},
		{state: workspacesession.StateClosing},
		{state: workspacesession.StateClosed, terminal: true},
		{state: workspacesession.StateFailed, terminal: true},
	}
	for _, test := range tests {
		t.Run(test.state, func(t *testing.T) {
			t.Parallel()
			if got := workspacesession.Terminal(test.state); got != test.terminal {
				t.Fatalf("Terminal(%q) = %v, want %v", test.state, got, test.terminal)
			}
			if got := workspacesession.Open(test.state); got != !test.terminal {
				t.Fatalf("Open(%q) = %v, want %v", test.state, got, !test.terminal)
			}
			if got := workspacesession.Bound(test.state); got != test.bound {
				t.Fatalf("Bound(%q) = %v, want %v", test.state, got, test.bound)
			}
		})
	}
}

func TestReasonsAreClosed(t *testing.T) {
	t.Parallel()
	want := []string{
		"no_runner", "checkout_failed", "worktree_missing", "runner_restarted",
		"hub_restarted", "lease_lost", "capacity", "closed_by_actor", "expired",
	}
	got := workspacesession.Reasons()
	if len(got) != len(want) {
		t.Fatalf("Reasons() = %v, want %v", got, want)
	}
	for _, reason := range want {
		if !workspacesession.ValidReason(reason) {
			t.Errorf("ValidReason(%q) = false, want true", reason)
		}
	}
	if workspacesession.ValidReason("because") {
		t.Error("ValidReason(\"because\") = true, want false")
	}
}

func TestNormalizeRequires(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		input   []string
		want    []string
		wantErr bool
	}{
		{name: "empty takes the read-only default", want: []string{"files", "diff"}},
		{name: "ordered canonically", input: []string{"preview", "files"}, want: []string{"files", "preview"}},
		{name: "deduplicated", input: []string{"files", "files"}, want: []string{"files"}},
		{name: "trimmed and lowered", input: []string{"  Files "}, want: []string{"files"}},
		{name: "terminal is a capability", input: []string{"terminal"}, want: []string{"terminal"}},
		{name: "unknown refused", input: []string{"agents"}, wantErr: true},
		{name: "empty entry refused", input: []string{""}, wantErr: true},
		{name: "too many refused", input: []string{"files", "diff", "terminal", "preview", "exec", "git", "files"}, wantErr: true},
		{name: "exec is a capability a request may name", input: []string{"files", "exec"}, want: []string{"files", "exec"}},
		{name: "git is a capability a request may name", input: []string{"files", "git"}, want: []string{"files", "git"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := workspacesession.NormalizeRequires(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizeRequires(%v) = %v, want an error", test.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeRequires(%v) = %v", test.input, err)
			}
			if !slices.Equal(got, test.want) {
				t.Fatalf("NormalizeRequires(%v) = %v, want %v", test.input, got, test.want)
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		set       workspacesession.Capabilities
		requires  []string
		satisfies bool
		names     []string
	}{
		{
			name:     "read-only runner serves the default requires",
			set:      workspacesession.Capabilities{Files: true, Diff: true},
			requires: workspacesession.DefaultRequires(), satisfies: true,
			names: []string{"files", "diff"},
		},
		{
			name:     "a runner without a terminal cannot serve one",
			set:      workspacesession.Capabilities{Files: true, Diff: true},
			requires: []string{"terminal"},
			names:    []string{"files", "diff"},
		},
		{
			name: "everything serves everything",
			set: workspacesession.Capabilities{
				Terminal: true, Files: true, Diff: true, Preview: true, Exec: true, Git: true,
			},
			requires:  workspacesession.CapabilityNames(),
			satisfies: true,
			names:     []string{"terminal", "files", "diff", "preview", "exec", "git"},
		},
		{
			name:      "exec is held on its own",
			set:       workspacesession.Capabilities{Exec: true},
			requires:  []string{workspacesession.CapabilityExec},
			satisfies: true,
			names:     []string{"exec"},
		},
		{
			name:     "a runner with a terminal does not thereby serve exec",
			set:      workspacesession.Capabilities{Terminal: true},
			requires: []string{workspacesession.CapabilityExec},
			names:    []string{"terminal"},
		},
		{
			name:      "a runner that reports git serves the header's git group",
			set:       workspacesession.Capabilities{Files: true, Git: true},
			requires:  []string{"files", "git"},
			satisfies: true,
			names:     []string{"files", "git"},
		},
		{
			name:     "a runner without git cannot serve the git channel",
			set:      workspacesession.Capabilities{Files: true, Diff: true},
			requires: []string{"git"},
			names:    []string{"files", "diff"},
		},
		{
			// The two channels added on the same day are independent: exec runs
			// a command the project wrote down, git writes to the repository,
			// and a runner may serve either without the other.
			name:     "exec does not imply git",
			set:      workspacesession.Capabilities{Exec: true},
			requires: []string{workspacesession.CapabilityGit},
			names:    []string{"exec"},
		},
		{
			name:     "an unknown requirement is never held",
			set:      workspacesession.Capabilities{Terminal: true, Files: true, Diff: true, Preview: true, Git: true},
			requires: []string{"agents"},
			names:    []string{"terminal", "files", "diff", "preview", "git"},
		},
		{name: "nothing reported serves nothing", requires: []string{"files"}, names: []string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := test.set.Satisfies(test.requires); got != test.satisfies {
				t.Fatalf("Satisfies(%v) = %v, want %v", test.requires, got, test.satisfies)
			}
			if got := test.set.Names(); !slices.Equal(got, test.names) {
				t.Fatalf("Names() = %v, want %v", got, test.names)
			}
		})
	}
}

func TestCapabilitiesFrom(t *testing.T) {
	t.Parallel()
	got := workspacesession.CapabilitiesFrom([]string{"Files", " diff ", "agents"})
	want := workspacesession.Capabilities{Files: true, Diff: true}
	if got != want {
		t.Fatalf("CapabilitiesFrom = %+v, want %+v", got, want)
	}
}

func TestValidIsolationAndWorktree(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "user", "container"} {
		if !workspacesession.ValidIsolation(value) {
			t.Errorf("ValidIsolation(%q) = false, want true", value)
		}
	}
	if workspacesession.ValidIsolation("vm") {
		t.Error("ValidIsolation(\"vm\") = true, want false")
	}
	for _, value := range []string{"", "retained", "fresh"} {
		if !workspacesession.ValidWorktree(value) {
			t.Errorf("ValidWorktree(%q) = false, want true", value)
		}
	}
	if workspacesession.ValidWorktree("borrowed") {
		t.Error("ValidWorktree(\"borrowed\") = true, want false")
	}
}

func TestOwnerZero(t *testing.T) {
	t.Parallel()
	if !(workspacesession.Owner{}).Zero() {
		t.Fatal("an empty owner tuple must report zero")
	}
	if (workspacesession.Owner{FencingToken: 1}).Zero() {
		t.Fatal("a tuple with a fencing token is not zero")
	}
}

func TestEventType(t *testing.T) {
	t.Parallel()
	for _, state := range workspacesession.States() {
		if got, want := workspacesession.EventType(state), "workspace."+state; got != want {
			t.Fatalf("EventType(%q) = %q, want %q", state, got, want)
		}
	}
}
