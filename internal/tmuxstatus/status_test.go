package tmuxstatus

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestEnabled(t *testing.T) {
	t.Parallel()
	trueValue := true
	falseValue := false
	tests := []struct {
		name       string
		tmux       string
		pane       string
		configured *bool
		want       bool
	}{
		{name: "outside tmux defaults off", want: false},
		{name: "outside tmux explicit on stays off", pane: "%7", configured: &trueValue, want: false},
		{name: "inside tmux without pane stays off", tmux: "/tmp/tmux-501/default,1,0", want: false},
		{name: "inside tmux defaults on", tmux: "/tmp/tmux-501/default,1,0", pane: "%7", want: true},
		{name: "inside tmux explicit on", tmux: "/tmp/tmux-501/default,1,0", pane: "%7", configured: &trueValue, want: true},
		{name: "inside tmux explicit off", tmux: "/tmp/tmux-501/default,1,0", pane: "%7", configured: &falseValue, want: false},
		{name: "blank tmux is absent", tmux: "  ", pane: "%7", want: false},
		{name: "blank pane is absent", tmux: "/tmp/tmux-501/default,1,0", pane: "  ", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Enabled(test.tmux, test.pane, test.configured); got != test.want {
				t.Fatalf("Enabled() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		counts telemetry.Counts
		want   string
	}{
		{name: "idle", want: "detent 0r/0q/0b"},
		{name: "active", counts: telemetry.Counts{Running: 2, Queue: 1, Blocked: 3}, want: "detent 2r/1q/3b"},
		{name: "large counts stay compact", counts: telemetry.Counts{Running: 120, Queue: 45, Blocked: 8}, want: "detent 120r/45q/8b"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Format(test.counts); got != test.want {
				t.Fatalf("Format() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStatusLifecycle(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: "@7\tDetent:2\n", windowName: "Detent:2"}
	status, err := newStatus(context.Background(), runner, "%7", discardLogger())
	if err != nil {
		t.Fatalf("newStatus() error = %v", err)
	}

	snapshot := telemetry.Snapshot{
		Running: []telemetry.Running{{}, {}},
		Queue:   []telemetry.Queued{{}},
		Blocked: []telemetry.Blocked{{}, {}, {}},
	}
	if err := status.Update(context.Background(), snapshot); err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if err := status.Update(context.Background(), snapshot); err != nil {
		t.Fatalf("duplicate Update() error = %v", err)
	}
	if err := status.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := status.Close(context.Background()); err != nil {
		t.Fatalf("duplicate Close() error = %v", err)
	}

	want := [][]string{
		{"display-message", "-t", "%7", "-p", "#{window_id}\t#{window_name}"},
		{"rename-window", "-t", "@7", "detent 2r/1q/3b"},
		{"display-message", "-t", "@7", "-p", "#{window_name}"},
		{"rename-window", "-t", "@7", "Detent:2"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

func TestStatusCorrectsExternalRenameOnNextUpdate(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: "@7\tDetent:2\n", windowName: "Detent:2"}
	status, err := newStatus(context.Background(), runner, "%7", discardLogger())
	if err != nil {
		t.Fatalf("newStatus() error = %v", err)
	}

	snapshot := telemetry.Snapshot{Counts: telemetry.Counts{Running: 2, Queue: 3}}
	if err := status.Update(context.Background(), snapshot); err != nil {
		t.Fatalf("first Update() error = %v", err)
	}
	runner.windowName = "external-rename"
	if err := status.Update(context.Background(), snapshot); err != nil {
		t.Fatalf("second Update() error = %v", err)
	}

	if got, want := runner.windowName, "detent 2r/3q/0b"; got != want {
		t.Fatalf("window name = %q, want %q", got, want)
	}
}

func TestStatusUpdateUsesCurrentBoardCounts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		snapshot telemetry.Snapshot
		want     string
	}{
		{
			name: "ignores aggregate blocked count",
			snapshot: telemetry.Snapshot{
				Counts: telemetry.Counts{Running: 7, Queue: 6, Blocked: 10},
			},
			want: "detent 7r/6q/0b",
		},
		{
			name: "current tracker rows override stale blocked runtime rows",
			snapshot: telemetry.Snapshot{
				Counts: telemetry.Counts{Running: 2, Queue: 3, Blocked: 15},
				BoardIssues: []telemetry.Issue{
					{ID: "blocked-1", State: "Blocked"},
					{ID: "blocked-2", State: "Blocked"},
					{ID: "stale-1", State: "Done"},
				},
				Blocked: []telemetry.Blocked{
					{Issue: telemetry.Issue{ID: "blocked-1", State: "Blocked"}},
					{Issue: telemetry.Issue{ID: "blocked-2", State: "Blocked"}},
					{Issue: telemetry.Issue{ID: "stale-1", State: "Blocked"}},
				},
			},
			want: "detent 2r/3q/2b",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			runner := &recordingRunner{output: "@7\tDetent:2\n"}
			status, err := newStatus(context.Background(), runner, "%7", discardLogger())
			if err != nil {
				t.Fatalf("newStatus() error = %v", err)
			}

			if err := status.Update(context.Background(), test.snapshot); err != nil {
				t.Fatalf("Update() error = %v", err)
			}

			if got := runner.calls[1][3]; got != test.want {
				t.Fatalf("window name = %q, want %q", got, test.want)
			}
		})
	}
}

func TestStatusRetriesFailedRename(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: "@7\tDetent:2\n"}
	status, err := newStatus(context.Background(), runner, "%7", discardLogger())
	if err != nil {
		t.Fatalf("newStatus() error = %v", err)
	}

	runner.err = errors.New("tmux unavailable")
	snapshot := telemetry.Snapshot{Counts: telemetry.Counts{Running: 1}}
	if err := status.Update(context.Background(), snapshot); err == nil {
		t.Fatal("Update() error = nil, want rename failure")
	}
	runner.err = nil
	if err := status.Update(context.Background(), snapshot); err != nil {
		t.Fatalf("retry Update() error = %v", err)
	}

	wantRename := []string{"rename-window", "-t", "@7", "detent 1r/0q/0b"}
	if len(runner.calls) != 3 || !reflect.DeepEqual(runner.calls[1], wantRename) || !reflect.DeepEqual(runner.calls[2], wantRename) {
		t.Fatalf("commands = %#v, want two rename attempts %#v", runner.calls, wantRename)
	}
}

func TestStatusLogsLifecycle(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		firstFails    bool
		firstCounts   telemetry.Counts
		secondCounts  telemetry.Counts
		wantFirstName string
	}{
		{
			name:          "logs first successful rename only",
			firstCounts:   telemetry.Counts{Running: 1},
			secondCounts:  telemetry.Counts{Running: 2},
			wantFirstName: "detent 1r/0q/0b",
		},
		{
			name:          "failed rename does not consume first success log",
			firstFails:    true,
			firstCounts:   telemetry.Counts{Running: 1},
			secondCounts:  telemetry.Counts{Running: 2},
			wantFirstName: "detent 2r/0q/0b",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			runner := &recordingRunner{output: "@7\tDetent:2\n"}
			status, err := newStatus(context.Background(), runner, "%7", logger)
			if err != nil {
				t.Fatalf("newStatus() error = %v", err)
			}

			if test.firstFails {
				runner.err = errors.New("tmux unavailable")
			}
			firstErr := status.Update(context.Background(), telemetry.Snapshot{Counts: test.firstCounts})
			if test.firstFails && firstErr == nil {
				t.Fatal("first Update() error = nil, want rename failure")
			}
			if !test.firstFails && firstErr != nil {
				t.Fatalf("first Update() error = %v", firstErr)
			}
			runner.err = nil
			if err := status.Update(context.Background(), telemetry.Snapshot{Counts: test.secondCounts}); err != nil {
				t.Fatalf("second Update() error = %v", err)
			}

			output := logs.String()
			if got := strings.Count(output, "msg=\"initialized tmux window status\""); got != 1 {
				t.Fatalf("initialization log count = %d, want 1; logs:\n%s", got, output)
			}
			if !strings.Contains(output, "window_id=@7 original_name=Detent:2") {
				t.Fatalf("initialization log missing window metadata; logs:\n%s", output)
			}
			if got := strings.Count(output, "msg=\"tmux window status renamed\""); got != 1 {
				t.Fatalf("rename log count = %d, want 1; logs:\n%s", got, output)
			}
			if !strings.Contains(output, "window_id=@7 name=\""+test.wantFirstName+"\"") {
				t.Fatalf("rename log missing first successful name %q; logs:\n%s", test.wantFirstName, output)
			}
		})
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type recordingRunner struct {
	output     string
	windowName string
	err        error
	calls      [][]string
}

func (r *recordingRunner) Output(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(args) == 5 && args[0] == "display-message" && args[4] == "#{window_name}" {
		return r.windowName + "\n", r.err
	}
	return r.output, r.err
}

func (r *recordingRunner) Run(_ context.Context, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	if r.err == nil && len(args) == 4 && args[0] == "rename-window" {
		r.windowName = args[3]
	}
	return r.err
}
