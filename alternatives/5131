//go:build !windows

package toolcache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveNeutralToolchain(t *testing.T) {

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

func TestResolveFailedToolchain(t *testing.T) {
	testResolveFallback(t, true, false, false)
}

func TestRemoveLegacyChmodPermissionFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
	}{{"read-only subtree", 0o500}, {"inaccessible subtree", 0o000}} {
		t.Run(test.name, func(t *testing.T) { testRemoveLegacyChmodFailure(t, test.mode, true) })
	}
}
