package toolcache

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCacheProgramInterruptedIndexHelper(t *testing.T) {
	root := os.Getenv("DETENT_CACHE_INTERRUPTED_ROOT")
	if root == "" {
		return
	}
	program, err := newCacheProgram(root, os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer program.close()
	program.rename = func(old, new string) error {
		if strings.HasSuffix(new, "-a") {
			interruptCachePublication(t)
		}
		return program.cache.Rename(old, new)
	}
	putInterruptedOutput(t, program)
}

func TestManagedCacheReclaimsAbandonedStages(t *testing.T) {
	for _, tt := range []struct {
		name    string
		managed bool
		aged    bool
	}{
		{"managed recent retained", true, false},
		{"managed age", true, true},
		{"native size", false, false},
		{"native age", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache, err := NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "README"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "00"), 0o700); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			at := now
			policy := Policy{MaxBytes: 1}
			if tt.aged {
				at = now.Add(-72 * time.Hour)
				policy.MaxBytes = 1 << 20
			}
			prefix := strings.Repeat("0", 64)
			names := []string{prefix + "-d." + rand.Text(), prefix + "-a." + rand.Text(), prefix + "-d.backup", prefix + "-a." + strings.Repeat("A", 25)}
			for _, name := range names {
				path := filepath.Join(root, "00", name)
				if err := os.WriteFile(path, make([]byte, 2048), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, at, at); err != nil {
					t.Fatal(err)
				}
			}
			var report Report
			if tt.managed {
				report, err = cache.Maintain(t.Context(), policy, now)
			} else {
				report, err = TrimWithReport(t.Context(), root, policy, now)
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, name := range names {
				_, err := os.Stat(filepath.Join(root, "00", name))
				removed := tt.managed && tt.aged && i < 2
				if os.IsNotExist(err) != removed || err != nil && !os.IsNotExist(err) {
					t.Errorf("stage %q: removed=%t stat=%v", name, removed, err)
				}
			}
			var aged int64
			if tt.managed && tt.aged {
				aged = 4096
			}
			if report.AgeExpiredBytes != aged || report.SizeEvictedBytes != 0 {
				t.Fatalf("abandoned stage accounting: %#v", report)
			}
		})
	}
}

type publicationReader struct {
	io.Reader
	beforeRead func()
}

func (r *publicationReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		f := r.beforeRead
		r.beforeRead = nil
		f()
	}
	return r.Reader.Read(p)
}

func TestCacheProgramPutSurvivesMaintenance(t *testing.T) {
	for _, crossDevice := range []bool{false, true} {
		for _, point := range []string{"body", "pin", "data", "index"} {
			t.Run(fmt.Sprintf("cross_device=%t/%s", crossDevice, point), func(t *testing.T) {
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
				body := strings.Repeat("cache output", 8192)
				action, output := sha256.Sum256([]byte("action")), sha256.Sum256([]byte(body))
				maintained := false
				maintain := func() {
					t.Helper()
					maintained = true
					if point == "body" {
						stages, err := filepath.Glob(filepath.Join(root, "*", "*-d.*"))
						if err != nil || len(stages) != 1 {
							t.Fatalf("active write has no stage: %v %v", stages, err)
						}
						if info, err := os.Stat(stages[0]); err != nil || info.Size() == 0 || info.Size() >= int64(len(body)) {
							t.Fatalf("maintenance did not interrupt a partial write: %v", err)
						}
					}
					if _, err := cache.Maintain(t.Context(), Policy{MaxBytes: 1}, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				writer.link = func(old, new string) error {
					if point == "pin" {
						maintain()
					}
					if crossDevice {
						return &os.LinkError{Op: "link", Old: old, New: new, Err: syscall.EXDEV}
					}
					return os.Link(old, new)
				}
				writer.rename = func(old, new string) error {
					if point == "data" && strings.HasSuffix(new, "-d") || point == "index" && strings.HasSuffix(new, "-a") {
						maintain()
					}
					return writer.cache.Rename(old, new)
				}
				encoded := `"` + base64.StdEncoding.EncodeToString([]byte(body)) + `"`
				var input io.Reader = strings.NewReader(encoded)
				if point == "body" {
					input = io.MultiReader(strings.NewReader(encoded[:8193]), &publicationReader{Reader: strings.NewReader(encoded[8193:]), beforeRead: maintain})
				}
				put, err := writer.put(cacheRequest{ActionID: action[:], OutputID: output[:], BodySize: int64(len(body))}, bufio.NewReader(input))
				if err != nil {
					t.Fatalf("maintenance broke active put: %v", err)
				}
				if !maintained {
					t.Fatal("maintenance did not run during publication")
				}
				if get := writer.get(action[:]); get.Miss {
					t.Fatalf("publication unavailable after gentle maintenance: %#v", get)
				}
				if _, err := cache.Maintain(t.Context(), Policy{MaxBytes: 1}, time.Now()); err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(put.DiskPath); err != nil || string(data) != body {
					t.Fatalf("maintenance invalidated returned output: bytes=%d err=%v", len(data), err)
				}
				stages, err := filepath.Glob(filepath.Join(root, "*", "*-*.*"))
				if err != nil || len(stages) != 0 {
					t.Fatalf("put left staging behind: %v %v", stages, err)
				}
			})
		}
	}
}
