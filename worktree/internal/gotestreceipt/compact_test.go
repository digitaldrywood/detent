package gotestreceipt

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestParseTestLogCompactsOversizedInputs(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"wide", "other", "nested"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	var log strings.Builder
	log.WriteString("# test log\n")
	member := func(i int) string { return filepath.Join(root, "wide", strings.Repeat("input-", 12)+strconv.Itoa(i)) }
	for i := range 1500 {
		name := member(i)
		if err := os.WriteFile(name, []byte(strconv.Itoa(i)), 0o600); err != nil {
			t.Fatal(err)
		}
		log.WriteString("open " + name + "\n")
	}
	if err := os.WriteFile(filepath.Join(root, "other", "single"), []byte("single"), 0o600); err != nil {
		t.Fatal(err)
	}
	log.WriteString("open " + filepath.Join(root, "other", "single") + "\nopen " + filepath.Join(root, "wide", "missing-later") + "\nstat " + filepath.Join(root, "wide", "nested-dir") + "\nstat " + member(0) + "\n")
	if err := os.WriteFile(filepath.Join(root, "wide", "missing-later"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "wide", "nested-dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := Environment{Root: root, DigestKey: testDigestKey}
	start := time.Now().Add(time.Hour)
	inputs, err := ParseTestLog(strings.NewReader(log.String()), root, env, start)
	if err != nil {
		t.Fatalf("ParseTestLog() error = %v", err)
	}
	want := []string{"open\tm\tother/single", "stat\tm\twide/" + filepath.Base(member(0)), "stat\tm\twide/nested-dir", "tree\tm\twide"}
	if !slices.Equal(inputs, want) {
		t.Fatalf("inputs = %q, want %q", inputs, want)
	}
	if err := ModifiedSince(inputs, env, start); err != nil {
		t.Fatalf("ModifiedSince() error = %v", err)
	}
	if err := ModifiedSince([]string{"tree\tm\twide"}, env, time.Now().Add(-time.Hour)); err == nil {
		t.Fatal("ModifiedSince() accepted a tree whose files changed after the test started")
	}
	digest, err := Digest(inputs, env)
	if err != nil {
		t.Fatalf("Digest() error = %v", err)
	}
	tests := []struct {
		name    string
		change  func(t *testing.T)
		wantErr error
	}{
		{name: "a read file changes content", change: func(t *testing.T) {
			if err := os.WriteFile(member(7), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a read file is removed", change: func(t *testing.T) {
			if err := os.Remove(filepath.Join(root, "wide", "missing-later")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "a directory appears in the tree", change: func(t *testing.T) {
			if err := os.Mkdir(filepath.Join(root, "wide", "appeared"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the tree grows past the directory entry limit", change: func(t *testing.T) {
			for i := range maxDirectoryEntries {
				if err := os.WriteFile(filepath.Join(root, "wide", "extra-"+strconv.Itoa(i)), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}, wantErr: errInputEntries},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.change(t)
			changed, err := Digest(inputs, env)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Digest() error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err == nil && changed == digest {
				t.Fatal("Digest() did not change after a tree member changed")
			}
			digest = changed
		})
	}
}
