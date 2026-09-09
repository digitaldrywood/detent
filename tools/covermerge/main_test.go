package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeProfiles(t *testing.T) {
	t.Parallel()
	const first = "mode: atomic\nexample.com/a/a.go:1.1,2.2 1 2\n"
	for _, tt := range []struct {
		name, second, want string
	}{
		{"disjoint", "mode: atomic\nexample.com/b/b.go:3.1,4.2 2 0\n", ""},
		{"disjoint set", "mode: set\nexample.com/b/b.go:3.1,4.2 2 0\n", ""},
		{"mismatched mode", "mode: count\nexample.com/b/b.go:3.1,4.2 2 0\n", "incompatible"},
		{"invalid set count", "mode: set\nexample.com/b/b.go:3.1,4.2 2 2\n", "invalid coverage count"},
		{"missing header", "example.com/b/b.go:3.1,4.2 2 0\n", "incompatible"},
		{"empty", "mode: atomic\n", "empty"},
		{"overlap", first, "overlapping"},
		{"changed block in same file", "mode: atomic\nexample.com/a/a.go:8.1,9.2 1 0\n", "overlapping"},
		{"invalid block", "mode: atomic\ninvalid\n", "invalid coverage block"},
		{"invalid path", "mode: atomic\n:1.1,2.2 1 0\n", "invalid coverage location"},
		{"invalid location", "mode: atomic\na.go:1.1,2 1 0\n", "invalid coverage location"},
		{"negative count", "mode: atomic\na.go:1.1,2.2 1 -1\n", "invalid coverage count"},
		{"missing file", "", "merge coverage"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			paths := []string{filepath.Join(dir, "a.out"), filepath.Join(dir, "b.out")}
			for i, data := range []string{first, tt.second} {
				if data == "" {
					continue
				}
				if err := os.WriteFile(paths[i], []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run(paths, &stdout, &stderr)
			if tt.want == "" {
				second := strings.TrimPrefix(strings.TrimPrefix(tt.second, "mode: atomic\n"), "mode: set\n")
				if code != 0 || stdout.String() != "mode: set\nexample.com/a/a.go:1.1,2.2 1 1\n"+second {
					t.Fatalf("merge = %d, %s: %s", code, &stdout, &stderr)
				}
			} else if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("invalid profile published output: %d, %s: %s", code, &stdout, &stderr)
			}
		})
	}
}

func TestMergeArguments(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"one.out"}, {"-invalid"}} {
		var output bytes.Buffer
		if code := run(args, &output, &output); code != 2 {
			t.Fatalf("run(%q) = %d", args, code)
		}
	}
}

func TestCoverageLocation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		location string
		valid    bool
	}{
		{"1.2,3.4", true}, {"0.2,3.4", false}, {"a.2,3.4", false},
		{"1.2.3.4", false}, {"1,2,3.4", false}, {"1.2,3.4.5", false},
	} {
		if validLocation(tt.location) != tt.valid {
			t.Errorf("location %q: want valid=%t", tt.location, tt.valid)
		}
	}
}
