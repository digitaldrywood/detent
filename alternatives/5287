package gitexec

import (
	"errors"
	"os"
	"testing"
)

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
