package workspacesession

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// The files channel (decisions section 18.4). Read-only in this slice: writes
// happen through the model or the reader's own checkout.

// Frame types the files channel defines. Anything else on this channel is
// answered with unknown_frame and dropped.
const (
	// Person → runner.
	TypeFilesList  = "list"
	TypeFilesStat  = "stat"
	TypeFilesRead  = "read"
	TypeFilesWatch = "watch"
	// Runner → person. A stat answer reuses the request's name: the
	// direction disambiguates, and section 18.4 names both "stat".
	TypeFilesListed  = "listed"
	TypeFilesContent = "content"
	TypeFilesChanged = "changed"
)

// FilesRequestTypes lists what a person may send on the files channel.
func FilesRequestTypes() []string {
	return []string{TypeFilesList, TypeFilesStat, TypeFilesRead, TypeFilesWatch}
}

// ValidFilesRequest reports whether type names a files request.
func ValidFilesRequest(value string) bool { return slices.Contains(FilesRequestTypes(), value) }

// Entry kinds. A symlink is listed as a symlink with no target and reading it
// answers forbidden, which is what closes symlink swaps and links to in-tree
// secrets.
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
)

// Files channel limits.
const (
	MaxReadBytes = 2 << 20
	// DirectoryPage is how many entries one listed frame carries.
	DirectoryPage = 500
	// MaxReadsInFlight bounds concurrent reads on one stream.
	MaxReadsInFlight = 4
	// MaxPathBytes bounds a requested path. It is well past any real worktree
	// path and stops a request from becoming a memory cost.
	MaxPathBytes = 4096
)

// FilesRequest is the body of a list, stat, read or watch frame. Only the
// fields that frame defines are read; the rest are ignored, so one struct
// serves the whole person side of the channel.
type FilesRequest struct {
	// Path is relative to the worktree root. Empty means the root itself.
	Path   string `json:"path"`
	Cursor string `json:"cursor,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
	// ShowIgnored asks for gitignored entries in a listing.
	ShowIgnored bool `json:"show_ignored,omitempty"`
}

// FilesEntry is one row of a directory listing.
type FilesEntry struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Ignored    bool      `json:"ignored"`
	Denied     bool      `json:"denied"`
}

// FilesListed answers a list frame.
type FilesListed struct {
	Path       string       `json:"path"`
	Entries    []FilesEntry `json:"entries"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

// FilesStat answers a stat frame.
type FilesStat struct {
	Path       string    `json:"path"`
	Kind       string    `json:"kind"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	MIME       string    `json:"mime"`
	Ignored    bool      `json:"ignored"`
	Denied     bool      `json:"denied"`
}

// FilesContent answers a read frame. Data is the file's bytes; Encoding is
// "base64" when they are not valid UTF-8 text.
type FilesContent struct {
	Path      string `json:"path"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Data      string `json:"data"`
	Encoding  string `json:"encoding,omitempty"`
	Truncated bool   `json:"truncated"`
}

// FilesChanged answers a watch.
type FilesChanged struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// ErrInvalidPath reports a path the channel refuses before it ever opens
// anything. It is the request-string check; containment itself is enforced at
// open time on the canonical path, not here (section 18.4).
var ErrInvalidPath = errors.New("workspacesession: invalid path")

// NormalizePath turns a requested path into a clean worktree-relative path. It
// refuses absolute paths, parent traversal, NUL bytes and anything over
// MaxPathBytes. It is deliberately not the containment check: a caller that
// stopped here would still be open to a symlink swap, which is why the runner
// resolves the canonical path of what it actually opened.
func NormalizePath(value string) (string, error) {
	if len(value) > MaxPathBytes {
		return "", fmt.Errorf("%w: path exceeds %d bytes", ErrInvalidPath, MaxPathBytes)
	}
	if strings.ContainsRune(value, 0) {
		return "", fmt.Errorf("%w: path contains a NUL byte", ErrInvalidPath)
	}
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "./")
	if trimmed == "" || trimmed == "." || trimmed == "/" {
		return "", nil
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, `\`) {
		return "", fmt.Errorf("%w: path must be relative to the worktree", ErrInvalidPath)
	}
	if strings.Contains(trimmed, `\`) {
		// Backslashes are a path separator on the runner's Windows hosts and
		// a legal file name character elsewhere. Refusing them keeps one
		// canonical spelling rather than two that normalize differently.
		return "", fmt.Errorf("%w: path separators must be /", ErrInvalidPath)
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." {
		return "", nil
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%w: path leaves the worktree", ErrInvalidPath)
	}
	return cleaned, nil
}

// Denylist decides what the files and diff channels refuse. It is applied to
// the canonical path of what was opened, never to the request string; the diff
// applies it to both path and old_path by string, because a deleted or renamed
// file has no current canonical target.
//
// The fixed half of the list — .git/objects, .git/config, node_modules and the
// secret patterns — is tracker.DiffPathDenied, which the stored attempt diff
// already filters on. Delegating rather than restating it is what keeps the
// live surface and the stored diff from drifting apart: a later reader must
// not be shown what the live reader was not, and the only way to guarantee
// that is for both to ask the same function.
type Denylist struct {
	// Extra is workspaces.files.deny: additional globs, matched against the
	// whole relative path and each path segment. It is the configurable half
	// tracker.DiffPathDenied has no way to read.
	Extra []string
}

// NewDenylist builds a denylist from the configured extra globs. Invalid
// patterns are refused here rather than silently never matching, because a
// pattern that never matches is a hole in the filter.
func NewDenylist(extra []string) (Denylist, error) {
	cleaned := make([]string, 0, len(extra))
	for _, pattern := range extra {
		trimmed := strings.TrimSpace(pattern)
		if trimmed == "" {
			continue
		}
		if _, err := path.Match(trimmed, "probe"); err != nil {
			return Denylist{}, fmt.Errorf("workspacesession: deny pattern %q is invalid: %w", pattern, err)
		}
		cleaned = append(cleaned, trimmed)
	}
	slices.Sort(cleaned)
	return Denylist{Extra: slices.Compact(cleaned)}, nil
}

// Denied reports whether a worktree-relative path is refused. The path is
// expected to be clean and relative; an empty path is the worktree root and is
// never denied.
func (d Denylist) Denied(relative string) bool {
	if relative == "" {
		return false
	}
	cleaned := path.Clean(strings.TrimPrefix(relative, "./"))
	if cleaned == "." {
		return false
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		// A path that leaves the worktree never reaches a read, but treating
		// it as denied here means no caller can turn a normalization mistake
		// into a disclosure.
		return true
	}
	if tracker.DiffPathDenied(cleaned) {
		return true
	}
	segments := strings.Split(cleaned, "/")
	for _, pattern := range d.Extra {
		if matched, err := path.Match(pattern, cleaned); err == nil && matched {
			return true
		}
		// A configured glob denies every segment it names, so that "secrets"
		// in the configuration is not defeated by "secrets/key".
		for _, segment := range segments {
			if matched, err := path.Match(pattern, segment); err == nil && matched {
				return true
			}
		}
	}
	return false
}

// DeniedDiffPath applies the same filter to a diff's path and old_path. A file
// whose either side is denied appears with denied: true, its patch empty and
// its counts intact, whatever its status (section 18.5).
func (d Denylist) DeniedDiffPath(newPath, oldPath string) bool {
	return d.Denied(newPath) || (oldPath != "" && d.Denied(oldPath))
}
