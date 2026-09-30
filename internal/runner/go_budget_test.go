package runner

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestWithGoBudgetMergesWorkerEnvironment(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "budget")
	budget := gobudget.New(8, dir, "/opt/detent")
	toolexec := "-toolexec=" + budget.WrapperPath()
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	goModule := t.TempDir()
	if err := os.WriteFile(filepath.Join(goModule, "go.mod"), []byte("module example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonGo := t.TempDir()

	tests := []struct {
		name      string
		workspace string
		budget    gobudget.Budget
		hostFlags string
		variables map[string]string
		want      map[string]string
	}{
		{
			name:      "disabled budget leaves environment",
			workspace: goModule,
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
		{
			name:      "host GOFLAGS are preserved",
			workspace: goModule,
			budget:    budget,
			hostFlags: "-mod=mod",
			variables: map[string]string{"A": "1"},
			want: map[string]string{
				"A": "1", "GOFLAGS": "-mod=mod -p=2 " + toolexec, "GOMAXPROCS": "2",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "worker GOFLAGS and GOMAXPROCS win",
			workspace: goModule,
			budget:    budget,
			hostFlags: "-mod=mod",
			variables: map[string]string{"GOFLAGS": "-p=1", "GOMAXPROCS": "3"},
			want: map[string]string{
				"GOFLAGS": "-p=1 " + toolexec, "GOMAXPROCS": "3",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "non-Go workspace leaves environment",
			workspace: nonGo,
			budget:    budget,
			hostFlags: "-mod=mod",
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
		{
			name:      "missing workspace leaves environment",
			budget:    budget,
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
		{
			name:      "unavailable budget fails open",
			workspace: goModule,
			budget:    gobudget.New(2, filepath.Join(blocked, "budget"), "/opt/detent"),
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{
				goBudget:  tt.budget,
				logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
				lookupEnv: func(key string) string { return map[string]string{"GOFLAGS": tt.hostFlags}[key] },
			}
			input := procgroup.Environment{Variables: tt.variables, PathPrefixes: []string{"/shim"}}

			got := r.withGoBudget(tt.workspace, input)

			if len(got.Variables) != len(tt.want) {
				t.Fatalf("variables = %v, want %v", got.Variables, tt.want)
			}
			for key, value := range tt.want {
				if got.Variables[key] != value {
					t.Fatalf("variables[%s] = %q, want %q (all %v)", key, got.Variables[key], value, got.Variables)
				}
			}
			if len(got.PathPrefixes) != 1 || got.PathPrefixes[0] != "/shim" {
				t.Fatalf("PathPrefixes = %v, want preserved", got.PathPrefixes)
			}
		})
	}
}
