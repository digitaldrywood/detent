package tracker

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// Stored attempt diffs (decisions section 18.5). This file is the wire
// contract the runner posts and the hub stores, together with the two
// write-side rules both sides have to agree on: the size caps and the files
// denylist of section 18.4. Keeping them here means the producer cannot ship
// a patch the hub would have had to strip, and the hub does not have to trust
// that the producer stripped it.

// Limits on a stored diff.
const (
	// MaxDiffPatchBytes is the largest patch stored for one file. A longer
	// patch is cut at this bound and the file is reported truncated.
	MaxDiffPatchBytes = 1 << 20
	// MaxDiffBytes is the largest whole diff the hub accepts. Beyond it the
	// post is refused with diff_too_large and the producer re-posts the same
	// file list without patches.
	MaxDiffBytes = 20 << 20
	// MaxDiffFiles bounds how many files one diff may name, so a runaway
	// worktree cannot make one stored diff unbounded.
	MaxDiffFiles = 10000
	// MaxDiffPathBytes bounds one reported path.
	MaxDiffPathBytes = 4096
	// MaxDiffSHABytes bounds a reported commit identity.
	MaxDiffSHABytes = 64
)

// Diff generation sources.
const (
	DiffSourceAttempt   = "attempt"
	DiffSourceWorkspace = "workspace"
)

// Diff file statuses, in the order section 18.5 lists them.
const (
	DiffStatusAdded    = "added"
	DiffStatusModified = "modified"
	DiffStatusDeleted  = "deleted"
	DiffStatusRenamed  = "renamed"
)

// DiffProducer is the ownership generation that wrote a diff. It is not
// necessarily the subject attempt: while an attempt runs its own lease is the
// producer, and after it finishes only a workspace lease on that attempt may
// write.
type DiffProducer struct {
	Kind         string       `json:"kind"`
	ID           string       `json:"id,omitempty"`
	RunnerID     string       `json:"runner_id,omitempty"`
	LeaseID      LeaseID      `json:"lease_id"`
	FencingToken FencingToken `json:"fencing_token"`
}

// DiffGeneration orders the diffs of one attempt. For an attempt producer Seq
// is the run event sequence the diff belongs to; a workspace producer carries
// its own id and its own monotonic counter, and its diffs are stored beside
// the attempt's rather than over them.
type DiffGeneration struct {
	Source string `json:"source"`
	ID     string `json:"id,omitempty"`
	Seq    int64  `json:"seq"`
}

// AttemptDiffFile is one changed file. Patch is empty for three
// distinguishable reasons: Binary content, a Denied path, or a patch cut at
// MaxDiffPatchBytes and reported Truncated. The counts survive all three.
type AttemptDiffFile struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Binary    bool   `json:"binary"`
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated"`
	Denied    bool   `json:"denied"`
}

// AttemptDiffRequest is the body of POST {nativeBase}/attempts/:id/diff. It
// carries no idempotency key: the generation is the idempotency record, and a
// seq at or below the stored one is refused as stale_generation.
type AttemptDiffRequest struct {
	Producer   DiffProducer      `json:"producer"`
	Generation DiffGeneration    `json:"generation"`
	BaseSHA    string            `json:"base_sha"`
	HeadSHA    string            `json:"head_sha"`
	Files      []AttemptDiffFile `json:"files"`
}

// AttemptDiff is one stored generation of one attempt's worktree.
type AttemptDiff struct {
	ID         string            `json:"id"`
	AttemptID  string            `json:"attempt_id"`
	Producer   DiffProducer      `json:"producer"`
	Generation DiffGeneration    `json:"generation"`
	BaseSHA    string            `json:"base_sha"`
	HeadSHA    string            `json:"head_sha"`
	Files      []AttemptDiffFile `json:"files"`
	FileCount  int               `json:"file_count"`
	PatchBytes int64             `json:"patch_bytes"`
	Truncated  bool              `json:"truncated"`
	CreatedAt  time.Time         `json:"created_at"`
}

// WorkItemDiff is what GET {nativeBase}/work-items/:item/diff answers with:
// the latest stored diff on the issue, or an explicit absence.
//
// The attempt-addressed read above answers 404 for an attempt that posted no
// diff, which is right for a caller that named one attempt and wrong for a
// client asking "does this issue have a diff at all". Such a client would
// otherwise walk the attempt list and take a 404 for every attempt that never
// ran long enough to checkpoint — several round trips, and a console error
// apiece, to learn one fact. The issue-addressed read is one request with a
// nullable answer, and migration 00026's attempt_diffs_item_idx
// (organization_id, project_id, work_item_id, created_at) is the index it was
// already given.
type WorkItemDiff struct {
	Diff *AttemptDiff `json:"diff"`
}

// AttemptDiffReceipt is what a successful post answers with. The producer
// needs the generation it landed on and nothing else.
type AttemptDiffReceipt struct {
	Accepted   bool           `json:"accepted"`
	DiffID     string         `json:"diff_id"`
	Generation DiffGeneration `json:"generation"`
	FileCount  int            `json:"file_count"`
	PatchBytes int64          `json:"patch_bytes"`
	Truncated  bool           `json:"truncated"`
}

// diffDeniedSegments are path segments refused wherever they appear.
var diffDeniedSegments = []string{"node_modules"}

// diffDeniedPairs are two-segment sequences refused wherever they appear, so a
// nested checkout's .git is filtered as well as the root one.
var diffDeniedPairs = [][2]string{{".git", "objects"}, {".git", "config"}}

// diffDeniedSuffixes are the file-name suffixes of the project's secret
// patterns: "*.pem", "*.key", "*.p12".
var diffDeniedSuffixes = []string{".pem", ".key", ".p12"}

// diffDeniedPrefixes are the file-name prefixes of the project's secret
// patterns: ".env*", "id_rsa*".
var diffDeniedPrefixes = []string{".env", "id_rsa"}

// DiffPathDenied reports whether value matches the files denylist of section
// 18.4. It works on the path string alone, because a deleted or renamed file
// has no current canonical target to resolve against, and it is applied to
// both a file's path and its old_path.
//
// It is the fixed half of the filter, and deliberately the only half that
// lives here. workspaces.files.deny now exists, and workspacesession.Denylist
// adds it on top of this function for the live files and diff surfaces; the
// stored attempt diff is filtered on write by a runner that has the project's
// configuration and by a hub path that does not, so folding the configured
// globs in here would mean the two halves of one filter disagreeing about the
// same file. Whatever reads this must ask workspacesession.Denylist when it
// has a project in scope.
func DiffPathDenied(value string) bool {
	segments := diffPathSegments(value)
	if len(segments) == 0 {
		return false
	}
	for index, segment := range segments {
		for _, denied := range diffDeniedSegments {
			if segment == denied {
				return true
			}
		}
		if index+1 >= len(segments) {
			continue
		}
		for _, pair := range diffDeniedPairs {
			if segment == pair[0] && segments[index+1] == pair[1] {
				return true
			}
		}
	}
	name := path.Base(segments[len(segments)-1])
	for _, suffix := range diffDeniedSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	for _, prefix := range diffDeniedPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// diffPathSegments lowercases and splits a reported path. Backslashes are
// treated as separators too, so a Windows-style path cannot slip a denied
// segment past a comparison that only knows about "/".
func diffPathSegments(value string) []string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return nil
	}
	var segments []string
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool { return r == '/' || r == '\\' }) {
		if segment == "." {
			continue
		}
		segments = append(segments, segment)
	}
	return segments
}

// FilterDiffFile applies the denylist to one file. A denied file keeps its
// counts and its status, loses its patch and is marked denied, whatever its
// status. Both path and old_path are checked, because a rename can move a file
// into or out of a denied location.
func FilterDiffFile(file AttemptDiffFile) AttemptDiffFile {
	if DiffPathDenied(file.Path) || DiffPathDenied(file.OldPath) {
		file.Denied = true
		file.Patch = ""
		file.Truncated = false
	}
	return file
}

// TruncateDiffPatch cuts a patch at MaxDiffPatchBytes and reports whether it
// had to. The cut backs up to the last complete rune, so a stored patch is
// still valid UTF-8 for the renderer.
func TruncateDiffPatch(patch string) (string, bool) {
	if len(patch) <= MaxDiffPatchBytes {
		return patch, false
	}
	cut := MaxDiffPatchBytes
	for cut > 0 && patch[cut]&0xC0 == 0x80 {
		cut--
	}
	return patch[:cut], true
}

// NormalizeDiffFiles applies the per-file patch cap and the denylist to every
// file in order and reports the patch bytes that survived. It is the one place
// the two write-side rules live, so the runner and the hub cannot disagree
// about what a stored file looks like.
func NormalizeDiffFiles(files []AttemptDiffFile) ([]AttemptDiffFile, int64) {
	normalized := make([]AttemptDiffFile, 0, len(files))
	var total int64
	for _, file := range files {
		if file.Binary {
			file.Patch, file.Truncated = "", false
		}
		patch, cut := TruncateDiffPatch(file.Patch)
		file.Patch, file.Truncated = patch, file.Truncated || cut
		file = FilterDiffFile(file)
		total += int64(len(file.Patch))
		normalized = append(normalized, file)
	}
	return normalized, total
}

// StripDiffPatches drops every patch and keeps every count. It is what a
// producer posts after diff_too_large: the file list without patches.
func StripDiffPatches(files []AttemptDiffFile) []AttemptDiffFile {
	stripped := make([]AttemptDiffFile, 0, len(files))
	for _, file := range files {
		file.Patch, file.Truncated = "", file.Truncated || !file.Binary && !file.Denied
		stripped = append(stripped, file)
	}
	return stripped
}

// ValidateDiffGeneration checks the generation a producer reported.
func ValidateDiffGeneration(generation DiffGeneration) error {
	switch generation.Source {
	case DiffSourceAttempt:
		if generation.ID != "" {
			return fmt.Errorf("%w: an attempt generation carries no id", ErrInvalidWorkEvent)
		}
	case DiffSourceWorkspace:
		if strings.TrimSpace(generation.ID) == "" {
			return fmt.Errorf("%w: a workspace generation requires its workspace id", ErrInvalidWorkEvent)
		}
	default:
		return fmt.Errorf("%w: generation source must be attempt or workspace", ErrInvalidWorkEvent)
	}
	if generation.Seq <= 0 {
		return fmt.Errorf("%w: generation seq must be positive", ErrInvalidWorkEvent)
	}
	return nil
}

// ValidateDiffFiles checks a normalized file list. It never rewrites a file: a
// caller that has not run NormalizeDiffFiles is reporting something the hub
// refuses rather than silently repairs.
func ValidateDiffFiles(files []AttemptDiffFile) error {
	if len(files) > MaxDiffFiles {
		return fmt.Errorf("%w: at most %d files in one diff", ErrInvalidWorkEvent, MaxDiffFiles)
	}
	for _, file := range files {
		switch file.Status {
		case DiffStatusAdded, DiffStatusModified, DiffStatusDeleted, DiffStatusRenamed:
		default:
			return fmt.Errorf("%w: file status %q is not added, modified, deleted or renamed", ErrInvalidWorkEvent, file.Status)
		}
		if strings.TrimSpace(file.Path) == "" || len(file.Path) > MaxDiffPathBytes || len(file.OldPath) > MaxDiffPathBytes {
			return fmt.Errorf("%w: a file path is required and bounded to %d bytes", ErrInvalidWorkEvent, MaxDiffPathBytes)
		}
		if file.Additions < 0 || file.Deletions < 0 {
			return fmt.Errorf("%w: file counts cannot be negative", ErrInvalidWorkEvent)
		}
		if file.Denied && file.Patch != "" {
			return fmt.Errorf("%w: a denied file cannot carry a patch", ErrInvalidWorkEvent)
		}
		if file.Binary && file.Patch != "" {
			return fmt.Errorf("%w: a binary file cannot carry a patch", ErrInvalidWorkEvent)
		}
		if len(file.Patch) > MaxDiffPatchBytes {
			return fmt.Errorf("%w: a patch is bounded to %d bytes", ErrInvalidWorkEvent, MaxDiffPatchBytes)
		}
	}
	return nil
}
