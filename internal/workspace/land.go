package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

// Lander lands a reviewed head on the repository's base branch with plain
// git: fetch the base, verify the head, combine the two per the merge
// method, push. Nothing here talks to a forge API; a base branch a forge
// protects refuses the push, and that refusal is reported, never worked
// around.
type Lander interface {
	LandChange(context.Context, Info, Issue, LandOptions) (LandResult, error)
}

type LandingWorkspaceCleaner interface {
	CleanupLanding(context.Context, Info, Issue) error
}

func (l *LocalGit) CleanupLanding(ctx context.Context, info Info, issue Issue) error {
	if issue.Landing == nil {
		return nil
	}
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return err
	}
	expected, err := l.workspacePathForIssue(issue, normalized.Key)
	if err != nil || normalized.Path != expected {
		return errors.Join(err, fmt.Errorf("landing cleanup path differs from the prepared workspace: %s", normalized.Path))
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := preserveLandingRecord(ctx, normalized.Path); err != nil {
		return err
	}
	stagingErr := l.removeLandingWorktree(ctx, l.sourceRoot, filepath.Join(l.root, "landing-"+normalized.Key))
	if err := l.removeLandingWorktree(ctx, l.sourceRoot, normalized.Path); err != nil {
		return errors.Join(stagingErr, err)
	}
	return errors.Join(stagingErr, l.removeOwnershipRecord(normalized.Path))
}

type GitHubPRLander interface {
	LandChangeViaGitHub(context.Context, Info, Issue, LandOptions) (LandResult, error)
}

type IntegrationVerifier interface {
	VerifyIntegratedChange(context.Context, Info, Issue, LandOptions, string, string) (LandResult, error)
}

type GitHubRESTClient interface {
	REST(context.Context, string, string, any, any) error
	GraphQL(context.Context, string, map[string]any, any) error
}

type LandOptions struct {
	Native                bool
	RebaseRequired        bool
	Validation            *gate.CommandResult
	LandingMode           string
	Authorize             func(context.Context) error                                    `json:"-"`
	PublicationEffect     func(context.Context, string, string, GitHubPublication) error `json:"-"`
	ValidationCommand     string
	RequiredStatusChecks  []string
	CITriggerLabel        string
	CITriggerLabelStagger time.Duration
	PreviousCI            *tracker.NativeLandingCIReceipt
	SourceIssues          []tracker.ExternalReference
	GitHubClient          GitHubRESTClient
	External              *tracker.ChangeExternalReference
	// HeadSHA is the reviewed commit. It must be the worktree branch's head:
	// a branch that moved past its review is not landed.
	HeadSHA string
	// Method is squash, merge or rebase, from the approved policy.
	Method string
	// Message is the squash or merge commit's message.
	Message string
	// TargetBranch is the base branch; empty means the remote's default.
	TargetBranch string
	Remote       string
	Repository   string
	// PushAttemptBranch also publishes the reviewed head under the worktree
	// branch's name, so the landed history stays reachable by that name. It
	// is best effort: a remote that refuses it does not fail the landing.
	PushAttemptBranch bool
}

type LandResult struct {
	Pipeline            []gate.PipelineTiming           `json:"pipeline,omitempty"`
	PipelineDropped     int                             `json:"pipeline_dropped,omitempty"`
	Path                string                          `json:"path,omitempty"`
	Packages            []string                        `json:"packages,omitempty"`
	CI                  *tracker.NativeLandingCIReceipt `json:"ci,omitempty"`
	Gate                gate.CommandResult              `json:"gate,omitzero"`
	Rebased             bool                            `json:"rebased,omitempty"`
	MergeSHA            string
	BaseRef             string
	BaseBefore          string
	Method              string
	AttemptBranchPushed bool
}

// Landing refusal kinds, each a reason a person acts on.
const (
	LandRefusalHeadMoved     = "head_moved"
	LandRefusalMissingHead   = "missing_head"
	LandRefusalConflict      = "conflict"
	LandRefusalNothing       = "nothing_to_land"
	LandRefusalProtected     = "base_protected"
	LandRefusalBaseMoved     = "base_moved"
	LandRefusalReviewThreads = "review_threads"
)

// LandRefusal is a landing the repository or its history did not allow. It is
// not an infrastructure failure: retrying the same landing gives the same
// answer until something changes, so the reason is reported on the change.
type LandRefusal struct {
	Kind    string
	Reason  string
	BaseSHA string
}

func (r *LandRefusal) Error() string {
	return r.Kind + ": " + r.Reason
}

func refuse(kind, reason string) error {
	return &LandRefusal{Kind: kind, Reason: reason}
}

func (l *LocalGit) createLandingWorktree(ctx context.Context, info Info, issue Issue) (Info, bool, error) {
	l.createMu.Lock()
	defer l.createMu.Unlock()
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return info, false, err
	}
	defer release()
	opts := *issue.Landing
	if opts.Repository == "" || RepositoryURL(ctx, l.sourceRoot) != opts.Repository {
		return info, false, refuse(LandRefusalProtected, "the reviewed repository differs from the runner's authorized source")
	}
	if !validLandingHead(opts.HeadSHA) {
		return info, false, refuse(LandRefusalMissingHead, "landing requires an immutable commit identity")
	}
	if err := ensurePrivateDirectories(l.root, filepath.Join(l.root, ".detent", "landing", opts.HeadSHA)); err != nil {
		return info, false, err
	}
	if opts.External != nil {
		base := opts.TargetBranch
		if base == "" {
			base, err = remoteDefaultBranch(ctx, l.sourceRoot, defaultGitRemote)
			if err != nil {
				return info, false, err
			}
		}
		pull, err := readExternalLandingPull(ctx, opts.GitHubClient, opts.Repository, opts.External, opts.HeadSHA, base)
		if err != nil {
			return info, false, err
		}
		info.ReviewBranch = pull.Head.Ref
		if _, err := l.runGit(ctx, "check-ref-format", "--branch", info.ReviewBranch); err != nil || strings.HasPrefix(info.ReviewBranch, "-") {
			return info, false, refuse(LandRefusalProtected, "the external pull request branch is invalid")
		}
		issue.PullRequestNumber = pull.Number
		issue.PullRequestHeadSHA = opts.HeadSHA
		issue.PullRequestBranch = info.ReviewBranch
	}
	exists, isDir, err := pathExists(info.Path)
	if err != nil {
		return info, false, err
	}
	if exists {
		if !isDir || !l.isSourceWorktree(ctx, info.Path) {
			return info, false, refuse(LandRefusalProtected, "the landing workspace is not owned by the authorized source")
		}
		if err := l.VerifyReviewTree(ctx, info, issue); err != nil {
			return info, false, refuse(LandRefusalHeadMoved, err.Error())
		}
		matches, _, err := l.workspaceOnExpectedBranch(ctx, info.Path, info.Branch)
		if err != nil {
			return info, false, err
		}
		head, err := l.Head(ctx, info, issue)
		if err != nil {
			return info, false, err
		}
		if !matches || strings.TrimSpace(head) != opts.HeadSHA {
			return info, false, refuse(LandRefusalHeadMoved, "the existing landing workspace differs from the reviewed branch or head")
		}
		return info, false, nil
	}
	if info.Branch != "" {
		if err := l.releaseBranchHolders(ctx, info.Branch, info.Path); err != nil {
			return info, false, err
		}
	}
	if opts.External != nil {
		if _, err := fetchReviewHead(ctx, l.sourceRoot, info.ReviewBranch, issue); err != nil {
			return info, false, err
		}
	} else if _, err := l.runGit(ctx, "cat-file", "-e", opts.HeadSHA+"^{commit}"); err != nil {
		if _, err := l.runGit(ctx, "fetch", "--no-tags", "--no-write-fetch-head", defaultGitRemote, opts.HeadSHA); err != nil {
			return info, false, fmt.Errorf("hydrate reviewed landing head: %w", err)
		}
	}
	if _, err := l.runGit(ctx, "cat-file", "-e", opts.HeadSHA+"^{commit}"); err != nil {
		return info, false, refuse(LandRefusalMissingHead, "the reviewed commit is unavailable in the authorized source")
	}
	args := []string{"worktree", "add"}
	if info.Branch == "" {
		args = append(args, "--detach", info.Path, opts.HeadSHA)
	} else {
		exists, err := l.branchExists(ctx, info.Branch)
		if err != nil {
			return info, false, err
		}
		if exists {
			head, err := l.runGit(ctx, "rev-parse", "refs/heads/"+info.Branch)
			if err != nil {
				return info, false, err
			}
			if strings.TrimSpace(head) != opts.HeadSHA {
				return info, false, refuse(LandRefusalHeadMoved, "the local landing branch differs from the reviewed head")
			}
			args = append(args, info.Path, info.Branch)
		} else {
			args = append(args, "-b", info.Branch, info.Path, opts.HeadSHA)
		}
	}
	prepared := false
	defer func() {
		if !prepared {
			if err := l.removeLandingWorktree(context.WithoutCancel(ctx), l.sourceRoot, info.Path); err != nil {
				l.logger.Warn("landing worktree left behind", "path", info.Path, "error", err)
			}
		}
	}()
	if err := l.addWorktreeWithPrune(ctx, func() error {
		_, err := l.runGit(ctx, args...)
		return err
	}); err != nil {
		return info, false, &worktreeCreationError{err: err}
	}
	prepared = true
	return info, true, nil
}

func validLandingHead(head string) bool {
	if len(head) != 40 && len(head) != 64 {
		return false
	}
	for _, character := range head {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func (l *LocalGit) verifyLandingWorktree(ctx context.Context, info Info, issue Issue, opts LandOptions) error {
	if err := l.validateCreatedWorktree(ctx, info.Path); err != nil {
		return refuse(LandRefusalProtected, "the landing workspace is not owned by the authorized source")
	}
	if opts.Repository != "" && (RepositoryURL(ctx, l.sourceRoot) != opts.Repository || RepositoryURL(ctx, info.Path) != opts.Repository) {
		return refuse(LandRefusalProtected, "the landing source differs from the reviewed repository")
	}
	if err := l.VerifyReviewTree(ctx, info, issue); err != nil {
		return refuse(LandRefusalHeadMoved, err.Error())
	}
	matches, _, err := l.workspaceOnExpectedBranch(ctx, info.Path, info.Branch)
	if err != nil {
		return err
	}
	if !matches {
		return refuse(LandRefusalHeadMoved, "the landing workspace changed branches after preparation")
	}
	return nil
}

func (l *LocalGit) LandChange(ctx context.Context, info Info, issue Issue, opts LandOptions) (LandResult, error) {
	if opts.Native {
		outcome := l.LandChanges(ctx, []LandRequest{{Info: info, Issue: issue, Options: opts}})[0]
		return outcome.Result, outcome.Err
	}
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return LandResult{}, err
	}
	head := strings.TrimSpace(opts.HeadSHA)
	if head == "" {
		return LandResult{}, errors.New("landing requires the reviewed head")
	}
	method := strings.TrimSpace(opts.Method)
	if method == "" {
		method = "squash"
	}
	if method != "squash" && method != "merge" && method != "rebase" {
		return LandResult{}, fmt.Errorf("unsupported merge method %q", method)
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return LandResult{}, fmt.Errorf("wait for source repository operation: %w", err)
	}
	defer release()
	if opts.Authorize != nil {
		if err := opts.Authorize(ctx); err != nil {
			return LandResult{}, err
		}
	}
	if err := l.verifyLandingWorktree(ctx, normalized, issue, opts); err != nil {
		return LandResult{}, err
	}

	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultGitRemote
	}
	target := strings.TrimSpace(opts.TargetBranch)
	if target == "" {
		if target, err = remoteDefaultBranch(ctx, normalized.Path, remote); err != nil {
			return LandResult{}, fmt.Errorf("resolve remote default branch: %w", err)
		}
	}
	localHead, err := runGitAt(ctx, normalized.Path, "rev-parse", "HEAD")
	if err != nil {
		return LandResult{}, fmt.Errorf("inspect worktree head: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(localHead), head) {
		if _, err := runGitAt(ctx, normalized.Path, "cat-file", "-e", head+"^{commit}"); err != nil {
			return LandResult{}, refuse(LandRefusalMissingHead, "the reviewed head "+head+" is not in the runner's checkout; the run that produced it is gone")
		}
		return LandResult{}, refuse(LandRefusalHeadMoved, "the worktree moved to "+strings.TrimSpace(localHead)+" after "+head+" was reviewed; review the current head")
	}
	targetRef := "refs/remotes/" + remote + "/" + target
	if _, err := runGitAt(ctx, normalized.Path, "fetch", remote, "+refs/heads/"+target+":"+targetRef); err != nil {
		return LandResult{}, fmt.Errorf("git fetch %s %s: %w", remote, target, err)
	}
	targetHead, err := runGitAt(ctx, normalized.Path, "rev-parse", targetRef)
	if err != nil {
		return LandResult{}, fmt.Errorf("inspect fetched base: %w", err)
	}
	targetHead = strings.TrimSpace(targetHead)
	if kept, ok := keptLanding(ctx, normalized.Path, head, targetRef); ok {
		return kept, nil
	}

	staging := filepath.Join(l.root, "landing-"+normalized.Key)
	defer func() {
		if err := l.removeLandingWorktree(context.WithoutCancel(ctx), l.sourceRoot, staging); err != nil {
			l.logger.Warn("landing worktree left behind", "path", staging, "error", err)
		}
	}()
	if err := l.removeLandingWorktree(context.WithoutCancel(ctx), l.sourceRoot, staging); err != nil {
		return LandResult{}, err
	}
	if _, err := runGitAt(ctx, normalized.Path, "worktree", "add", "--detach", staging, targetHead); err != nil {
		return LandResult{}, fmt.Errorf("add landing worktree: %w", err)
	}

	mergeSHA, err := combine(ctx, staging, method, head, targetHead, opts.Message)
	rebased := false
	var refusal *LandRefusal
	if errors.As(err, &refusal) && refusal.Kind == LandRefusalConflict {
		if _, err := runGitAt(ctx, normalized.Path, "fetch", remote, "+refs/heads/"+target+":"+targetRef); err != nil {
			return LandResult{}, fmt.Errorf("refresh conflicted landing base: %w", err)
		}
		currentBase, refreshErr := runGitAt(ctx, normalized.Path, "rev-parse", targetRef)
		if refreshErr != nil {
			return LandResult{}, refreshErr
		}
		targetHead = strings.TrimSpace(currentBase)
		if _, err := runGitAt(ctx, staging, "reset", "--hard", targetHead); err != nil {
			return LandResult{}, err
		}
		rebased = true
		mergeSHA, err = combine(ctx, staging, method, head, targetHead, opts.Message)
	}
	if err != nil {
		return LandResult{Rebased: rebased}, err
	}
	var validation gate.CommandResult
	for {
		if gate.NormalizeLandingMode(opts.LandingMode) != gate.LandingRollingBarrier {
			validationInfo := normalized
			validationInfo.Path = staging
			validation, err = l.validateLanding(ctx, validationInfo, issue, opts.ValidationCommand, mergeSHA)
			if err != nil {
				return LandResult{Rebased: rebased, Gate: validation, BaseBefore: targetHead}, err
			}
		}
		if opts.Authorize != nil {
			if err := opts.Authorize(ctx); err != nil {
				return LandResult{}, err
			}
		}
		pushArgs := []string{"push", "--force-with-lease=refs/heads/" + target + ":" + targetHead, remote, mergeSHA + ":refs/heads/" + target}
		if _, err := runGitAt(ctx, staging, pushArgs...); err != nil {
			classified := classifyLandingPush(err, target)
			var refusal *LandRefusal
			if gate.NormalizeLandingMode(opts.LandingMode) != gate.LandingRollingBarrier || !errors.As(classified, &refusal) || refusal.Kind != LandRefusalBaseMoved {
				return LandResult{Rebased: rebased, Gate: validation, BaseBefore: targetHead}, classified
			}
			if _, err := runGitAt(ctx, normalized.Path, "fetch", remote, "+refs/heads/"+target+":"+targetRef); err != nil {
				return LandResult{}, err
			}
			current, err := runGitAt(ctx, normalized.Path, "rev-parse", targetRef)
			if err != nil {
				return LandResult{}, err
			}
			current = strings.TrimSpace(current)
			if current == targetHead {
				return LandResult{}, classified
			}
			targetHead = current
			if _, err := runGitAt(ctx, staging, "reset", "--hard", targetHead); err != nil {
				return LandResult{}, err
			}
			mergeSHA, err = combine(ctx, staging, method, head, targetHead, opts.Message)
			if err != nil {
				return LandResult{Rebased: true}, err
			}
			rebased = true
			continue
		}
		break
	}
	result := LandResult{Gate: validation, MergeSHA: mergeSHA, BaseRef: target, BaseBefore: targetHead, Method: method, Rebased: rebased}
	if opts.PushAttemptBranch && strings.TrimSpace(normalized.Branch) != "" {
		if _, err := runGitAt(ctx, normalized.Path, "push", remote, head+":refs/heads/"+normalized.Branch); err == nil {
			result.AttemptBranchPushed = true
		} else if l.logger != nil {
			l.logger.Warn("landed head not published under its branch name", "branch", normalized.Branch, "error", err)
		}
	}
	return result, nil
}

func (l *LocalGit) VerifyIntegratedChange(ctx context.Context, info Info, issue Issue, opts LandOptions, sourceBase, integratedBase string) (LandResult, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return LandResult{}, err
	}
	if opts.Repository == "" {
		return LandResult{}, refuse(LandRefusalProtected, "integration requires the published repository identity")
	}
	if !validLandingHead(sourceBase) || !validLandingHead(opts.HeadSHA) || !validLandingHead(integratedBase) || sourceBase == opts.HeadSHA {
		return LandResult{}, refuse(LandRefusalNothing, "integration requires the published source delta and final base")
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return LandResult{}, err
	}
	defer release()
	if err := l.verifyLandingWorktree(ctx, normalized, issue, opts); err != nil {
		return LandResult{}, err
	}
	head, err := runGitAt(ctx, normalized.Path, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != integratedBase {
		return LandResult{}, refuse(LandRefusalHeadMoved, "the final checkpoint differs from the integration base")
	}
	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultGitRemote
	}
	target := strings.TrimSpace(opts.TargetBranch)
	if target == "" {
		target, err = remoteDefaultBranch(ctx, normalized.Path, remote)
		if err != nil {
			return LandResult{}, err
		}
	}
	base, exists, err := remoteBranchHead(ctx, normalized.Path, remote, target)
	if err != nil {
		return LandResult{}, err
	}
	if !exists {
		return LandResult{}, refuse(LandRefusalMissingHead, "the integration branch is unavailable")
	}
	if _, err := runGitAt(ctx, normalized.Path, "fetch", "--no-tags", "--no-write-fetch-head", remote, base); err != nil {
		return LandResult{}, err
	}
	for _, ancestry := range [][2]string{{integratedBase, base}, {sourceBase, integratedBase}, {sourceBase, opts.HeadSHA}} {
		if _, err := runGitAt(ctx, normalized.Path, "merge-base", "--is-ancestor", ancestry[0], ancestry[1]); err != nil {
			return LandResult{}, refuse(LandRefusalHeadMoved, "the published source and final base do not identify the integration branch")
		}
	}
	delta, err := runGitAt(ctx, normalized.Path, "diff", "--no-ext-diff", "--no-textconv", "--name-only", sourceBase, opts.HeadSHA)
	if err != nil {
		return LandResult{}, err
	}
	if strings.TrimSpace(delta) == "" {
		return LandResult{}, refuse(LandRefusalNothing, "the published source has no deliverable to verify")
	}
	merged, err := runGitAt(ctx, normalized.Path, "merge-tree", "--write-tree", "--merge-base="+sourceBase, base, opts.HeadSHA)
	if err != nil {
		return LandResult{}, refuse(LandRefusalConflict, "the published source delta is not proven incorporated on the current integration base")
	}
	tree, err := runGitAt(ctx, normalized.Path, "rev-parse", base+"^{tree}")
	if err != nil {
		return LandResult{}, err
	}
	if strings.TrimSpace(merged) != strings.TrimSpace(tree) {
		return LandResult{}, refuse(LandRefusalNothing, "the current integration base does not incorporate the published source delta")
	}
	return LandResult{MergeSHA: base, BaseRef: target, BaseBefore: base, Method: opts.Method}, nil
}

func (l *LocalGit) removeLandingWorktree(ctx context.Context, workspacePath, staging string) error {
	registered, err := l.landingWorktreeRegistered(ctx, staging)
	if err != nil {
		return err
	}
	if !registered {
		return removeWorkspacePath(l.root, staging)
	}
	if l.isGitWorkspace(ctx, staging) && !l.isSourceWorktree(ctx, staging) {
		return fmt.Errorf("refusing to remove landing workspace not managed by source: %s", staging)
	}
	if _, err := runGitAt(ctx, workspacePath, "worktree", "remove", "--force", staging); err != nil {
		l.logger.Warn("landing worktree not removed by git", "path", staging, "error", err)
		if cleanupErr := removeWorkspacePath(l.root, staging); cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
		if _, pruneErr := l.runGit(ctx, "worktree", "prune", "--expire", "now"); pruneErr != nil {
			return errors.Join(err, pruneErr)
		}
	}
	registered, err = l.landingWorktreeRegistered(ctx, staging)
	if err != nil {
		return err
	}
	if registered {
		return fmt.Errorf("landing worktree registration remains: %s", staging)
	}
	return removeWorkspacePath(l.root, staging)
}

func (l *LocalGit) landingWorktreeRegistered(ctx context.Context, path string) (bool, error) {
	output, err := l.runGit(ctx, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	for _, entry := range strings.Split(output, "\x00\x00") {
		found, locked := false, false
		for _, field := range strings.Split(entry, "\x00") {
			if listed, ok := strings.CutPrefix(field, "worktree "); ok && filepath.Clean(listed) == filepath.Clean(path) {
				found = true
			}
			locked = locked || field == "locked" || strings.HasPrefix(field, "locked ")
		}
		if found {
			if locked {
				return true, fmt.Errorf("landing worktree is locked: %s", path)
			}
			return true, nil
		}
	}
	return false, nil
}

// combine produces the commit the base branch advances to: a squash commit,
// a merge commit, or the head's commits replayed onto the base. It runs in
// the detached landing worktree, so a conflict leaves the source and the
// attempt worktree untouched.
func combine(ctx context.Context, staging, method, head, targetHead, message string) (string, error) {
	if strings.TrimSpace(message) == "" {
		message = "Land " + head
	}
	switch method {
	case "squash":
		if _, err := runGitAt(ctx, staging, "merge", "--squash", head); err != nil {
			return "", abandon(refuse(LandRefusalConflict, "squashing "+head+" onto the base conflicts: "+commandErrorOutput(err)), gitErr(ctx, staging, "reset", "--merge"))
		}
		status, err := runGitAt(ctx, staging, "status", "--porcelain")
		if err != nil {
			return "", fmt.Errorf("inspect squash result: %w", err)
		}
		if strings.TrimSpace(status) == "" {
			return "", refuse(LandRefusalNothing, "the base branch already contains everything in "+head)
		}
		if _, err := runGitAt(ctx, staging, "commit", "--no-verify", "-m", message); err != nil {
			return "", fmt.Errorf("commit squash: %w", err)
		}
	case "merge":
		if strings.TrimSpace(mustOutput(runGitAt(ctx, staging, "merge-base", "--is-ancestor", head, targetHead))) == "ancestor" {
			return "", refuse(LandRefusalNothing, "the base branch already contains "+head)
		}
		if _, err := runGitAt(ctx, staging, "merge", "--no-ff", "--no-verify", "-m", message, head); err != nil {
			return "", abandon(refuse(LandRefusalConflict, "merging "+head+" into the base conflicts: "+commandErrorOutput(err)), gitErr(ctx, staging, "merge", "--abort"))
		}
	case "rebase":
		mergeBase, err := runGitAt(ctx, staging, "merge-base", targetHead, head)
		if err != nil {
			return "", fmt.Errorf("inspect merge base: %w", err)
		}
		if strings.TrimSpace(mergeBase) == head {
			return "", refuse(LandRefusalNothing, "the base branch already contains "+head)
		}
		if _, err := runGitAt(ctx, staging, "cherry-pick", strings.TrimSpace(mergeBase)+".."+head); err != nil {
			return "", abandon(refuse(LandRefusalConflict, "replaying "+head+" onto the base conflicts: "+commandErrorOutput(err)), gitErr(ctx, staging, "cherry-pick", "--abort"))
		}
	}
	sha, err := runGitAt(ctx, staging, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("inspect landed commit: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// abandon returns the refusal for a combine that conflicted, joined with any
// failure of the abort that put the staging worktree back; the staging
// worktree is removed afterwards either way.
func abandon(refusal error, abortErr error) error {
	if abortErr != nil {
		return errors.Join(refusal, fmt.Errorf("abort after conflict: %w", abortErr))
	}
	return refusal
}

// gitErr runs a git command for its outcome alone.
func gitErr(ctx context.Context, dir string, args ...string) error {
	_, err := runGitAt(ctx, dir, args...)
	return err
}

// mustOutput folds git's ancestor check into a word: "ancestor" when the
// command succeeded, "" for exit 1 (not an ancestor) or any failure.
func mustOutput(_ string, err error) string {
	if err == nil {
		return "ancestor"
	}
	return ""
}

// classifyLandingPush turns a refused push into the reason a person acts
// on. A forge that requires pull requests, a protected branch, or a hook
// that declines is a policy the operator changes; a base that moved is
// retried by the next landing run.
func classifyLandingPush(err error, target string) error {
	output := strings.ToLower(commandErrorOutput(err))
	switch {
	case strings.Contains(output, "protected branch"),
		strings.Contains(output, "gh006"),
		strings.Contains(output, "gh013"),
		strings.Contains(output, "pre-receive hook declined"),
		strings.Contains(output, "pull request"),
		strings.Contains(output, "required status check"),
		strings.Contains(output, "not allowed to push"),
		strings.Contains(output, "permission denied"),
		strings.Contains(output, "refusing to allow"):
		return refuse(LandRefusalProtected, "the base branch "+target+" refused the push: "+strings.TrimSpace(commandErrorOutput(err))+". Allow the runner to push to "+target+", or enable GitHub pull request mode for this project.")
	case strings.Contains(output, "fetch first"),
		strings.Contains(output, "non-fast-forward"),
		strings.Contains(output, "stale info"),
		strings.Contains(output, "rejected"):
		return refuse(LandRefusalBaseMoved, "the base branch "+target+" moved while the change was being landed: "+strings.TrimSpace(commandErrorOutput(err)))
	}
	return fmt.Errorf("git push %s: %w", target, err)
}

// A landing that reached the base branch but whose report to the hub failed
// is kept beside the attempt worktree's git metadata, so the next landing
// run for the same head reports it instead of finding "nothing to land" and
// sending a landed change back to review. The record is the LandResult plus
// the head it landed; it is read only when that head is asked for again and
// its merge commit is still on the base branch.
const landingRecordFile = "detent-landing.json"

type landingRecord struct {
	HeadSHA string     `json:"head_sha"`
	Result  LandResult `json:"result"`
}

func landingRecordPath(ctx context.Context, workspacePath string) (string, error) {
	if isLandingWorkspacePath(workspacePath) {
		dir, err := gitCommonDir(ctx, workspacePath)
		if err != nil {
			return "", err
		}
		digest := sha256.Sum256([]byte(filepath.Clean(workspacePath)))
		return filepath.Join(dir, fmt.Sprintf("detent-landing-%x.json", digest)), nil
	}
	dir, err := runGitAt(ctx, workspacePath, "rev-parse", "--git-dir")
	if err != nil {
		return "", err
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(workspacePath, dir)
	}
	return filepath.Join(dir, landingRecordFile), nil
}

func isLandingWorkspacePath(path string) bool {
	head := filepath.Dir(path)
	parent := filepath.Dir(head)
	return validLandingHead(filepath.Base(head)) && filepath.Base(parent) == "landing" && filepath.Base(filepath.Dir(parent)) == ".detent"
}

func preserveLandingRecord(ctx context.Context, path string) error {
	if !isLandingWorkspacePath(path) {
		return nil
	}
	exists, _, err := pathExists(path)
	if err != nil || !exists {
		return err
	}
	dir, err := runGitAt(ctx, path, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	legacy := filepath.Join(strings.TrimSpace(dir), landingRecordFile)
	_, err = os.Stat(legacy)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	destination, err := landingRecordPath(ctx, path)
	if err != nil {
		return err
	}
	return os.Rename(legacy, destination)
}

// RecordLanding keeps a landing that reached the base branch, for a report
// that could not be delivered yet.
func RecordLanding(ctx context.Context, info Info, head string, result LandResult) error {
	path, err := landingRecordPath(ctx, info.Path)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(landingRecord{HeadSHA: head, Result: result})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

// ForgetLanding removes a kept landing once the hub has it.
func ForgetLanding(ctx context.Context, info Info) error {
	path, err := landingRecordPath(ctx, info.Path)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// keptLanding returns the kept landing for head when its merge commit is
// still reachable from the fetched base; otherwise nothing, and a stale
// record is forgotten.
func keptLanding(ctx context.Context, workspacePath, head, targetRef string) (LandResult, bool) {
	if err := preserveLandingRecord(ctx, workspacePath); err != nil {
		return LandResult{}, false
	}
	path, err := landingRecordPath(ctx, workspacePath)
	if err != nil {
		return LandResult{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return LandResult{}, false
	}
	var record landingRecord
	if err := json.Unmarshal(raw, &record); err != nil || !strings.EqualFold(record.HeadSHA, head) || record.Result.MergeSHA == "" {
		return LandResult{}, forgetStale(path)
	}
	if _, err := runGitAt(ctx, workspacePath, "merge-base", "--is-ancestor", record.Result.MergeSHA, targetRef); err != nil {
		return LandResult{}, forgetStale(path)
	}
	return record.Result, true
}

// forgetStale removes a kept landing that no longer applies and reports
// that nothing was kept; a record that cannot be removed is still ignored.
func forgetStale(path string) bool {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	return false
}

func (l *LocalGit) validateLanding(ctx context.Context, info Info, issue Issue, command, head string) (gate.CommandResult, error) {
	if strings.TrimSpace(command) == "" {
		return gate.CommandResult{}, nil
	}
	validationIssue := issue
	validationIssue.PullRequestHeadSHA = head
	result, err := l.RunReviewCommand(ctx, info, validationIssue, command)
	if err != nil {
		if result.Command != "" && ctx.Err() == nil {
			current, headErr := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
			if headErr != nil {
				return result, errors.Join(err, headErr)
			}
			if strings.TrimSpace(current) != head {
				return result, refuse(LandRefusalHeadMoved, "validation changed the reviewed landing head")
			}
			if treeErr := l.VerifyReviewTree(ctx, info, validationIssue); treeErr != nil {
				return result, refuse(LandRefusalHeadMoved, "validation left source changes: "+treeErr.Error())
			}
		}
		return result, err
	}
	if result.ExitCode != 0 {
		return result, newValidationError(ValidationStageLanding, result, fmt.Errorf("exit status %d", result.ExitCode))
	}
	return result, nil
}
