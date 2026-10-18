package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/service"
)

func TestServiceLogRetention(t *testing.T) {
	t.Parallel()
	for _, manager := range []service.ManagerName{service.ManagerLaunchd, service.ManagerSystemd} {
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
	path := filepath.Join(t.TempDir(), "service.err.log")
	stream := openServiceLogFixture(t, path)
	defer stream.Close()
	if _, err := stream.WriteString("diagnostic stderr"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".1", "unrelated"), []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	log := serviceLog{path: path, stream: stream}
	if err := log.retain(logRotation{MaxSizeBytes: 4, MaxBackups: 1}); err == nil {
		t.Fatal("retention succeeded with a nonempty directory at the snapshot path")
	}
	if _, err := stream.WriteString(" still writable"); err != nil {
		t.Fatal(err)
	}
	assertServiceLogText(t, path, "diagnostic stderr still writable")
	assertServiceLogText(t, filepath.Join(path+".1", "unrelated"), "untouched")
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
