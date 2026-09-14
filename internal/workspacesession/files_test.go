package workspacesession_test

import (
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspacesession"
)

func TestNormalizePath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		want    string
		wantErr bool
	}{
		{name: "empty is the worktree root"},
		{name: "dot is the worktree root", value: "."},
		{name: "slash is the worktree root", value: "/"},
		{name: "plain file", value: "main.go", want: "main.go"},
		{name: "nested", value: "internal/hubserver/service.go", want: "internal/hubserver/service.go"},
		{name: "leading dot slash trimmed", value: "./internal/x.go", want: "internal/x.go"},
		{name: "redundant segments cleaned", value: "internal/./hubserver//x.go", want: "internal/hubserver/x.go"},
		{name: "interior traversal that stays inside", value: "internal/hubserver/../x.go", want: "internal/x.go"},
		{name: "absolute refused", value: "/etc/passwd", wantErr: true},
		{name: "traversal out refused", value: "../secrets", wantErr: true},
		{name: "traversal out through a segment refused", value: "internal/../../secrets", wantErr: true},
		{name: "bare parent refused", value: "..", wantErr: true},
		{name: "NUL refused", value: "main\x00.go", wantErr: true},
		{name: "backslash separator refused", value: `internal\x.go`, wantErr: true},
		{name: "windows absolute refused", value: `\\server\share`, wantErr: true},
		{name: "over the cap refused", value: strings.Repeat("a", workspacesession.MaxPathBytes+1), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := workspacesession.NormalizePath(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("NormalizePath(%q) = %q, want an error", test.value, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizePath(%q) = %v", test.value, err)
			}
			if got != test.want {
				t.Fatalf("NormalizePath(%q) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}

func TestDenylist(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		extra  []string
		path   string
		denied bool
	}{
		{name: "worktree root is readable"},
		{name: "an ordinary file is readable", path: "internal/hubserver/service.go"},
		{name: "git itself is listable", path: ".git"},
		{name: "git HEAD is readable", path: ".git/HEAD"},
		{name: "the object store is denied", path: ".git/objects/ab/cdef", denied: true},
		{name: "the git config is denied", path: ".git/config", denied: true},
		{name: "a nested checkout's objects are denied", path: "vendor/dep/.git/objects/pack", denied: true},
		{name: "node_modules anywhere is denied", path: "web/conversation/node_modules/react/index.js", denied: true},
		{name: "dotenv is denied", path: ".env", denied: true},
		{name: "a dotenv variant is denied", path: "config/.env.production", denied: true},
		{name: "a pem is denied", path: "certs/server.pem", denied: true},
		{name: "a key is denied", path: "certs/server.key", denied: true},
		{name: "a p12 is denied", path: "certs/bundle.p12", denied: true},
		{name: "an ssh key is denied", path: "home/id_rsa", denied: true},
		{name: "an ssh public key is denied too", path: "home/id_rsa.pub", denied: true},
		{name: "a configured glob denies by name", extra: []string{"*.secret"}, path: "deploy/prod.secret", denied: true},
		{name: "a configured glob denies a whole directory", extra: []string{"secrets"}, path: "secrets/prod/key.txt", denied: true},
		{name: "a configured path glob denies that path", extra: []string{"deploy/*"}, path: "deploy/keys", denied: true},
		{name: "a configured glob leaves everything else alone", extra: []string{"*.secret"}, path: "deploy/prod.yaml"},
		{name: "a path that leaves the worktree is denied", path: "../outside", denied: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			list, err := workspacesession.NewDenylist(test.extra)
			if err != nil {
				t.Fatalf("NewDenylist(%v) = %v", test.extra, err)
			}
			if got := list.Denied(test.path); got != test.denied {
				t.Fatalf("Denied(%q) = %v, want %v", test.path, got, test.denied)
			}
		})
	}
}

func TestNewDenylistRefusesAnInvalidPattern(t *testing.T) {
	t.Parallel()
	if _, err := workspacesession.NewDenylist([]string{"["}); err == nil {
		t.Fatal("an unparseable glob must be refused: a pattern that never matches is a hole in the filter")
	}
	list, err := workspacesession.NewDenylist([]string{"  ", ""})
	if err != nil {
		t.Fatalf("NewDenylist with blank patterns = %v", err)
	}
	if len(list.Extra) != 0 {
		t.Fatalf("blank patterns survived as %v", list.Extra)
	}
}

// TestDenylistAgreesWithTheStoredDiffFilter is the guarantee section 18.5
// states in words: "a later reader is not shown what the live reader was not".
// The live surface and the stored attempt diff must therefore refuse the same
// fixed set, which they do by asking the same function; this proves it.
func TestDenylistAgreesWithTheStoredDiffFilter(t *testing.T) {
	t.Parallel()
	paths := []string{
		"internal/hubserver/service.go", ".git/HEAD", ".git/config", ".git/objects/ab/cd",
		"node_modules/react/index.js", ".env", "certs/server.pem", "home/id_rsa",
		"deploy/prod.yaml", "vendor/dep/.git/objects/pack",
	}
	var list workspacesession.Denylist
	for _, path := range paths {
		if got, want := list.Denied(path), tracker.DiffPathDenied(path); got != want {
			t.Errorf("Denied(%q) = %v, but the stored diff filter says %v", path, got, want)
		}
	}
}

func TestDeniedDiffPath(t *testing.T) {
	t.Parallel()
	var list workspacesession.Denylist
	tests := []struct {
		name    string
		newPath string
		oldPath string
		denied  bool
	}{
		{name: "neither side denied", newPath: "a.go", oldPath: "b.go"},
		{name: "the new side denied", newPath: ".env", oldPath: "b.go", denied: true},
		// A rename out of a denied location has no current canonical target,
		// which is exactly why the diff filter works on the path string.
		{name: "the old side denied", newPath: "a.go", oldPath: "certs/server.key", denied: true},
		{name: "no old path", newPath: "a.go"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := list.DeniedDiffPath(test.newPath, test.oldPath); got != test.denied {
				t.Fatalf("DeniedDiffPath(%q, %q) = %v, want %v", test.newPath, test.oldPath, got, test.denied)
			}
		})
	}
}

func TestValidFilesRequest(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"list", "stat", "read", "watch"} {
		if !workspacesession.ValidFilesRequest(name) {
			t.Errorf("ValidFilesRequest(%q) = false, want true", name)
		}
	}
	// A runner-bound answer is not a request: a person sending "listed" is
	// answered with unknown_frame, not served.
	for _, name := range []string{"listed", "content", "changed", "write", ""} {
		if workspacesession.ValidFilesRequest(name) {
			t.Errorf("ValidFilesRequest(%q) = true, want false", name)
		}
	}
}
