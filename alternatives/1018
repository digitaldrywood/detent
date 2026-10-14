package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestTrackerLanes(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, tracker, wantError string
		want                     []string
		legacy                   bool
	}{
		{name: "ordered", tracker: "lanes: [{name: Backlog, role: holding}, {name: Todo, role: active}, {name: Blocked, role: holding}, {name: Done, role: terminal}]", want: []string{"Backlog", "Todo", "Blocked", "Done"}},
		{name: "legacy", tracker: "observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]", want: []string{"Backlog", "Blocked", "Todo", "Done"}, legacy: true},
		{name: "empty", tracker: "lanes: []", want: []string{}},
		{name: "mixed empty", tracker: "lanes: []\n  active_states: []", wantError: "cannot be combined"},
		{name: "mixed merged", tracker: "<<: {active_states: [Todo]}\n  lanes: []", wantError: "cannot be combined"},
		{name: "mixed null", tracker: "lanes: []\n  observed_states: null", wantError: "cannot be combined"},
		{name: "mixed terminal", tracker: "lanes: []\n  terminal_states: [Done]", wantError: "cannot be combined"},
		{name: "null lanes", tracker: "lanes: null", wantError: "must be a list"},
		{name: "unknown role", tracker: "lanes: [{name: Todo, role: working}]", wantError: ".role must be"},
		{name: "blank name", tracker: "lanes: [{name: ' ', role: active}]", wantError: ".name must not be blank"},
		{name: "duplicate name", tracker: "lanes: [{name: Todo, role: active}, {name: ' todo ', role: holding}]", wantError: "must be unique"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte("---\ntracker:\n  kind: hub_native\n  " + test.tracker + "\n---\nWork.\n"))
			if test.wantError != "" {
				if err == nil {
					err = workflow.Config.Validate()
				}
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := workflow.Config.KanbanStateNames(); !slices.Equal(got, test.want) {
				t.Fatalf("order = %v, want %v", got, test.want)
			}
			if slices.Contains(workflow.Config.ValidationWarnings(), LegacyLaneWarning) != test.legacy {
				t.Fatalf("warnings = %v", workflow.Config.ValidationWarnings())
			}
			for index, state := range workflow.Config.NativeWorkflowStates() {
				if state.Name != test.want[index] || state.Dispatchable != (state.Name == "Todo") || state.Terminal != (state.Name == "Done") {
					t.Fatalf("state = %#v", state)
				}
			}
			if len(test.want) == 0 {
				return
			}
			workflow.Definition.Layout = ProjectDefinitionSplit
			workflow.Definition.ConfigPath = "detent.yaml"
			descriptor, err := ResolvePolicy(workflow)
			if err != nil {
				t.Fatal(err)
			}
			if test.legacy {
				old := workflow
				old.Config.Tracker.Lanes = nil
				oldDescriptor, err := ResolvePolicy(old)
				if err != nil {
					t.Fatal(err)
				}
				if err := descriptor.Match(oldDescriptor); err != nil {
					t.Fatalf("legacy approval identity changed: %v", err)
				}
			}
			applied, err := ApplyNativePolicy(Workflow{Config: Default()}, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(applied.Config.KanbanStateNames(), test.want) {
				t.Fatal("approved policy lost lane order")
			}
		})
	}
}

func TestMigrateTrackerLanes(t *testing.T) {
	t.Parallel()
	legacy := "tracker:\n  kind: memory\n  observed_states: [Backlog, Blocked]\n  active_states: [Todo]\n  terminal_states: [Done]\n"
	for _, test := range []struct {
		name, raw string
		changed   bool
	}{
		{"yaml", "schema: 1\n" + legacy, true},
		{"frontmatter", "---\n" + legacy + "---\nKeep this prompt exactly.\n", true},
		{"crlf", strings.ReplaceAll("---\n"+legacy+"---\nPrompt.\r\n", "\n", "\r\n"), true},
		{"tracker alias", "schema: 1\nbase: &base {observed_states: [Backlog, Blocked], active_states: [Todo], terminal_states: [Done]}\ntracker: *base\n", true},
		{"tracker merge", "schema: 1\nbase: &base {observed_states: [Backlog, Blocked], active_states: [Todo], terminal_states: [Done]}\ntracker: {<<: *base, kind: memory}\n", true},
		{"partial defaults", "schema: 1\ntracker:\n  active_states: [Todo]\n", true},
		{"plan stop", "schema: 1\n" + legacy + "plan:\n  enabled: true\n", true},
		{"already migrated", "schema: 1\ntracker:\n  lanes: [{name: Todo, role: active}]\n", false},
		{"prompt only", "Complete the assigned work.\n", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			out, changed, err := MigrateTrackerLanes([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if changed != test.changed {
				t.Fatalf("changed = %t", changed)
			}
			if !changed && !bytes.Equal(out, []byte(test.raw)) {
				t.Fatal("no-op changed input")
			}
			if changed {
				before, err := splitProjectWorkflow([]byte(test.raw))
				if err != nil {
					t.Fatal(err)
				}
				after, err := splitProjectWorkflow(out)
				if err != nil {
					t.Fatal(err)
				}
				if before.hasFrontmatter && !bytes.Equal(before.prompt, after.prompt) {
					t.Fatal("prompt changed")
				}
				parse := func(raw []byte) Config {
					t.Helper()
					doc, err := splitProjectWorkflow(raw)
					if err != nil {
						t.Fatal(err)
					}
					if !doc.hasFrontmatter {
						raw = append(append([]byte("---\n"), raw...), []byte("---\n")...)
					}
					workflow, err := ParseWorkflow(raw)
					if err != nil {
						t.Fatal(err)
					}
					return workflow.Config
				}
				left, right := parse([]byte(test.raw)), parse(out)
				if !reflect.DeepEqual(left.NativeWorkflowStates(), right.NativeWorkflowStates()) {
					t.Fatal("migration changed workflow semantics")
				}
				if slices.Contains(right.ValidationWarnings(), LegacyLaneWarning) {
					t.Fatal("migration still warns")
				}
			}
			second, again, err := MigrateTrackerLanes(out)
			if err != nil || again || !bytes.Equal(second, out) {
				t.Fatalf("second migration changed output: %v", err)
			}
		})
	}
}

func TestMigrateTrackerLaneSamples(t *testing.T) {
	t.Parallel()
	for _, directory := range []string{"templates", "examples"} {
		err := filepath.WalkDir(filepath.Join("..", "..", "docs", directory), func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || (filepath.Ext(path) != ".yaml" && filepath.Ext(path) != ".md") {
				return nil
			}
			if strings.HasSuffix(path, "global.yaml") {
				return nil
			}
			t.Run(path, func(t *testing.T) {
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				doc, err := splitProjectWorkflow(raw)
				if err != nil {
					t.Fatal(err)
				}
				if filepath.Ext(path) == ".md" && !doc.hasFrontmatter {
					return
				}
				out, changed, err := MigrateTrackerLanes(raw)
				if err != nil || changed || !bytes.Equal(out, raw) {
					t.Fatalf("sample round-trip changed=%t error=%v", changed, err)
				}
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
