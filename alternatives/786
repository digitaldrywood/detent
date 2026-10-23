package cli

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestHubProfilingReload(t *testing.T) {
	if testing.Short() {
		t.Skip("live service, profiling, or filesystem watcher integration")
	}

	for _, source := range []string{"config flag", "CONFIG", "hosted config"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "config.yaml")
			write := func(block string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("profiling: "+block+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			write("{capture: {enabled: true, interval: 300ms, cpu_duration: 1ms}}")
			cmd := &cobra.Command{}
			cmd.SetContext(t.Context())
			hostedPath := ""
			env := func(string) string { return "" }
			switch source {
			case "config flag":
				cmd.Flags().String("config", path, "")
			case "CONFIG":
				env = func(name string) string {
					if name == "CONFIG" {
						return path
					}
					return ""
				}
			case "hosted config":
				hostedPath = path
			}
			closeProfiling := startHubProfiling(cmd, env, hostedPath, filepath.Join(root, "hub.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
			t.Cleanup(closeProfiling)
			waitProfilingCondition(t, func() bool {
				matches, _ := filepath.Glob(filepath.Join(root, "profiles", "hub", "*", "heap.pprof"))
				return len(matches) > 0
			})
			write("{capture: {enabled: true, interval: 400ms, cpu_duration: 1ms, dir: replacement}}")
			waitProfilingCondition(t, func() bool {
				matches, _ := filepath.Glob(filepath.Join(root, "replacement", "*", "block.pprof"))
				return len(matches) > 0
			})
			write("{listen_addr: '0.0.0.0:0'}")
			waitProfilingCondition(t, func() bool {
				matches, _ := filepath.Glob(filepath.Join(root, "replacement", "*", "block.pprof"))
				return len(matches) > 1
			})
			if runtime.SetMutexProfileFraction(-1) != 5 {
				t.Fatal("invalid reload disabled working capture")
			}
			write("{}")
			waitProfilingCondition(t, func() bool { return runtime.SetMutexProfileFraction(-1) == 0 })
			closeProfiling()
		})
	}
}

func waitProfilingCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("profiling reload did not apply")
		case <-ticker.C:
		}
	}
}
