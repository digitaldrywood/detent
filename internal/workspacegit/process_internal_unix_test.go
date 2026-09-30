//go:build unix

package workspacegit

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestExecDoesNotWaitOnDescendantsHoldingPipes(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	tests := []struct {
		name       string
		timeout    time.Duration
		alias      string
		wantErr    bool
		wantStdout string
		maxElapsed time.Duration
	}{
		{name: "success leaves a background child holding stdout", timeout: time.Minute, alias: "!sleep 30 & echo done", wantStdout: "done", maxElapsed: 10 * time.Second},
		{name: "timeout kills git and its background child", timeout: 300 * time.Millisecond, alias: "!sleep 30 & sleep 30", wantErr: true, maxElapsed: 10 * time.Second},
		{name: "oversized output is refused", timeout: time.Minute, alias: "!head -c 17000000 /dev/zero", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			service := &Service{path: t.TempDir()}
			started := time.Now()
			outcome := service.exec(t.Context(), test.timeout, nil, "-c", "alias.probe="+test.alias, "probe")
			if elapsed := time.Since(started); test.maxElapsed > 0 && elapsed > test.maxElapsed {
				t.Fatalf("exec took %v; a descendant holding a pipe must not block it", elapsed)
			}
			if (outcome.err != nil) != test.wantErr {
				t.Fatalf("err = %v, want error %v (stderr %q)", outcome.err, test.wantErr, outcome.stderr)
			}
			if !test.wantErr && outcome.stdout != test.wantStdout {
				t.Fatalf("stdout = %q, want %q", outcome.stdout, test.wantStdout)
			}
			if len(outcome.stdout) > maxStdoutBytes {
				t.Fatalf("stdout kept %d bytes, cap %d", len(outcome.stdout), maxStdoutBytes)
			}
		})
	}
}
