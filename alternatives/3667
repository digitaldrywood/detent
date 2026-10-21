package workspacesession

import (
	"fmt"
	"slices"
	"strings"
)

// The git channel (decisions section 18.13). Three frames, all of them about
// the workspace's own worktree: what state it is in, a commit, and a push.
//
// It is the only channel that writes, and every rule that follows from that is
// written down here rather than discovered at the runner. The hub requires
// write on the project for a commit or a push and re-validates it per frame
// the way it already does for every person-originated frame; the runner
// validates its lease immediately before acting and refuses a write on a
// read-only workspace, which is a workspace whose attempt is still running.
//
// Reading and writing are deliberately separated by type rather than by
// channel: a person who may only read still wants the branch name and the
// dirty count the header shows, and a second channel for one frame would mean
// two streams, two capabilities and two places to get the authority wrong.

// Frame types the git channel defines. Anything else on this channel is
// answered with unknown_frame and dropped.
const (
	// Person → runner.
	TypeGitStatus = "status"
	TypeGitCommit = "commit"
	TypeGitPush   = "push"
	// Runner → person. A status answer reuses the request's name, exactly as
	// the files channel's stat does: the direction disambiguates, and section
	// 18.13 names both "status".
	TypeGitCommitted = "committed"
	TypeGitPushed    = "pushed"
)

// GitRequestTypes lists what a person may send on the git channel.
func GitRequestTypes() []string {
	return []string{TypeGitStatus, TypeGitCommit, TypeGitPush}
}

// ValidGitRequest reports whether value names a git request.
func ValidGitRequest(value string) bool { return slices.Contains(GitRequestTypes(), value) }

// GitWriteRequests lists the requests that change the worktree or the remote.
// They are the ones that need write on the project and that a read-only
// workspace refuses.
func GitWriteRequests() []string { return []string{TypeGitCommit, TypeGitPush} }

// GitWriteRequest reports whether a git request writes.
func GitWriteRequest(value string) bool { return slices.Contains(GitWriteRequests(), value) }

// MaxCommitMessageBytes bounds a commit message. It is far past any subject
// and body a person would type and stops a request from becoming a storage
// cost on the runner's command line.
const MaxCommitMessageBytes = 8 << 10

// GitRequest is the body of a status, commit or push frame. Only the fields
// that frame defines are read, so one struct serves the whole person side of
// the channel, as FilesRequest does for files.
type GitRequest struct {
	// Message is the commit message. It is required on a commit and ignored
	// on the other two.
	Message string `json:"message,omitempty"`
}

// ValidateGitRequest applies the shape rules for one git frame before the
// runner touches the worktree. A commit with no message is refused here rather
// than handed to git, which would answer with its own editor advice.
func ValidateGitRequest(requestType string, request GitRequest) error {
	if requestType != TypeGitCommit {
		return nil
	}
	message := strings.TrimSpace(request.Message)
	if message == "" {
		return fmt.Errorf("%w: a commit message is required", ErrInvalidFrame)
	}
	if len(request.Message) > MaxCommitMessageBytes {
		return fmt.Errorf("%w: the commit message exceeds %d bytes", ErrInvalidFrame, MaxCommitMessageBytes)
	}
	return nil
}

// GitStatus answers a status frame: the branch, how far it is from its
// upstream, and how many files are dirty. It is what the header's git action
// group enables from, so it carries no file list: naming the files would make
// a header control a second Files surface, and section 18.5 already has one.
type GitStatus struct {
	Branch string `json:"branch"`
	// Detached reports a HEAD on no branch. A commit is still possible there
	// and a push is not, so the two are not one flag.
	Detached bool `json:"detached"`
	// Remote is the remote the branch pushes to, empty when it has none.
	Remote string `json:"remote,omitempty"`
	// Upstream reports whether the branch has a tracking ref. Without one
	// Ahead and Behind are zero because they are unknown, not because the
	// branch is in step, so a reader must not be shown "0 ahead".
	Upstream bool   `json:"upstream"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	Dirty    int    `json:"dirty_file_count"`
	HeadSHA  string `json:"head_sha,omitempty"`
}

// GitCommitted answers a commit frame.
type GitCommitted struct {
	Commit string `json:"commit"`
	Branch string `json:"branch"`
	// Files is how many paths the commit carries.
	Files int `json:"files"`
	// Excluded names the dirty paths the denylist kept out of the commit
	// (section 18.4, applied here as well). They are reported rather than
	// silently dropped: a person who expected a file in the commit has to be
	// able to see why it is not.
	Excluded []string `json:"excluded,omitempty"`
}

// GitPushed answers a push frame.
type GitPushed struct {
	Branch string `json:"branch"`
	Remote string `json:"remote"`
	Commit string `json:"commit"`
}
