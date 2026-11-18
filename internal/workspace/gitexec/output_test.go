package gitexec

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestOutputWithExactEnv(t *testing.T) {
	dir := t.TempDir()
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir}
	t.Setenv("GIT_TRACE", "1")
	const input = "first\x00second\n"
	args := []string{"-c", "alias.example=!printf diagnostic >&2 && cat", "example"}
	combined, err := WithInput(t.Context(), dir, nil, false, input, args...)
	if err != nil || !strings.Contains(combined, "diagnostic") || !strings.Contains(combined, input) || !strings.Contains(combined, "trace:") {
		t.Fatalf("inherited environment and combined output: %q, %v", combined, err)
	}
	out, err := OutputWithExactEnv(t.Context(), dir, env, input, args...)
	if err != nil || string(out) != input {
		t.Fatalf("exact stdout: %q, %v", out, err)
	}
	env = append(env, "EXAMPLE_ENV=selected")
	t.Setenv("EXAMPLE_INHERITED", "ambient")
	out, err = OutputWithExactEnv(t.Context(), dir, env, "", "-c", `alias.example=!printf '%s/%s' "$EXAMPLE_ENV" "$EXAMPLE_INHERITED"`, "example")
	if err != nil || string(out) != "selected/" {
		t.Fatalf("exact environment: %q, %v", out, err)
	}
	out, err = OutputWithExactEnv(t.Context(), dir, env, input, "-c", "alias.example=!cat && echo private-diagnostic >&2 && exit 1", "example")
	if err == nil || len(out) != 0 || strings.Contains(err.Error(), "private-diagnostic") || strings.Contains(err.Error(), input) || strings.Contains(err.Error(), dir) {
		t.Fatal("failed command must discard data and redact its arguments, directory and diagnostics")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := OutputWithExactEnv(ctx, dir, env, input, args...); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled command: %v", err)
	}
}

func TestGitDiffStopErrorIgnoresCompletedProcess(t *testing.T) {
	t.Parallel()

	waitErr := errors.New("wait")

	tests := []struct {
		name    string
		killErr error
		waitErr error
		wantErr bool
	}{
		{
			name: "no kill error",
		},
		{
			name:    "already done",
			killErr: os.ErrProcessDone,
			waitErr: waitErr,
		},
		{
			name:    "kill denied after successful wait",
			killErr: os.ErrPermission,
		},
		{
			name:    "kill denied before failed wait",
			killErr: os.ErrPermission,
			waitErr: waitErr,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := gitDiffStopError(tt.killErr, tt.waitErr)
			if tt.wantErr {
				if err == nil {
					t.Fatal("gitDiffStopError() error = nil, want error")
				}
				if !errors.Is(err, tt.killErr) {
					t.Fatalf("gitDiffStopError() error = %v, want wrapped %v", err, tt.killErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("gitDiffStopError() error = %v, want nil", err)
			}
		})
	}
}
