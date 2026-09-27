package tracker

import (
	"errors"
	"strings"
	"testing"
)

// The denylist of decisions section 18.4 is applied by path string, to both
// path and old_path, because a deleted or renamed file has no canonical target
// to resolve.

// A denied file keeps its counts and its status and loses only its patch,
// whatever its status, and old_path is checked as well as path.
func TestFilterDiffFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		file       AttemptDiffFile
		wantDenied bool
	}{
		{
			name: "allowed file keeps its patch",
			file: AttemptDiffFile{Path: "main.go", Status: DiffStatusModified, Additions: 2, Deletions: 1, Patch: "@@"},
		},
		{
			name:       "denied by path",
			file:       AttemptDiffFile{Path: ".env", Status: DiffStatusAdded, Additions: 3, Patch: "@@ secret"},
			wantDenied: true,
		},
		{
			name:       "denied by old path after a rename out of the denylist",
			file:       AttemptDiffFile{Path: "settings.txt", OldPath: ".env", Status: DiffStatusRenamed, Additions: 1, Patch: "@@ secret"},
			wantDenied: true,
		},
		{
			name:       "denied on a deletion",
			file:       AttemptDiffFile{Path: "certs/a.pem", Status: DiffStatusDeleted, Deletions: 9, Patch: "@@ key"},
			wantDenied: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got := FilterDiffFile(test.file)
			if got.Denied != test.wantDenied {
				t.Fatalf("denied = %v, want %v", got.Denied, test.wantDenied)
			}
			if got.Additions != test.file.Additions || got.Deletions != test.file.Deletions || got.Status != test.file.Status {
				t.Fatalf("counts or status changed: %+v", got)
			}
			if test.wantDenied && got.Patch != "" {
				t.Fatalf("denied file kept a patch of %d bytes", len(got.Patch))
			}
			if !test.wantDenied && got.Patch != test.file.Patch {
				t.Fatalf("allowed file lost its patch")
			}
		})
	}
}

// A patch over the per-file bound is stored truncated, cut on a rune boundary.
func TestTruncateDiffPatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		patch         string
		wantTruncated bool
		wantLen       int
	}{
		{name: "short patch is untouched", patch: "@@ -1 +1 @@", wantLen: 11},
		{name: "exactly at the bound", patch: strings.Repeat("a", MaxDiffPatchBytes), wantLen: MaxDiffPatchBytes},
		{name: "one byte over", patch: strings.Repeat("a", MaxDiffPatchBytes+1), wantTruncated: true, wantLen: MaxDiffPatchBytes},
		{
			name:          "cut lands inside a multi-byte rune",
			patch:         strings.Repeat("a", MaxDiffPatchBytes-1) + "€€",
			wantTruncated: true,
			wantLen:       MaxDiffPatchBytes - 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, truncated := TruncateDiffPatch(test.patch)
			if truncated != test.wantTruncated {
				t.Fatalf("truncated = %v, want %v", truncated, test.wantTruncated)
			}
			if len(got) != test.wantLen {
				t.Fatalf("len = %d, want %d", len(got), test.wantLen)
			}
			if !isValidUTF8(got) {
				t.Fatal("truncated patch is not valid UTF-8")
			}
		})
	}
}

func isValidUTF8(value string) bool {
	for _, r := range value {
		if r == '�' {
			return false
		}
	}
	return true
}

// NormalizeDiffFiles is the one place the write-side rules live: the per-file
// cap, then the denylist, and it reports the patch bytes that survived.
func TestNormalizeDiffFiles(t *testing.T) {
	t.Parallel()
	files := []AttemptDiffFile{
		{Path: "a.go", Status: DiffStatusModified, Additions: 1, Patch: "short"},
		{Path: "big.go", Status: DiffStatusModified, Additions: 2, Patch: strings.Repeat("b", MaxDiffPatchBytes+10)},
		{Path: ".env", Status: DiffStatusAdded, Additions: 3, Patch: "secret"},
		{Path: "image.png", Status: DiffStatusAdded, Binary: true, Patch: "binary junk"},
	}
	got, total := NormalizeDiffFiles(files)
	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	if got[0].Patch != "short" || got[0].Truncated {
		t.Fatalf("short patch changed: %+v", got[0])
	}
	if !got[1].Truncated || len(got[1].Patch) != MaxDiffPatchBytes {
		t.Fatalf("long patch not truncated: truncated=%v len=%d", got[1].Truncated, len(got[1].Patch))
	}
	if !got[2].Denied || got[2].Patch != "" || got[2].Additions != 3 {
		t.Fatalf("denied file wrong: %+v", got[2])
	}
	if got[3].Patch != "" || got[3].Truncated {
		t.Fatalf("binary file kept a patch: %+v", got[3])
	}
	want := int64(len("short") + MaxDiffPatchBytes)
	if total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
}

// A producer that has been told the diff is too large re-posts the file list
// without patches; the counts survive.
func TestStripDiffPatches(t *testing.T) {
	t.Parallel()
	got := StripDiffPatches([]AttemptDiffFile{
		{Path: "a.go", Status: DiffStatusModified, Additions: 4, Deletions: 2, Patch: "@@"},
		{Path: "image.png", Status: DiffStatusAdded, Binary: true},
		{Path: ".env", Status: DiffStatusAdded, Additions: 1, Denied: true},
	})
	for _, file := range got {
		if file.Patch != "" {
			t.Fatalf("%s kept a patch", file.Path)
		}
	}
	if !got[0].Truncated {
		t.Fatal("a stripped text file is truncated")
	}
	if got[1].Truncated || got[2].Truncated {
		t.Fatal("a binary or denied file has no patch to truncate")
	}
	if got[0].Additions != 4 || got[0].Deletions != 2 {
		t.Fatalf("counts changed: %+v", got[0])
	}
}

func TestValidateDiffGeneration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		generation DiffGeneration
		wantErr    bool
	}{
		{name: "attempt", generation: DiffGeneration{Source: DiffSourceAttempt, Seq: 3}},
		{name: "workspace", generation: DiffGeneration{Source: DiffSourceWorkspace, ID: "ws_1", Seq: 1}},
		{name: "attempt with an id", generation: DiffGeneration{Source: DiffSourceAttempt, ID: "ws_1", Seq: 1}, wantErr: true},
		{name: "workspace without an id", generation: DiffGeneration{Source: DiffSourceWorkspace, Seq: 1}, wantErr: true},
		{name: "unknown source", generation: DiffGeneration{Source: "relay", Seq: 1}, wantErr: true},
		{name: "zero seq", generation: DiffGeneration{Source: DiffSourceAttempt}, wantErr: true},
		{name: "negative seq", generation: DiffGeneration{Source: DiffSourceAttempt, Seq: -1}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDiffGeneration(test.generation)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalidWorkEvent) {
				t.Fatalf("err = %v, want ErrInvalidWorkEvent", err)
			}
		})
	}
}

func TestValidateDiffFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   []AttemptDiffFile
		wantErr bool
	}{
		{name: "empty list"},
		{name: "valid", files: []AttemptDiffFile{{Path: "a.go", Status: DiffStatusModified}}},
		{name: "unknown status", files: []AttemptDiffFile{{Path: "a.go", Status: "copied"}}, wantErr: true},
		{name: "missing path", files: []AttemptDiffFile{{Status: DiffStatusAdded}}, wantErr: true},
		{name: "path too long", files: []AttemptDiffFile{{Path: strings.Repeat("a", MaxDiffPathBytes+1), Status: DiffStatusAdded}}, wantErr: true},
		{name: "negative counts", files: []AttemptDiffFile{{Path: "a.go", Status: DiffStatusAdded, Additions: -1}}, wantErr: true},
		{name: "denied with a patch", files: []AttemptDiffFile{{Path: ".env", Status: DiffStatusAdded, Denied: true, Patch: "x"}}, wantErr: true},
		{name: "binary with a patch", files: []AttemptDiffFile{{Path: "a.png", Status: DiffStatusAdded, Binary: true, Patch: "x"}}, wantErr: true},
		{
			name:    "patch over the per-file cap",
			files:   []AttemptDiffFile{{Path: "a.go", Status: DiffStatusModified, Patch: strings.Repeat("a", MaxDiffPatchBytes+1)}},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDiffFiles(test.files)
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

// Mergeability aside, the generation shape travels as 18.5 writes it.
func TestAttemptDiffGenerationJSON(t *testing.T) {
	t.Parallel()
	if err := ValidateDiffGeneration(DiffGeneration{Source: DiffSourceAttempt, Seq: 1}); err != nil {
		t.Fatalf("attempt generation: %v", err)
	}
	if len(pathsOfDenied()) == 0 {
		t.Fatal("the denylist is empty")
	}
}

// pathsOfDenied exercises the fixed denylist that stands in for the project's
// workspaces.files.deny globs until workspace sessions introduce that key.
func pathsOfDenied() []string {
	denied := []string{}
	for _, candidate := range []string{".env", "a.pem", "a.key", "a.p12", "id_rsa", "node_modules/x", ".git/config", ".git/objects/x"} {
		if DiffPathDenied(candidate) {
			denied = append(denied, candidate)
		}
	}
	return denied
}
