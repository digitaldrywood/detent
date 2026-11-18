package toolcache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveFallback(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		fail, override, gopath bool
	}{
		{name: "go missing"},
		{name: "environment overrides", override: true}, {name: "GOPATH fallback", gopath: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			testResolveFallback(t, tt.fail, tt.override, tt.gopath)
		})
	}
}

func testResolveFallback(t *testing.T, fail, override, gopath bool) {
	t.Helper()

	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("HOME", root)
	t.Setenv("USERPROFILE", root)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	t.Setenv("LocalAppData", filepath.Join(root, "cache"))
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOPATH", "")
	if fail {
		if err := os.WriteFile(filepath.Join(root, "go"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{Build: filepath.Join(cache, "go-build"), Modules: filepath.Join(root, "go", "pkg", "mod")}
	if gopath {
		t.Setenv("GOPATH", filepath.Join(root, "custom")+string(os.PathListSeparator)+filepath.Join(root, "second"))
		want.Modules = filepath.Join(root, "custom", "pkg", "mod")
	}
	if override {
		want = Paths{Build: filepath.Join(root, "build"), Modules: filepath.Join(root, "modules")}
		t.Setenv("GOCACHE", want.Build)
		t.Setenv("GOMODCACHE", want.Modules)
	}
	got, err := resolve(context.Background())
	if err != nil || got != want {
		t.Fatalf("resolve=%+v, %v; want %+v", got, err, want)
	}
}

func TestResolveOnce(t *testing.T) {
	if os.Getenv("DETENT_TEST_CACHE_ONCE") == "1" {
		first, err := Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOCACHE", filepath.Join(t.TempDir(), "changed"))
		for range 3 {
			got, err := Resolve(context.Background())
			if err != nil || got != first {
				t.Fatalf("discovery repeated: %+v, %v", got, err)
			}
		}
		return
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestResolveOnce$")
	cmd.Env = append(os.Environ(), "DETENT_TEST_CACHE_ONCE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func TestResolveHostSettings(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		persisted, environment bool
	}{
		{name: "default"},
		{name: "GOCACHE environment", environment: true},
		{name: "go env -w GOCACHE", persisted: true},
		{name: "environment overrides persisted", persisted: true, environment: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			telemetry := t.TempDir()
			t.Setenv("TEST_TELEMETRY_DIR", telemetry)
			if err := os.WriteFile(filepath.Join(telemetry, "mode"), []byte("off\n"), 0600); err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
			t.Setenv("LocalAppData", filepath.Join(home, "cache"))
			t.Setenv("GOENV", filepath.Join(home, "goenv"))
			t.Setenv("GOCACHE", "")
			t.Setenv("GOMODCACHE", "")
			t.Setenv("GOPATH", filepath.Join(home, "go"))
			t.Setenv("GOTOOLCHAIN", "local")
			t.Setenv("GOWORK", "off")
			t.Setenv("GOFLAGS", "")
			if tt.persisted {
				cmd := exec.CommandContext(t.Context(), "go", "env", "-w", "GOCACHE="+filepath.Join(home, "persisted-build"))
				cmd.Dir = home
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("write isolated Go settings: %v\n%s", err, output)
				}
			}
			if tt.environment {
				t.Setenv("GOCACHE", filepath.Join(home, "environment-build"))
			}
			expected := exec.CommandContext(t.Context(), "go", "env", "GOCACHE")
			expected.Dir = home
			output, err := expected.Output()
			if err != nil {
				t.Fatal(err)
			}
			got, err := resolve(t.Context())
			if err != nil || got.Build != strings.TrimSpace(string(output)) {
				t.Fatalf("resolved %+v, %v; want Go cache %q", got, err, output)
			}
		})
	}
}
