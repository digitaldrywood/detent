package cli

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/scheduler"
	"github.com/digitaldrywood/detent/internal/toolcache"
)

func TestRunnerBuildCacheMaintenanceResumesDispatch(t *testing.T) {
	for _, tt := range []struct {
		name           string
		maxBytes       int64
		freeBefore     uint64
		freeAfter      uint64
		separateVolume bool
		wantEvicted    int64
		wantAdmission  bool
		recent         bool
	}{
		{name: "over budget without pressure", maxBytes: 1000, freeBefore: 5000, freeAfter: 2500, wantEvicted: 2000, wantAdmission: true},
		{name: "within budget above pressure target", maxBytes: 4000, freeBefore: 2500, wantAdmission: true},
		{name: "within budget approaching reserve", maxBytes: 4000, freeBefore: 1500, freeAfter: 3500, wantEvicted: 2000, wantAdmission: true},
		{name: "within budget below reserve", maxBytes: 4000, freeAfter: 2500, wantEvicted: 2000, wantAdmission: true},
		{name: "insufficient reclaimable cache", maxBytes: 4000, freeAfter: 700, wantEvicted: 2000},
		{name: "separate workspace filesystem", maxBytes: 4000, separateVolume: true},
		{name: "recent above budget", maxBytes: 1000, recent: true},
		{name: "recent under pressure", maxBytes: 4000, recent: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, release, err := cache.Use(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			other, err := toolcache.NewManaged(t.Context(), root, filepath.Join(root, "other.db"))
			if err != nil {
				t.Fatal(err)
			}
			_, secondRelease, err := other.Use(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer secondRelease()
			if err := os.WriteFile(filepath.Join(root, "README"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "00"), 0o700); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "00", strings.Repeat("a", 64)+"-d")
			if err := os.WriteFile(output, make([]byte, 2000), 0o600); err != nil {
				t.Fatal(err)
			}
			if !tt.recent {
				old := time.Now().Add(-26 * time.Hour)
				if err := os.Chtimes(output, old, old); err != nil {
					t.Fatal(err)
				}
			}
			pin := filepath.Join(t.TempDir(), "active-output")
			if err := os.Link(output, pin); err != nil {
				t.Fatal(err)
			}
			warm := filepath.Join(root, "00", strings.Repeat("d", 64)+"-d")
			if err := os.WriteFile(warm, []byte("warm output"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(warm, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			warmPin := filepath.Join(t.TempDir(), "active-warm-output")
			if err := os.Link(warm, warmPin); err != nil {
				t.Fatal(err)
			}
			candidate := scheduler.ProjectCandidate{ID: "example-project"}
			gate, err := scheduler.NewPoolRegistry([]scheduler.PoolConfig{{Name: scheduler.DefaultPoolName, Scheduler: scheduler.Config{Kind: "round_robin", Capacity: 2}}}, []scheduler.ProjectCandidate{candidate})
			if err != nil {
				t.Fatal(err)
			}
			first, acquired, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !acquired {
				t.Fatalf("first dispatch: %t %v", acquired, err)
			}
			defer gate.Release(first)
			managed := &runnerBuildCache{cache: cache, root: root}
			workspace := root
			if tt.separateVolume {
				workspace = filepath.Join(t.TempDir(), "workspace")
			}
			volume := func(path string) (uint64, uint64, error) {
				if path != root && path != workspace {
					t.Fatalf("observed unrelated filesystem: %s", path)
				}
				if tt.separateVolume {
					if path == root {
						return 2500, 10000, nil
					}
					return 500, 10000, nil
				}
				if _, err := os.Stat(output); err == nil {
					return max(500, tt.freeBefore), 10000, nil
				}
				return tt.freeAfter, 10000, nil
			}
			storageConstraint := func() string {
				return runnerStorageConstraintForPaths(globalconfig.Config{WorkspaceRoot: workspace}, toolcache.Paths{Build: root}, volume)
			}
			if got := storageConstraint(); strings.Contains(got, "reserve") != (tt.freeBefore < 1000) {
				t.Fatalf("low free disk did not constrain capacity: %q", got)
			}
			for _, active := range []int{2, 1, 0} {
				err := managed.trimForVolume(t.Context(), toolcache.Policy{MaxBytes: tt.maxBytes}, time.Now(), volume)
				if active > 0 {
					if err != nil {
						t.Fatalf("maintenance with %d active consumers did not yield: %v", active, err)
					}
					if report := managed.observe(); report == nil || report.Deferred || report.SizeEvictedBytes != 0 {
						t.Fatalf("busy maintenance did not retain pinned output: %#v", report)
					}
					if data, err := os.ReadFile(output); err != nil || len(data) != 2000 {
						t.Fatalf("active cache changed: bytes=%d err=%v", len(data), err)
					}
					if got := storageConstraint(); strings.Contains(got, "reserve") != (tt.freeBefore < 1000) {
						t.Fatalf("active cache pressure lost its reserve constraint: %q", got)
					}
					if active == 2 {
						if err := release(); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := secondRelease(); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(pin); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(warmPin); err != nil {
							t.Fatal(err)
						}
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				if report := managed.observe(); report == nil || report.Deferred || report.SizeEvictedBytes != tt.wantEvicted || !tt.recent && report.RetainedBytes > tt.maxBytes {
					t.Fatalf("maintenance result: %+v, want evicted=%d", report, tt.wantEvicted)
				}
				if _, err := os.Stat(output); os.IsNotExist(err) != (tt.wantEvicted != 0) {
					t.Fatalf("cache reclamation: stat=%v, want evicted=%d", err, tt.wantEvicted)
				}
			}
			if got := storageConstraint(); (got == "") != tt.wantAdmission {
				t.Fatalf("admission after reclamation: %q, want available=%v", got, tt.wantAdmission)
			}
			if !tt.wantAdmission {
				return
			}
			_, nextRelease, err := cache.Use(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer nextRelease()
			if data, err := os.ReadFile(warm); err != nil || string(data) != "warm output" {
				t.Fatalf("retained output not reusable: %q %v", data, err)
			}
			slot, acquired, err := gate.TryAcquire(t.Context(), candidate, scheduler.SlotRequest{State: "Todo"}, time.Now())
			if err != nil || !acquired {
				t.Fatalf("second dispatch: %t %v", acquired, err)
			}
			defer gate.Release(slot)
		})
	}
}

func TestRunnerStorageAdmissionReclaimsBeforeRefusing(t *testing.T) {
	for _, tt := range []struct {
		name                                                                      string
		pinned, recent, separateVolume, native, observationError, maintenanceBusy bool
		wantAvailable                                                             bool
	}{
		{name: "cache frees reserve", wantAvailable: true},
		{name: "pinned output retains reserve refusal", pinned: true},
		{name: "recent output retains reserve refusal", recent: true},
		{name: "workspace volume still below reserve", separateVolume: true},
		{name: "operator cache is not reclaimed", native: true},
		{name: "unavailable observation fails closed", observationError: true},
		{name: "concurrent maintenance retains reserve refusal", maintenanceBusy: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			cache, err := toolcache.NewManaged(t.Context(), root, filepath.Join(root, "runtime.db"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "00"), 0700); err != nil {
				t.Fatal(err)
			}
			output := filepath.Join(root, "00", strings.Repeat("a", 64)+"-d")
			if err := os.WriteFile(output, make([]byte, 2000), 0600); err != nil {
				t.Fatal(err)
			}
			if !tt.recent {
				old := time.Now().Add(-4 * time.Hour)
				if err := os.Chtimes(output, old, old); err != nil {
					t.Fatal(err)
				}
			}
			if tt.pinned {
				if err := os.Link(output, filepath.Join(t.TempDir(), "pin")); err != nil {
					t.Fatal(err)
				}
			}
			managed := &runnerBuildCache{cache: cache, root: root}
			if tt.maintenanceBusy {
				managed.mu.Lock()
				t.Cleanup(managed.mu.Unlock)
			}
			if tt.native {
				managed = nil
			}
			workspace := root
			if tt.separateVolume {
				workspace = t.TempDir()
			}
			cfg := globalconfig.Config{WorkspaceRoot: workspace}
			cfg.Global.Cache = toolcache.Policy{MaxBytes: 4000}
			volume := func(path string) (uint64, uint64, error) {
				if tt.observationError {
					return 0, 0, errors.New("observation failed")
				}
				if path != root {
					return 500, 10000, nil
				}
				if _, err := os.Stat(output); err == nil {
					return 500, 10000, nil
				}
				return 2500, 10000, nil
			}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			got := runnerStorageConstraintAfterReclaim(t.Context(), cfg, toolcache.Paths{Build: root}, managed, volume)
			if (got == "") != tt.wantAvailable {
				t.Fatalf("constraint=%q, want available=%t", got, tt.wantAvailable)
			}
			if tt.wantAvailable {
				if _, err := os.Stat(output); !os.IsNotExist(err) {
					t.Fatalf("cache not reclaimed before admission: %v", err)
				}
				if !strings.Contains(logs.String(), "size_evicted_bytes=2000") || !strings.Contains(logs.String(), "pressure_reclaim_bytes=1500") {
					t.Fatalf("missing reclaim log: %s", logs.String())
				}
			}
			if tt.pinned || tt.recent || tt.native || tt.observationError || tt.maintenanceBusy {
				if data, err := os.ReadFile(output); err != nil || len(data) != 2000 {
					t.Fatalf("protected cache changed: bytes=%d err=%v", len(data), err)
				}
			}
		})
	}
}

func TestRunnerBuildCacheSelection(t *testing.T) {
	for _, tt := range []struct {
		name                                                string
		environment, program, persisted, existing, disabled bool
	}{
		{name: "Go default"},
		{name: "operator environment", environment: true},
		{name: "operator GOENV", persisted: true},
		{name: "environment overrides GOENV", persisted: true, environment: true},
		{name: "operator program", program: true},
		{name: "leave previous managed directory", existing: true},
		{name: "cache off", disabled: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			telemetry := t.TempDir()
			t.Setenv("TEST_TELEMETRY_DIR", telemetry)
			if err := os.WriteFile(filepath.Join(telemetry, "mode"), []byte("off\n"), 0600); err != nil {
				t.Fatal(err)
			}
			base := t.TempDir()
			t.Setenv("HOME", base)
			t.Setenv("USERPROFILE", base)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "cache"))
			t.Setenv("LocalAppData", filepath.Join(base, "cache"))
			t.Setenv("GOENV", filepath.Join(base, "goenv"))
			t.Setenv("GOCACHE", "")
			t.Setenv("GOCACHEPROG", "")
			t.Setenv("GOTOOLCHAIN", "local")
			goEnv := func(args ...string) string {
				t.Helper()
				cmd := exec.CommandContext(t.Context(), "go", append([]string{"env"}, args...)...)
				cmd.Dir = base
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("go env: %v\n%s", err, output)
				}
				return strings.TrimSpace(string(output))
			}
			if tt.persisted {
				goEnv("-w", "GOCACHE="+filepath.Join(base, "persisted-cache"))
			}
			if tt.environment {
				t.Setenv("GOCACHE", filepath.Join(base, "environment-cache"))
			}
			if tt.program {
				t.Setenv("GOCACHEPROG", "operator-program")
			}
			if tt.disabled {
				t.Setenv("GOCACHE", "off")
			}
			root := goEnv("GOCACHE")
			userCache, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			legacy := filepath.Join(userCache, "detent", "go-build")
			if tt.existing {
				if err := os.MkdirAll(legacy, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(legacy, "operator-data"), []byte("keep"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			owner := filepath.Join(base, "runtime.db")
			if err := os.WriteFile(owner, nil, 0600); err != nil {
				t.Fatal(err)
			}
			cfg := BootConfig{RuntimeDBPath: owner, Global: globalconfig.Config{Client: globalconfig.HubClient{URL: "https://hub.example.test", IdentityFile: filepath.Join(base, "identity.json")}}}
			cache, err := resolveRunnerBuildCache(t.Context(), cfg, func(context.Context) (toolcache.Paths, error) {
				return toolcache.Paths{Build: root}, nil
			})
			if tt.program || tt.disabled {
				if cache != nil || !errors.Is(err, errNativeBuildCache) {
					t.Fatalf("operator cache bypass: cache=%+v err=%v", cache, err)
				}
				return
			}
			if err != nil || cache == nil || cache.root != root {
				t.Fatalf("cache=%+v err=%v, want path %s", cache, err, root)
			}
			environment := toolcache.ManagedEnvironment(cache.cache.WithContext(t.Context()))
			if environment["GOCACHE"] != root || !strings.Contains(environment["GOCACHEPROG"], toolcache.ProgramArgument) {
				t.Fatalf("worker cache environment = %v", environment)
			}
			for _, store := range []string{"runtime.db", "second.db", "replacement.db"} {
				t.Run(store, func(t *testing.T) {
					if store == "replacement.db" {
						if err := os.Remove(owner); err != nil {
							t.Fatal(err)
						}
					}
					cfg.RuntimeDBPath = filepath.Join(base, store)
					if err := os.WriteFile(cfg.RuntimeDBPath, nil, 0600); err != nil {
						t.Fatal(err)
					}
					participant, err := resolveRunnerBuildCache(t.Context(), cfg, func(context.Context) (toolcache.Paths, error) {
						return toolcache.Paths{Build: root}, nil
					})
					if err != nil || participant == nil || participant.root != cache.root {
						t.Fatalf("runtime store could not share cache: cache=%+v err=%v", participant, err)
					}
					environment := toolcache.ManagedEnvironment(participant.cache.WithContext(t.Context()))
					if environment["GOCACHE"] != root || !strings.Contains(environment["GOCACHEPROG"], toolcache.ProgramArgument) {
						t.Fatalf("participant cache program missing: %v", environment)
					}
					if err := os.MkdirAll(filepath.Join(root, "00"), 0700); err != nil {
						t.Fatal(err)
					}
					entry := filepath.Join(root, "00", strings.Repeat("a", 64)+"-d")
					if err := os.WriteFile(entry, make([]byte, 2048), 0600); err != nil {
						t.Fatal(err)
					}
					now := time.Now()
					old := now.Add(-26 * time.Hour)
					if err := os.Chtimes(entry, old, old); err != nil {
						t.Fatal(err)
					}
					if err := participant.trimForVolume(t.Context(), toolcache.Policy{MaxBytes: 1024}, now, func(string) (uint64, uint64, error) { return 5000, 10000, nil }); err != nil {
						t.Fatal(err)
					}
					if report := participant.observe(); report == nil || report.SizeEvictedBytes != 2048 || report.RetainedBytes > 1024 {
						t.Fatalf("participant maintenance = %+v", report)
					}
				})
			}
			if tt.existing {
				if data, err := os.ReadFile(filepath.Join(legacy, "operator-data")); err != nil || string(data) != "keep" {
					t.Fatalf("previous cache changed: %q %v", data, err)
				}
			} else if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("previous managed cache created: %v", err)
			}
		})
	}
}
