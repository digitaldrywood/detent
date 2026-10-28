package toolcache

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestCacheProgramInterruptedPublication(t *testing.T) {
	for _, kind := range []string{"d", "a"} {
		t.Run(kind, func(t *testing.T) {
			helper := "TestCacheProgramInterruptedPublicationHelper"
			if kind == "a" {
				helper = "TestCacheProgramInterruptedIndexHelper"
			}
			root, scratch := t.TempDir(), t.TempDir()
			cache, err := NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), executable, "-test.run=^"+helper+"$")
			procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: map[string]string{
				"DETENT_CACHE_INTERRUPTED_ROOT": root,
				"GOCOVERDIR":                    t.TempDir(),
			}})
			procgroup.SetTempDir(cmd, scratch)
			procgroup.Configure(t.Context(), cmd)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if err := procgroup.Cleanup(procgroup.GroupID(cmd)); err != nil {
					t.Error(err)
				}
				if !waited {
					_ = cmd.Wait()
				}
			})
			ready, err := bufio.NewReader(output).ReadString('\n')
			if err != nil || ready != "staged\n" {
				t.Fatalf("publication did not reach interruption point: %q %v\n%s", ready, err, &stderr)
			}
			staged, err := filepath.Glob(filepath.Join(root, "*", "*-"+kind+".*"))
			if err != nil || len(staged) != 1 {
				t.Fatalf("interrupted publication has no staging file: %v %v", staged, err)
			}
			info, err := os.Stat(staged[0])
			if err != nil || info.Size() == 0 {
				t.Fatalf("interrupted staging contains no data: %v", err)
			}
			if _, err := input.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			waited = true
			if err != nil {
				t.Fatalf("interrupted helper failed: %v\n%s", err, &stderr)
			}
			report, err := cache.Maintain(t.Context(), Policy{MaxBytes: 1}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(staged[0]); !os.IsNotExist(err) || report.SizeEvictedBytes != info.Size() {
				t.Fatalf("existing maintenance left interrupted publication behind: report=%#v stat=%v", report, err)
			}
		})
	}
}

func TestCacheProgramInterruptedPublicationHelper(t *testing.T) {
	root := os.Getenv("DETENT_CACHE_INTERRUPTED_ROOT")
	if root == "" {
		return
	}
	program, err := newCacheProgram(root, os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer program.close()
	program.link = func(_, _ string) error {
		interruptCachePublication(t)
		return nil
	}
	putInterruptedOutput(t, program)
}

func interruptCachePublication(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(os.Stdout, "staged"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func putInterruptedOutput(t *testing.T, program *cacheProgram) {
	t.Helper()
	body := []byte("interrupted cache output")
	action, output := sha256.Sum256([]byte("action")), sha256.Sum256(body)
	encoded := `"` + base64.StdEncoding.EncodeToString(body) + `"`
	if _, err := program.put(cacheRequest{ActionID: action[:], OutputID: output[:], BodySize: int64(len(body))}, bufio.NewReader(strings.NewReader(encoded))); err != nil {
		t.Fatal(err)
	}
	t.Fatal("publication was not interrupted")
}
