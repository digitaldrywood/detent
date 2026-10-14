package tui

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestModelCollapseBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		key     rune
		section dashboardSectionIndex
		label   string
	}{
		{key: '1', section: runningSection, label: "RUNNING"},
		{key: '2', section: queueSection, label: "QUEUE"},
		{key: '3', section: blockedSection, label: "BLOCKED"},
		{key: '4', section: completedSection, label: "COMPLETED"},
	}

	for _, tt := range tests {
		t.Run(string(tt.key), func(t *testing.T) {
			t.Parallel()

			model := newInteractiveTestModel(t)
			next, cmd := model.Update(keyPress(tt.key))
			if cmd != nil {
				t.Fatal("collapse binding returned a command")
			}
			model = next.(Model)
			if !model.collapsed[tt.section] {
				t.Fatalf("collapsed[%d] = false, want true", tt.section)
			}
			if !strings.Contains(model.renderDashboard(), "▸ "+tt.label) {
				t.Fatalf("collapsed view missing %s", tt.label)
			}

			next, _ = model.Update(keyPress(tt.key))
			if next.(Model).collapsed[tt.section] {
				t.Fatalf("collapsed[%d] remained true after second toggle", tt.section)
			}
		})
	}
}

func TestModelScrollBindingsClamp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		down tea.KeyPressMsg
		up   tea.KeyPressMsg
	}{
		{name: "vim keys", down: keyPress('j'), up: keyPress('k')},
		{name: "arrow keys", down: tea.KeyPressMsg(tea.Key{Code: tea.KeyDown}), up: tea.KeyPressMsg(tea.Key{Code: tea.KeyUp})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := newInteractiveTestModel(t)
			next, _ := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
			model = next.(Model)
			for range 10 {
				next, _ = model.Update(tt.down)
				model = next.(Model)
			}
			if model.offsets[queueSection] != 3 {
				t.Fatalf("queue offset = %d, want 3", model.offsets[queueSection])
			}
			if !strings.Contains(model.renderDashboard(), "#466") {
				t.Fatal("scrolled queue window does not contain its final row")
			}
			next, _ = model.Update(tt.up)
			model = next.(Model)
			if model.offsets[queueSection] != 2 {
				t.Fatalf("queue offset after up = %d, want 2", model.offsets[queueSection])
			}

			shrunk := model.snapshot
			shrunk.Queue = shrunk.Queue[:1]
			shrunk.Counts.Queue = 1
			next, _ = model.Update(snapshotMsg{snapshot: shrunk})
			model = next.(Model)
			if model.offsets[queueSection] != 0 {
				t.Fatalf("queue offset after snapshot shrink = %d, want 0", model.offsets[queueSection])
			}
		})
	}
}

func TestModelScrollClampsOnResizeAndRunningShrink(t *testing.T) {
	t.Parallel()

	model := newInteractiveTestModel(t)
	for range 10 {
		next, _ := model.Update(keyPress('j'))
		model = next.(Model)
	}
	if model.runningTable.Cursor() != 6 {
		t.Fatalf("running cursor = %d, want 6", model.runningTable.Cursor())
	}
	if !strings.Contains(model.renderDashboard(), "#482") {
		t.Fatal("scrolled running table does not contain its final row")
	}

	shrunk := model.snapshot
	shrunk.Running = shrunk.Running[:2]
	shrunk.Counts.Running = 2
	next, _ := model.Update(snapshotMsg{snapshot: shrunk})
	model = next.(Model)
	if model.runningTable.Cursor() != 1 {
		t.Fatalf("running cursor after snapshot shrink = %d, want 1", model.runningTable.Cursor())
	}

	model = newInteractiveTestModel(t)
	next, _ = model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	model = next.(Model)
	for range 10 {
		next, _ = model.Update(keyPress('j'))
		model = next.(Model)
	}
	if model.offsets[queueSection] != 3 {
		t.Fatalf("queue offset before resize = %d, want 3", model.offsets[queueSection])
	}
	next, _ = model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model = next.(Model)
	if model.offsets[queueSection] != 0 {
		t.Fatalf("queue offset after resize = %d, want 0", model.offsets[queueSection])
	}
}

func TestModelHelpBindings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		closeKey tea.KeyPressMsg
	}{
		{name: "unknown key", closeKey: keyPress('x')},
		{name: "question mark", closeKey: keyPress('?')},
		{name: "quit key", closeKey: keyPress('q')},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			model := newInteractiveTestModel(t)
			next, _ := model.Update(keyPress('?'))
			model = next.(Model)
			if !model.helpVisible || !strings.Contains(model.renderDashboard(), "HELP") {
				t.Fatal("help overlay did not open")
			}
			next, cmd := model.Update(tt.closeKey)
			model = next.(Model)
			if model.helpVisible {
				t.Fatal("help overlay did not close")
			}
			if cmd != nil {
				t.Fatal("key that closed help also triggered a command")
			}
		})
	}
}

func TestModelDashboardBindingInvokesLauncherOnce(t *testing.T) {
	t.Parallel()

	invocations := 0
	var launchedURL string
	model := newInteractiveTestModelWithOptions(t, WithDashboardLauncher(func(_ context.Context, url string) error {
		invocations++
		launchedURL = url
		return nil
	}))

	next, cmd := model.Update(keyPress('d'))
	model = next.(Model)
	if cmd == nil {
		t.Fatal("dashboard binding returned no command")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("dashboard command message = %T, want nil", msg)
	}
	if invocations != 1 {
		t.Fatalf("launcher invocations = %d, want 1", invocations)
	}
	if launchedURL != "http://localhost:4101" {
		t.Fatalf("launched URL = %q, want dashboard URL", launchedURL)
	}
	if model.focusedSection != runningSection {
		t.Fatal("dashboard binding changed focus")
	}
}

func TestDashboardCommand(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		goos     string
		wantArgs []string
		wantErr  bool
	}{
		{name: "darwin", goos: "darwin", wantArgs: []string{"open", "http://localhost:4101"}},
		{name: "linux", goos: "linux", wantArgs: []string{"xdg-open", "http://localhost:4101"}},
		{name: "windows", goos: "windows", wantArgs: []string{"rundll32", "url.dll,FileProtocolHandler", "http://localhost:4101"}},
		{name: "unsupported", goos: "plan9", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			command, err := dashboardCommand(context.Background(), tt.goos, "http://localhost:4101")
			if (err != nil) != tt.wantErr {
				t.Fatalf("dashboardCommand() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got := strings.Join(command.Args, "\x00"); got != strings.Join(tt.wantArgs, "\x00") {
				t.Fatalf("dashboardCommand() args = %q, want %q", command.Args, tt.wantArgs)
			}
		})
	}
}

func TestModelTabCyclesFocusAndUnknownKeysAreNoOps(t *testing.T) {
	t.Parallel()

	model := newInteractiveTestModel(t)
	for section := queueSection; section < dashboardSectionCount; section++ {
		next, cmd := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
		if cmd != nil {
			t.Fatal("tab returned a command")
		}
		model = next.(Model)
		if model.focusedSection != section {
			t.Fatalf("focused section = %d, want %d", model.focusedSection, section)
		}
		if !strings.Contains(model.renderDashboard(), "▶ "+[...]string{"RUNNING", "QUEUE", "BLOCKED", "COMPLETED"}[section]) {
			t.Fatalf("focused section %d is not highlighted", section)
		}
	}
	next, _ := model.Update(tea.KeyPressMsg(tea.Key{Code: tea.KeyTab}))
	model = next.(Model)
	if model.focusedSection != runningSection {
		t.Fatalf("wrapped focus = %d, want running", model.focusedSection)
	}

	before := model.renderDashboard()
	next, cmd := model.Update(keyPress('x'))
	if cmd != nil {
		t.Fatal("unknown key returned a command")
	}
	after := next.(Model)
	if after.renderDashboard() != before {
		t.Fatal("unknown key changed the rendered model")
	}
}

func TestModelNoColorRenderingPreservesStateLabels(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	model := newInteractiveTestModel(t)
	var output bytes.Buffer
	writer := colorprofile.Writer{Forward: &output, Profile: colorprofile.NoTTY}
	if _, err := fmt.Fprint(&writer, model.View().Content); err != nil {
		t.Fatalf("render no-color dashboard: %v", err)
	}
	rendered := output.String()
	if regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(rendered) {
		t.Fatalf("no-color output contains SGR sequences: %q", rendered)
	}
	for _, label := range []string{"● 7 running", "◐ 5 queued", "✗ 5 blocked", "✓ 10 completed"} {
		if !strings.Contains(rendered, label) {
			t.Fatalf("no-color output missing %q: %q", label, rendered)
		}
	}
}

func newInteractiveTestModel(t *testing.T) Model {
	t.Helper()
	return newInteractiveTestModelWithOptions(t)
}

func newInteractiveTestModelWithOptions(t *testing.T, opts ...Option) Model {
	t.Helper()

	model, err := NewModel(context.Background(), hub.New[telemetry.Snapshot](), opts...)
	if err != nil {
		t.Fatalf("NewModel() error = %v", err)
	}
	t.Cleanup(model.Close)
	next, _ := model.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	model = next.(Model)
	next, _ = model.Update(snapshotMsg{snapshot: busySnapshot()})
	return next.(Model)
}

func keyPress(key rune) tea.KeyPressMsg {
	return tea.KeyPressMsg(tea.Key{Code: key, Text: string(key)})
}
