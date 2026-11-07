package pgtest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

type binaryLifetime struct {
	Data    string
	PID     int
	Segment string
}

func TestBinaryLifetime(t *testing.T) {
	if mode := os.Getenv("DETENT_PGTEST_LIFETIME"); mode != "" {
		if err := processTemplate.initialize(t.Context(), "binary"); err != nil {
			t.Fatal(err)
		}
		borrower := NewTemplate(migrateFixture)
		if err := borrower.initialize(t.Context(), "binary"); err != nil {
			t.Fatal(err)
		}
		if borrower.server.config.Host != processTemplate.server.config.Host {
			t.Fatal("lifetime fixture started a second postmaster")
		}
		if err := borrower.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		data := processTemplate.server.config.Host
		pidfile, err := os.ReadFile(filepath.Join(data, "postmaster.pid"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(pidfile), "\n")
		pid, err := strconv.Atoi(lines[0])
		if err != nil {
			t.Fatal(err)
		}
		ready := os.NewFile(3, "fixture-ready")
		if err := json.NewEncoder(ready).Encode(binaryLifetime{data, pid, strings.Fields(lines[6])[1]}); err != nil {
			t.Fatal(err)
		}
		if err := ready.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		if mode == "close" {
			if err := processTemplate.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for _, tc := range []struct {
		name   string
		signal syscall.Signal
	}{
		{name: "SIGKILL", signal: syscall.SIGKILL},
		{name: "SIGINT", signal: syscall.SIGINT},
		{name: "SIGTERM", signal: syscall.SIGTERM},
		{name: "TestMain"},
		{name: "close"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ready, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { ready.Close(); writer.Close() })
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestBinaryLifetime$", "-test.timeout="+flag.Lookup("test.timeout").Value.String())
			cmd.Env = append(os.Environ(), "DETENT_PGTEST_LIFETIME="+tc.name, "GOCOVERDIR="+t.TempDir())
			cmd.ExtraFiles = []*os.File{writer}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			control, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			var fixture binaryLifetime
			waited := false
			t.Cleanup(func() {
				control.Close()
				if !waited {
					cmd.Process.Kill()
					cmd.Wait()
				}
				if t.Failed() && fixture.Data != "" {
					cleanup := exec.Command("pg_ctl", "stop", "-D", fixture.Data, "-m", "immediate", "-w")
					if out, err := cleanup.CombinedOutput(); err != nil {
						t.Logf("failure cleanup: %v: %s", err, out)
					}
				}
			})
			writer.Close()
			if err := json.NewDecoder(ready).Decode(&fixture); err != nil {
				t.Fatalf("fixture readiness: %v", err)
			}
			base, err := filepath.EvalSymlinks(os.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			data, err := filepath.EvalSymlinks(fixture.Data)
			if err != nil {
				t.Fatal(err)
			}
			if rel, err := filepath.Rel(base, data); err != nil || strings.HasPrefix(rel, "..") {
				t.Fatalf("cluster data %s is outside test temporary directory %s", data, base)
			}
			if present, err := binarySegmentPresent(t.Context(), fixture.Segment); err != nil || !present {
				t.Fatalf("running fixture segment %s: present=%v, error=%v", fixture.Segment, present, err)
			}
			if tc.signal != 0 {
				if err := cmd.Process.Signal(tc.signal); err != nil {
					t.Fatal(err)
				}
			} else if _, err := control.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if (err != nil) != (tc.signal != 0) {
				t.Fatalf("helper exit: %v: %s", err, output.String())
			}
			testenv.WaitUntil(t, t.Context(), func() error {
				if err := syscall.Kill(fixture.PID, 0); !errors.Is(err, syscall.ESRCH) {
					return fmt.Errorf("postmaster %d still exists: %v", fixture.PID, err)
				}
				present, err := binarySegmentPresent(t.Context(), fixture.Segment)
				if err != nil {
					return err
				}
				if present {
					return fmt.Errorf("fixture shared-memory segment %s remains", fixture.Segment)
				}
				return nil
			})
		})
	}
}

func binarySegmentPresent(ctx context.Context, segment string) (bool, error) {
	output, err := exec.CommandContext(ctx, "ipcs", "-m").Output()
	if err != nil {
		return false, fmt.Errorf("read shared-memory segments: %w", err)
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		fields := strings.Fields(line)
		if runtime.GOOS == "darwin" && len(fields) >= 3 && fields[0] == "m" && fields[1] == segment {
			return true, nil
		}
		if runtime.GOOS != "darwin" && len(fields) >= 2 && strings.HasPrefix(fields[0], "0x") && fields[1] == segment {
			return true, nil
		}
	}
	return false, nil
}

func TestBinaryStartupExit(t *testing.T) {
	bin := t.TempDir()
	for name, body := range map[string]string{"initdb": "#!/bin/sh\nexit 0\n", "postgres": "#!/bin/sh\nexit 13\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	finished := false
	t.Cleanup(func() {
		cancel()
		if !finished {
			<-done
		}
	})
	go func() {
		_, stop, err := startBinary(ctx)
		if stop != nil {
			err = errors.Join(err, stop(ctx))
		}
		done <- err
	}()
	var result error
	testenv.WaitUntil(t, t.Context(), func() error {
		select {
		case result = <-done:
			finished = true
			return nil
		default:
			return errors.New("postmaster exited without reporting startup failure")
		}
	})
	if result == nil || !strings.Contains(result.Error(), "exited before readiness") {
		t.Fatalf("startup failure = %v", result)
	}
}
