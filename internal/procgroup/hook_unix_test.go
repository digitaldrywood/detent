//go:build unix

package procgroup

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"testing"
)

func TestRunHook(t *testing.T) {
	if testing.Short() {
		t.Skip("process lifecycle integration")
	}
	for _, tt := range []struct {
		name    string
		command string
		cancel  bool
		code    int
		output  string
	}{
		{name: "output", command: "printf complete", output: "complete"},
		{name: "failure", command: "printf evidence >&2; exit 37", code: 37, output: "evidence"},
		{name: "signal", command: "kill -TERM $$", code: -1},
		{name: "canceled before start", command: "printf unexpected", cancel: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", tt.command)
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			Configure(ctx, cmd)
			if tt.cancel {
				cancel()
			}
			err := RunHook(ctx, cmd, nil)
			if tt.cancel {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error = %v, want canceled", err)
				}
			} else if tt.code == 0 {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != tt.code {
					t.Fatalf("error = %v, want exit code %d", err, tt.code)
				}
			}
			if output.String() != tt.output {
				t.Fatalf("output = %q, want %q", output.String(), tt.output)
			}
		})
	}
}
