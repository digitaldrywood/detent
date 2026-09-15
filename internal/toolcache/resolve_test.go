package toolcache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveFallback(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		fail, override, gopath bool
	}{
		{name: "go missing"}, {name: "go env fails", fail: true},
		{name: "environment overrides", override: true}, {name: "GOPATH fallback", gopath: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("PATH", root)
			t.Setenv("HOME", root)
			t.Setenv("USERPROFILE", root)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
			t.Setenv("LocalAppData", filepath.Join(root, "cache"))
			t.Setenv("GOCACHE", "")
			t.Setenv("GOMODCACHE", "")
			t.Setenv("GOPATH", "")
			if tt.fail {
				if runtime.GOOS == "windows" {
					t.Skip("POSIX command fixture")
				}
				if err := os.WriteFile(filepath.Join(root, "go"), []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			cache, err := os.UserCacheDir()
			if err != nil {
				t.Fatal(err)
			}
			want := Paths{Build: filepath.Join(cache, "go-build"), Modules: filepath.Join(root, "go", "pkg", "mod")}
			if tt.gopath {
				t.Setenv("GOPATH", filepath.Join(root, "custom")+string(os.PathListSeparator)+filepath.Join(root, "second"))
				want.Modules = filepath.Join(root, "custom", "pkg", "mod")
			}
			if tt.override {
				want = Paths{Build: filepath.Join(root, "build"), Modules: filepath.Join(root, "modules")}
				t.Setenv("GOCACHE", want.Build)
				t.Setenv("GOMODCACHE", want.Modules)
			}
			got, err := resolve(context.Background())
			if err != nil || got != want {
				t.Fatalf("resolve=%+v, %v; want %+v", got, err, want)
			}
		})
	}
}

func TestResolveNeutralToolchain(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command fixture")
	}
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("GOCACHE", "")
	t.Setenv("GOMODCACHE", "")
	t.Setenv("GOTOOLCHAIN", "go1.99")
	t.Setenv("TMPDIR", t.TempDir())
	script := "#!/bin/sh\n[ \"$GOTOOLCHAIN\" = local ] || exit 1\n[ \"$PWD\" -ef \"$TMPDIR\" ] || exit 1\nprintf '%s' '{\"GOCACHE\":\"/discovered/build\",\"GOMODCACHE\":\"/discovered/mod\"}'\n"
	if err := os.WriteFile(filepath.Join(root, "go"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	// An unavailable module toolchain in the caller's cwd must not affect discovery.
	t.Chdir(root)
	if err := os.WriteFile("go.mod", []byte("module example.test/unavailable\ngo 1.99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := resolve(context.Background())
	if err != nil || got != (Paths{Build: "/discovered/build", Modules: "/discovered/mod"}) {
		t.Fatalf("got %+v, %v", got, err)
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
