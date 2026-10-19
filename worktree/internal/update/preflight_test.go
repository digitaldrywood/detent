package update

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestCommandPreflight(t *testing.T) {
	t.Parallel()

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable() error = %v", err)
	}
	tests := []struct {
		name    string
		mode    string
		wantErr string
	}{
		{name: "candidate accepts configuration", mode: "success"},
		{name: "candidate rejects configuration", mode: "failure", wantErr: "candidate startup rejected"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			preflight := CommandPreflight("-test.run=^TestCommandPreflightHelper$", "--", "startup-preflight="+tt.mode)
			err := preflight(context.Background(), executable)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("preflight() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("preflight() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestCommandPreflightHelper(t *testing.T) {
	mode := ""
	for _, arg := range os.Args {
		if value, ok := strings.CutPrefix(arg, "startup-preflight="); ok {
			mode = value
			break
		}
	}
	switch mode {
	case "":
		return
	case "success":
		return
	case "failure":
		_, _ = fmt.Fprintln(os.Stderr, "candidate startup rejected")
		os.Exit(7)
	default:
		t.Fatalf("unexpected helper mode %q", mode)
	}
}
