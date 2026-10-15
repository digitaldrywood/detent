package runner

import (
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/toolcache"
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
	monorepo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(monorepo, "apps", "api"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(monorepo, "apps", "api", "go.mod"), []byte("module example.com/api\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cacheRoot := t.TempDir()
	cache, err := toolcache.NewManaged(t.Context(), cacheRoot, filepath.Join(cacheRoot, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	managedContext, release, err := cache.Use(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	tests := []struct {
		name      string
		workspace string
		budget    gobudget.Budget
		hostFlags string
		hostProcs string
		variables map[string]string
		want      map[string]string
		managed   bool
	}{
		{
			name:      "disabled budget leaves environment",
			workspace: goModule,
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1"},
		},
		{
			name:      "disabled budget preserves nested module overrides",
			workspace: monorepo,
			hostFlags: "-mod=mod",
			hostProcs: "1",
			variables: map[string]string{"GOFLAGS": "-p=3", "GOMAXPROCS": "3"},
			want:      map[string]string{"GOFLAGS": "-p=3", "GOMAXPROCS": "3"},
		},
		{
			name:      "host GOFLAGS are preserved",
			workspace: goModule,
			budget:    budget,
			hostFlags: "-mod=mod",
			variables: map[string]string{"A": "1"},
			want: map[string]string{
				"A": "1", "GOFLAGS": "-mod=mod -p=2 -trimpath " + toolexec, "GOMAXPROCS": "2",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "host process limit is preserved",
			workspace: monorepo,
			budget:    budget,
			hostProcs: "1",
			want: map[string]string{
				"GOFLAGS": "-p=2 -trimpath " + toolexec, "GOMAXPROCS": "1",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "worker GOFLAGS and GOMAXPROCS win",
			workspace: goModule,
			budget:    budget,
			hostFlags: "-mod=mod",
			hostProcs: "1",
			variables: map[string]string{"GOFLAGS": "-p=1", "GOMAXPROCS": "3"},
			want: map[string]string{
				"GOFLAGS": "-p=1 -trimpath " + toolexec, "GOMAXPROCS": "3",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "workspace without root module retains budget for subprocesses",
			workspace: nonGo,
			budget:    budget,
			hostFlags: "-mod=mod",
			variables: map[string]string{"A": "1"},
			want: map[string]string{
				"A": "1", "GOFLAGS": "-mod=mod -p=2 -trimpath " + toolexec, "GOMAXPROCS": "2",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "nested Go module retains budget",
			workspace: monorepo,
			budget:    budget,
			want: map[string]string{
				"GOFLAGS": "-p=2 -trimpath " + toolexec, "GOMAXPROCS": "2",
				gobudget.DirEnvironment: dir, gobudget.SlotsEnvironment: "8",
			},
		},
		{
			name:      "managed cache reaches non-Go workspace",
			workspace: nonGo,
			managed:   true,
			variables: map[string]string{"A": "1"},
			want:      map[string]string{"A": "1", "GOCACHE": cacheRoot},
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
				goBudget: tt.budget,
				logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
				lookupEnv: func(key string) string {
					return map[string]string{"GOFLAGS": tt.hostFlags, "GOMAXPROCS": tt.hostProcs}[key]
				},
			}
			input := procgroup.Environment{Variables: tt.variables, PathPrefixes: []string{"/shim"}}

			ctx := t.Context()
			if tt.managed {
				ctx = managedContext
			}
			got := r.withGoBudget(ctx, tt.workspace, input)

			if runtime.GOOS != "windows" {
				command := exec.CommandContext(ctx, "/bin/sh", "-c", `printf '%s\n' "$GOFLAGS" "$GOMAXPROCS" "$DETENT_GO_BUDGET_DIR" "$DETENT_GO_BUDGET_SLOTS"`)
				command.Dir = tt.workspace
				if tt.workspace == monorepo {
					command.Dir = filepath.Join(monorepo, "apps", "api")
				}
				procgroup.SetEnvironment(command, procgroup.Environment{Variables: map[string]string{
					"GOFLAGS": tt.hostFlags, "GOMAXPROCS": tt.hostProcs,
					gobudget.DirEnvironment: "", gobudget.SlotsEnvironment: "",
				}})
				procgroup.SetEnvironment(command, got)
				output, err := command.CombinedOutput()
				if err != nil {
					t.Fatalf("worker child failed: %v: %s", err, output)
				}
				wantFlags, wantProcs := tt.hostFlags, tt.hostProcs
				if value, ok := tt.want["GOFLAGS"]; ok {
					wantFlags = value
				}
				if value, ok := tt.want["GOMAXPROCS"]; ok {
					wantProcs = value
				}
				want := strings.Join([]string{wantFlags, wantProcs, tt.want[gobudget.DirEnvironment], tt.want[gobudget.SlotsEnvironment], ""}, "\n")
				if string(output) != want {
					t.Fatalf("worker child environment = %q, want %q", output, want)
				}
			}
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
