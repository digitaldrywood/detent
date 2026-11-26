package gitdiff

import (
	"strings"
	"testing"
)

func TestParseGitNumstat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		output  string
		want    []FileDiff
		wantErr bool
	}{
		{name: "empty"},
		{
			name:   "text and binary",
			output: "3\t1\ta.go\x00-\t-\tb.png\x00",
			want: []FileDiff{
				{Path: "a.go", Status: FileDiffModified, Additions: 3, Deletions: 1},
				{Path: "b.png", Status: FileDiffModified, Binary: true},
			},
		},
		{
			name:   "rename",
			output: "1\t0\t\x00old.go\x00new.go\x00",
			want:   []FileDiff{{Path: "new.go", OldPath: "old.go", Status: FileDiffRenamed, Additions: 1}},
		},
		{name: "malformed record", output: "oops\x00", wantErr: true},
		{name: "non numeric additions", output: "x\t1\ta.go\x00", wantErr: true},
		{name: "non numeric deletions", output: "1\tx\ta.go\x00", wantErr: true},
		{name: "truncated rename", output: "1\t0\t\x00old.go\x00", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseGitNumstat(test.output)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
			if err != nil {
				return
			}
			if len(got) != len(test.want) {
				t.Fatalf("len = %d, want %d (%+v)", len(got), len(test.want), got)
			}
			for index := range got {
				if got[index] != test.want[index] {
					t.Fatalf("file %d = %+v, want %+v", index, got[index], test.want[index])
				}
			}
		})
	}
}

func TestSplitGitPatchAndAttach(t *testing.T) {
	t.Parallel()
	patch := "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-a\n+b\ndiff --git a/b.go b/b.go\n@@ -0,0 +1 @@\n+c\n"
	sections := splitGitPatch(patch)
	if len(sections) != 2 {
		t.Fatalf("sections = %d, want 2: %q", len(sections), sections)
	}
	if !strings.HasSuffix(sections[0], "+b\n") || !strings.HasPrefix(sections[1], "diff --git a/b.go") {
		t.Fatalf("sections split wrongly: %q", sections)
	}
	files := []FileDiff{{Path: "a.go"}, {Path: "b.go"}}
	attachGitPatches(files, sections)
	if files[0].Patch != sections[0] || files[1].Patch != sections[1] {
		t.Fatal("patches were not attached in order")
	}
	// A mismatched count attaches nothing rather than the wrong patch.
	mismatched := []FileDiff{{Path: "a.go"}}
	attachGitPatches(mismatched, sections)
	if mismatched[0].Patch != "" {
		t.Fatal("a mismatched section count must attach no patch")
	}
	if splitGitPatch("") != nil {
		t.Fatal("an empty patch has no sections")
	}
}

// isolateFileDiffGitConfig isolates one test's git commands from the host's global and system git
// configuration, so a developer's global excludes file (which commonly lists
// .env) cannot hide the files the test asserts on. It uses t.Setenv, so the
// calling test must not run in parallel.
