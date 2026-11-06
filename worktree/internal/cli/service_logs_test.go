package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/service"
)

func TestServiceLogRetention(t *testing.T) {
	t.Parallel()
	for _, manager := range []service.ManagerName{service.ManagerLaunchd, service.ManagerSystemd, service.ManagerScheduledTask} {
		t.Run(string(manager), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				configPath := filepath.Join(root, "global.yaml")
				outPath, errPath := service.LogPaths(service.Config{ConfigPath: configPath})
				stdout := openServiceLogFixture(t, outPath)
				defer stdout.Close()
				stderr := openServiceLogFixture(t, errPath)
				defer stderr.Close()
				inherited := openServiceLogFixture(t, errPath)
				defer inherited.Close()
				for _, path := range []string{configPath, filepath.Join(root, "identity.json"), filepath.Join(root, "source.go"), filepath.Join(root, "worker-artifact"), filepath.Join(root, "logs", "unrelated.log")} {
					if err := os.WriteFile(path, []byte("untouched"), 0o600); err != nil {
						t.Fatal(err)
					}
					defer assertServiceLogText(t, path, "untouched")
				}
				if err := os.Symlink(configPath, outPath+".1"); err != nil {
					t.Fatal(err)
				}
				write := func(index int) {
					t.Helper()
					for streamIndex, stream := range []*os.File{stdout, inherited} {
						if _, err := fmt.Fprintf(stream, "%s%08d%08d", strings.Repeat("x", 64), streamIndex, index); err != nil {
							t.Fatal(err)
						}
					}
				}
				write(0)
				stop := startServiceLogRetention(t.Context(), string(manager), configPath, logRotation{MaxSizeBytes: 16, MaxBackups: 2}, stdout, stderr)
				defer stop()
				for streamIndex, path := range []string{outPath, errPath} {
					assertServiceLogText(t, path, "")
					assertServiceLogText(t, path+".1", fmt.Sprintf("%08d00000000", streamIndex))
				}
				for index := 1; index <= 4; index++ {
					write(index)
					time.Sleep(serviceLogRetentionInterval)
					synctest.Wait()
				}
				for streamIndex, path := range []string{outPath, errPath} {
					assertServiceLogText(t, path, "")
					assertServiceLogText(t, path+".1", fmt.Sprintf("%08d00000004", streamIndex))
					assertServiceLogText(t, path+".2", fmt.Sprintf("%08d00000003", streamIndex))
					if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
						t.Fatalf("unexpected third backup: %v", err)
					}
					for _, file := range []struct {
						path string
						mode os.FileMode
					}{{path, 0o640}, {path + ".1", 0o600}, {path + ".2", 0o600}} {
						info, err := os.Stat(file.path)
						if err != nil || info.Mode().Perm() != file.mode {
							t.Fatalf("log permissions: %v, %v", info, err)
						}
					}
				}
				for _, stream := range []*os.File{stdout, inherited} {
					if _, err := stream.WriteString("continued"); err != nil {
						t.Fatal(err)
					}
				}
				assertServiceLogText(t, outPath, "continued")
				assertServiceLogText(t, errPath, "continued")
			})
		})
	}
}

func TestServiceLogRetentionSkipsOtherOutput(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"manual", "journal", "different file", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			configPath := filepath.Join(root, "global.yaml")
			path, _ := service.LogPaths(service.Config{ConfigPath: configPath})
			stream := openServiceLogFixture(t, path)
			defer stream.Close()
			if _, err := stream.WriteString("original output"); err != nil {
				t.Fatal(err)
			}
			manager := string(service.ManagerLaunchd)
			switch scenario {
			case "manual":
				manager = string(service.ManagerManual)
			case "journal":
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				defer writer.Close()
				stream = writer
			case "different file":
				stream = openServiceLogFixture(t, filepath.Join(root, "other.log"))
				defer stream.Close()
			case "symlink":
				target := filepath.Join(root, "source.go")
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			stop := startServiceLogRetention(t.Context(), manager, configPath, logRotation{MaxSizeBytes: 1, MaxBackups: 2}, stream, nil)
			stop()
			assertServiceLogText(t, path, "original output")
			if _, err := os.Stat(path + ".1"); !os.IsNotExist(err) {
				t.Fatalf("unexpected snapshot: %v", err)
			}
		})
	}
}

func TestServiceLogRetentionFiniteDefaults(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"unlimited size", "disabled history"} {
		t.Run(scenario, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "global.yaml")
			path, _ := service.LogPaths(service.Config{ConfigPath: configPath})
			stream := openServiceLogFixture(t, path)
			defer stream.Close()
			rotation := logRotation{MaxSizeBytes: 1}
			if scenario == "unlimited size" {
				rotation = logRotation{MaxBackups: 2}
				if err := stream.Truncate(int64(defaultRuntimeLogMaxSizeBytes) + 1); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := stream.WriteString("recent"); err != nil {
				t.Fatal(err)
			}
			stop := startServiceLogRetention(context.Background(), string(service.ManagerSystemd), configPath, rotation, stream, nil)
			stop()
			assertServiceLogText(t, path, "")
			info, err := os.Stat(path + ".1")
			wantSize := int64(1)
			if scenario == "unlimited size" {
				wantSize = int64(defaultRuntimeLogMaxSizeBytes)
			}
			if err != nil || info.Size() != wantSize {
				t.Fatalf("snapshot size: %v, %v", info, err)
			}
		})
	}
}

func TestServiceLogRetentionSnapshotFailurePreservesOutput(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"backup path", "copy", "close"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service.err.log")
			stream := openServiceLogFixture(t, path)
			defer stream.Close()
			if _, err := stream.WriteString("diagnostic stderr"); err != nil {
				t.Fatal(err)
			}
			rotation := logRotation{MaxSizeBytes: 4, MaxBackups: 2}
			copySnapshot := io.Copy
			if scenario == "backup path" {
				rotation.MaxBackups = 1
				if err := os.Mkdir(path+".1", 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(path+".1", "unrelated"), []byte("untouched"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				for index, text := range []string{"newer", "older"} {
					if err := os.WriteFile(rotatedLogPath(path, index+1), []byte(text), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				copySnapshot = func(writer io.Writer, reader io.Reader) (int64, error) {
					if scenario == "copy" {
						written, err := io.Copy(writer, io.LimitReader(reader, 2))
						if err != nil {
							return written, err
						}
						return written, io.ErrShortWrite
					}
					written, err := io.Copy(writer, reader)
					if err != nil {
						return written, err
					}
					return written, writer.(io.Closer).Close()
				}
			}
			log := serviceLog{path: path, stream: stream}
			for range 3 {
				if err := log.retainWithCopy(rotation, copySnapshot); err == nil {
					t.Fatal("retention succeeded with a snapshot failure")
				}
				assertServiceLogText(t, path, "diagnostic stderr")
				if scenario == "backup path" {
					assertServiceLogText(t, filepath.Join(path+".1", "unrelated"), "untouched")
				} else {
					assertServiceLogText(t, path+".1", "newer")
					assertServiceLogText(t, path+".2", "older")
				}
				entries, err := os.ReadDir(filepath.Dir(path))
				wantEntries := rotation.MaxBackups + 1
				if err != nil || len(entries) != wantEntries {
					t.Fatalf("files after failed retention = %v, %v; want %d files", entries, err, wantEntries)
				}
			}
			if _, err := stream.WriteString(" still writable"); err != nil {
				t.Fatal(err)
			}
			assertServiceLogText(t, path, "diagnostic stderr still writable")
			if scenario != "backup path" {
				if err := log.retain(rotation); err != nil {
					t.Fatal(err)
				}
				assertServiceLogText(t, path, "")
				assertServiceLogText(t, path+".1", "able")
				assertServiceLogText(t, path+".2", "newer")
				if _, err := stream.WriteString("continued"); err != nil {
					t.Fatal(err)
				}
				assertServiceLogText(t, path, "continued")
			}
		})
	}
}

func openServiceLogFixture(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stream, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func assertServiceLogText(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != want {
		t.Fatalf("log %s = %q, %v; want %q", filepath.Base(path), data, err, want)
	}
}

func TestScheduledTaskBootLogs(t *testing.T) {
	t.Parallel()
	for _, manager := range []service.ManagerName{service.ManagerScheduledTask, service.ManagerManual} {
		t.Run(string(manager), func(t *testing.T) {
			t.Parallel()
			configPath := filepath.Join(t.TempDir(), "runner config", "global.yaml")
			if err := globalconfig.Write(configPath, globalconfig.Config{APIVersion: globalconfig.APIVersion, Kind: globalconfig.Kind, Global: globalconfig.Settings{MaxConcurrentAgents: 2, Scheduling: globalconfig.SchedulingWeighted}, Projects: []globalconfig.Project{}}); err != nil {
				t.Fatal(err)
			}
			outPath, errPath := service.LogPaths(service.Config{ConfigPath: configPath})
			for _, path := range []string{outPath, errPath} {
				stream := openServiceLogFixture(t, path)
				if _, err := stream.WriteString("prior\n"); err != nil {
					t.Fatal(err)
				}
				if err := stream.Close(); err != nil {
					t.Fatal(err)
				}
			}
			var consoleOut, consoleErr strings.Builder
			cmd := NewRootCommand(t.Context(), func(opts *options) {
				opts.lookupEnv = func(key string) string {
					if key == service.ManagerEnvironment {
						return string(manager)
					}
					return ""
				}
			}, WithLoggerFunc(func(_ RuntimeSettings, stdout, stderr io.Writer, _ bool) *slog.LevelVar {
				if _, err := io.WriteString(stdout, "stdout\n"); err != nil {
					t.Fatal(err)
				}
				if _, err := io.WriteString(stderr, "stderr\n"); err != nil {
					t.Fatal(err)
				}
				return nil
			}), WithBootFunc(func(_ context.Context, cfg BootConfig) error {
				if _, err := io.WriteString(cfg.Output, "boot\n"); err != nil {
					t.Fatal(err)
				}
				if manager == service.ManagerScheduledTask {
					if cfg.Output != cfg.serviceStdout || cfg.serviceStderr == nil {
						t.Fatal("boot did not receive service streams")
					}
					stop := startServiceLogRetention(t.Context(), string(manager), configPath, logRotation{MaxSizeBytes: 7, MaxBackups: 1}, cfg.serviceStdout, cfg.serviceStderr)
					stop()
					assertServiceLogText(t, outPath, "")
					assertServiceLogText(t, outPath+".1", "t\nboot\n")
					assertServiceLogText(t, errPath, "")
					assertServiceLogText(t, errPath+".1", "stderr\n")
					if _, err := io.WriteString(cfg.Output, "after rotation\n"); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			}))
			cmd.SetOut(&consoleOut)
			cmd.SetErr(&consoleErr)
			cmd.SetArgs([]string{"--config", configPath, "--headless"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if manager == service.ManagerScheduledTask {
				assertServiceLogText(t, outPath, "after rotation\n")
				if consoleOut.Len() != 0 || consoleErr.Len() != 0 {
					t.Fatalf("service output escaped to console: %q, %q", consoleOut.String(), consoleErr.String())
				}
			} else {
				assertServiceLogText(t, outPath, "prior\n")
				assertServiceLogText(t, errPath, "prior\n")
				if consoleOut.String() != "stdout\nboot\n" || consoleErr.String() != "stderr\n" {
					t.Fatalf("manual output = %q, %q", consoleOut.String(), consoleErr.String())
				}
			}
		})
	}
}
