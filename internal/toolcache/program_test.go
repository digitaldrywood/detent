package toolcache

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == ProgramArgument {
		if err := RunProgram(os.Getenv("GOCACHE"), os.TempDir(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func TestCacheProgramPinsUntilClose(t *testing.T) {
	for _, body := range []string{"", "cached output", strings.Repeat("a", 64<<10)} {
		t.Run(fmt.Sprintf("bytes=%d", len(body)), func(t *testing.T) {
			root, scratch := t.TempDir(), t.TempDir()
			cache, err := NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			writer, err := newCacheProgram(root, scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.close()
			action, output := sha256.Sum256([]byte("action")), sha256.Sum256([]byte(body))
			encoded := `"` + base64.StdEncoding.EncodeToString([]byte(body)) + `"` + "\n"
			put, err := writer.put(cacheRequest{ActionID: action[:], OutputID: output[:], BodySize: int64(len(body))}, bufio.NewReader(strings.NewReader(encoded)))
			if err != nil {
				t.Fatal(err)
			}
			reader, err := newCacheProgram(root, scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.close()
			old := time.Now().Add(-72 * time.Hour)
			for _, name := range []string{cacheName(action[:], "a"), cacheName(output[:], "d")} {
				if err := os.Chtimes(filepath.Join(root, name), old, old); err != nil {
					t.Fatal(err)
				}
			}
			get := reader.get(action[:])
			if get.Miss || !bytes.Equal(get.OutputID, output[:]) || get.Size != int64(len(body)) {
				t.Fatalf("put output could not be read by another build: %#v", get)
			}
			for _, name := range []string{cacheName(action[:], "a"), cacheName(output[:], "d")} {
				if info, err := os.Stat(filepath.Join(root, name)); err != nil || !info.ModTime().After(old) {
					t.Fatalf("build access did not refresh cache access time: %v", err)
				}
			}
			if err := writer.close(); err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(-72 * time.Hour)
			if err := os.Chtimes(filepath.Join(root, cacheName(output[:], "d")), at, at); err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Maintain(t.Context(), Policy{MaxBytes: 1}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(get.DiskPath); err != nil || string(data) != body {
				t.Fatalf("active reader lost its output after writer exited and maintenance ran: bytes=%d err=%v", len(data), err)
			}
			if _, err := os.Stat(filepath.Join(root, cacheName(output[:], "d"))); err != nil {
				t.Fatalf("active output was age-evicted: %v", err)
			}
			if _, err := os.Stat(put.DiskPath); !os.IsNotExist(err) {
				t.Fatalf("writer retained scratch pins: %v", err)
			}
			if err := reader.close(); err != nil {
				t.Fatal(err)
			}
			if _, err := cache.Maintain(t.Context(), Policy{MaxBytes: 1}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, cacheName(output[:], "d"))); !os.IsNotExist(err) {
				t.Fatalf("released output retained: %v", err)
			}
		})
	}
}

func TestCacheProgramProtocol(t *testing.T) {
	for _, closeCommand := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%t", closeCommand), func(t *testing.T) {
			root, scratch := t.TempDir(), t.TempDir()
			body := []byte("streamed output")
			action, output := sha256.Sum256([]byte("action")), sha256.Sum256(body)
			var input, result bytes.Buffer
			encoder := json.NewEncoder(&input)
			for _, request := range []cacheRequest{
				{ID: 1, Command: "get", ActionID: action[:]},
				{ID: 2, Command: "put", ActionID: action[:], OutputID: output[:], BodySize: int64(len(body))},
				{ID: 3, Command: "get", ActionID: action[:]},
			} {
				if err := encoder.Encode(request); err != nil {
					t.Fatal(err)
				}
				if request.Command == "put" {
					if err := encoder.Encode(body); err != nil {
						t.Fatal(err)
					}
				}
			}
			if closeCommand {
				if err := encoder.Encode(cacheRequest{ID: 4, Command: "close"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := RunProgram(root, scratch, &input, &result); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&result)
			for id := range 4 {
				var response cacheResponse
				if err := decoder.Decode(&response); err != nil {
					t.Fatal(err)
				}
				if response.ID != int64(id) || id == 0 && strings.Join(response.KnownCommands, ",") != "get,put,close" || id == 1 && !response.Miss || id == 3 && (response.Miss || !bytes.Equal(response.OutputID, output[:])) {
					t.Fatalf("response %d: %#v", id, response)
				}
			}
			if files, err := os.ReadDir(scratch); err != nil || len(files) != 0 {
				t.Fatalf("cache program left pins after close or EOF: count=%d err=%v", len(files), err)
			}
		})
	}
}

func TestCacheProgramGoBuild(t *testing.T) {
	root, module, scratch := t.TempDir(), t.TempDir(), t.TempDir()
	cache, err := NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"go.mod": "module example.test/cache\ngo 1.26\n", "value.go": "package fixture\nconst Value = 42\n"} {
		if err := os.WriteFile(filepath.Join(module, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for build := range 2 {
		cmd := exec.CommandContext(t.Context(), "go", "build", "-x", "-o", filepath.Join(module, "output.a"), ".")
		cmd.Dir = module
		variables := ManagedEnvironment(cache.WithContext(t.Context()))
		variables["GOCOVERDIR"] = t.TempDir()
		variables["GOENV"], variables["GOFLAGS"], variables["GOWORK"], variables["GOTOOLCHAIN"] = "off", "", "off", "local"
		procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: variables})
		procgroup.SetTempDir(cmd, scratch)
		procgroup.Configure(t.Context(), cmd)
		t.Cleanup(func() {
			if err := procgroup.Cleanup(procgroup.GroupID(cmd)); err != nil {
				t.Error(err)
			}
		})
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("Go build using managed cache failed: %v\n%s", err, output)
		}
		if compiled := bytes.Contains(output, []byte(" -pack ")); compiled != (build == 0) {
			t.Fatalf("build %d: compiled=%t, expected cached reuse after first build\n%s", build, compiled, output)
		}
		if data, err := os.ReadFile(filepath.Join(module, "output.a")); err != nil || !bytes.HasPrefix(data, []byte("!<arch>\n")) {
			t.Fatalf("Go build did not produce a package archive: bytes=%d err=%v", len(data), err)
		}
	}
}

func TestCacheProgramPinSurvivesLinkFailureAndEviction(t *testing.T) {
	for _, evict := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_eviction=%t", evict), func(t *testing.T) {
			root, scratch := t.TempDir(), t.TempDir()
			cache, err := NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			writer, err := newCacheProgram(root, scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.close()
			body := "active output"
			action, output := sha256.Sum256([]byte("action")), sha256.Sum256([]byte(body))
			request := cacheRequest{ActionID: action[:], OutputID: output[:], BodySize: int64(len(body))}
			encoded := `"` + base64.StdEncoding.EncodeToString([]byte(body)) + `"` + "\n"
			if _, err := writer.put(request, bufio.NewReader(strings.NewReader(encoded))); err != nil {
				t.Fatal(err)
			}
			if err := writer.close(); err != nil {
				t.Fatal(err)
			}
			reader, err := newCacheProgram(root, scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.close()
			reader.link = func(_, _ string) error {
				if evict {
					if _, err := cache.Maintain(t.Context(), Policy{MaxBytes: 1}, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("hard links unavailable")
			}
			get := reader.get(action[:])
			if get.Miss {
				t.Fatal("link failure lost an output already open by the active build")
			}
			if _, err := cache.Maintain(t.Context(), Policy{MaxBytes: 1}, time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, cacheName(output[:], "d"))); !os.IsNotExist(err) {
				t.Fatalf("unlinked cache output retained: %v", err)
			}
			if data, err := os.ReadFile(get.DiskPath); err != nil || string(data) != body {
				t.Fatalf("active output lost after cache eviction: %q %v", data, err)
			}
		})
	}
}
