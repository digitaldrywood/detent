package gotestreceipt

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifierBoundsInputWork(t *testing.T) {
	root := t.TempDir()
	sparseFile(t, filepath.Join(root, "huge"), maxHashedBytes+1)
	sparseFile(t, filepath.Join(root, "first-half"), maxHashedBytes/2+1)
	sparseFile(t, filepath.Join(root, "second-half"), maxHashedBytes/2+1)
	if err := os.WriteFile(filepath.Join(root, "small"), []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []struct {
		name    string
		entries int
	}{{"wide", maxDirectoryEntries + 1}, {"narrow", 3}} {
		if err := os.Mkdir(filepath.Join(root, dir.name), 0o700); err != nil {
			t.Fatal(err)
		}
		for i := range dir.entries {
			if err := os.WriteFile(filepath.Join(root, dir.name, strconv.Itoa(i)), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	tests := []struct {
		name   string
		inputs []string
		want   error
	}{
		{name: "small file and directory", inputs: []string{"open\tm\tsmall", "open\tm\tnarrow"}},
		{name: "file larger than the byte limit", inputs: []string{"open\tm\thuge"}, want: errInputBytes},
		{name: "files that together exceed the byte limit", inputs: []string{"open\tm\tfirst-half", "open\tm\tsecond-half"}, want: errInputBytes},
		{name: "directory wider than the entry limit", inputs: []string{"open\tm\twide"}, want: errInputEntries},
		{name: "stat of a huge file reads no content", inputs: []string{"stat\tm\thuge", "stat\tm\twide"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Digest(tt.inputs, Environment{DigestKey: testDigestKey, Root: root})
			if tt.want == nil && err != nil {
				t.Fatalf("Digest() error = %v", err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Digest() error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestVerifierRejectsInputsChangedDuringLookup(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		change func(t *testing.T, path string)
		want   bool
	}{
		{name: "unchanged input", change: func(*testing.T, string) {}, want: true},
		{name: "content and size changed", change: func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "modification time changed with same content", change: func(t *testing.T, path string) {
			if err := os.Chtimes(path, time.Time{}, time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file replaced with same size and time", change: func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			replacement := path + ".new"
			if err := os.WriteFile(replacement, []byte("two"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(replacement, time.Time{}, info.ModTime()); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(replacement, path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file rewritten in place with same size and time", change: func(t *testing.T, path string) {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, time.Time{}, info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory entry added", input: "open\tm\tdir", change: func(t *testing.T, path string) {
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "dir", "added"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "file removed", change: func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "shared")
			if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
				t.Fatal(err)
			}
			inputs := []string{"open\tm\tshared"}
			if tt.input != "" {
				inputs = []string{tt.input}
			}
			if _, err := os.ReadFile(path); err != nil {
				t.Fatal(err)
			}
			verifier := NewVerifier(Environment{DigestKey: testDigestKey, Root: root})
			digest, err := verifier.Digest(inputs)
			if err != nil {
				t.Fatal(err)
			}
			tt.change(t, path)
			if got := verifier.Match(Candidate{Inputs: inputs, Digest: digest}); got != tt.want {
				t.Fatalf("Match() = %v, want %v", got, tt.want)
			}
		})
	}
}

type boundedDirectory struct {
	t         *testing.T
	requested int
}

func (d *boundedDirectory) ReadDir(n int) ([]os.DirEntry, error) {
	d.requested = n
	if n <= 0 || n > maxDirectoryEntries+1 {
		d.t.Fatalf("unbounded directory read: %d", n)
	}
	return make([]os.DirEntry, n), nil
}

func TestOversizedDirectoryStopsBeforeFullRead(t *testing.T) {
	dir := &boundedDirectory{t: t}
	if _, err := NewVerifier(Environment{}).readDirectory(dir.ReadDir); err == nil {
		t.Fatal("oversized directory accepted")
	}
	if dir.requested != maxDirectoryEntries+1 {
		t.Fatalf("read %d entries", dir.requested)
	}
}
