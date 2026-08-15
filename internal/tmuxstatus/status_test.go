package tmuxstatus

import (
	"context"
	"errors"
	"reflect"
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
		configured *bool
		want       bool
	}{
		{name: "outside tmux defaults off", want: false},
		{name: "outside tmux explicit on stays off", configured: &trueValue, want: false},
		{name: "inside tmux defaults on", tmux: "/tmp/tmux-501/default,1,0", want: true},
		{name: "inside tmux explicit on", tmux: "/tmp/tmux-501/default,1,0", configured: &trueValue, want: true},
		{name: "inside tmux explicit off", tmux: "/tmp/tmux-501/default,1,0", configured: &falseValue, want: false},
		{name: "blank tmux is absent", tmux: "  ", want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := Enabled(test.tmux, test.configured); got != test.want {
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
	runner := &recordingRunner{output: "@7\tDetent:2\n"}
	status, err := newStatus(context.Background(), runner)
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
		{"display-message", "-p", "#{window_id}\t#{window_name}"},
		{"rename-window", "-t", "@7", "detent 2r/1q/3b"},
		{"rename-window", "-t", "@7", "Detent:2"},
	}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("commands = %#v, want %#v", runner.calls, want)
	}
}

func TestStatusRetriesFailedRename(t *testing.T) {
	t.Parallel()
	runner := &recordingRunner{output: "@7\tDetent:2\n"}
	status, err := newStatus(context.Background(), runner)
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

type recordingRunner struct {
	output string
	err    error
	calls  [][]string
}

func (r *recordingRunner) Output(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.output, r.err
}

func (r *recordingRunner) Run(_ context.Context, args ...string) error {
	r.calls = append(r.calls, append([]string(nil), args...))
	return r.err
}
