package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	ValidationCommand string
	SourceIssues      []tracker.ExternalReference
	GitHubClient      GitHubRESTClient
	External          *tracker.ChangeExternalReference
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
	MergeSHA            string
	BaseRef             string
	BaseBefore          string
	Method              string
	AttemptBranchPushed bool
}

// Landing refusal kinds, each a reason a person acts on.
const (
	LandRefusalHeadMoved   = "head_moved"
	LandRefusalMissingHead = "missing_head"
	LandRefusalConflict    = "conflict"
	LandRefusalNothing     = "nothing_to_land"
	LandRefusalProtected   = "base_protected"
	LandRefusalBaseMoved   = "base_moved"
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
		if holder, held, err := l.branchWorktreePath(ctx, info.Branch, info.Path); err != nil {
			return info, false, err
		} else if held {
			return info, false, &BranchHeldError{Branch: info.Branch, Path: holder}
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
	if err := l.addWorktreeWithPrune(ctx, func() error {
		_, err := l.runGit(ctx, args...)
		return err
	}); err != nil {
		return info, false, &worktreeCreationError{err: err}
	}
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
	l.removeLandingWorktree(ctx, normalized.Path, staging)
	if _, err := runGitAt(ctx, normalized.Path, "worktree", "add", "--detach", staging, targetHead); err != nil {
		return LandResult{}, fmt.Errorf("add landing worktree: %w", err)
	}
	defer l.removeLandingWorktree(context.WithoutCancel(ctx), normalized.Path, staging)

	mergeSHA, err := combine(ctx, staging, method, head, targetHead, opts.Message)
	if err != nil {
		return LandResult{}, err
	}
	validationInfo := normalized
	validationInfo.Path = staging
	if err := l.validateLanding(ctx, validationInfo, issue, opts.ValidationCommand, mergeSHA); err != nil {
		return LandResult{}, err
	}
	result := LandResult{MergeSHA: mergeSHA, BaseRef: target, BaseBefore: targetHead, Method: method}
	pushArgs := []string{"push", "--force-with-lease=refs/heads/" + target + ":" + targetHead, remote, mergeSHA + ":refs/heads/" + target}
	if _, err := runGitAt(ctx, staging, pushArgs...); err != nil {
		return LandResult{}, classifyLandingPush(err, target)
	}
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

// removeLandingWorktree drops the detached staging worktree a landing used.
// A leftover is reported, not fatal: the next landing clears it again.
func (l *LocalGit) removeLandingWorktree(ctx context.Context, workspacePath, staging string) {
	if _, err := runGitAt(ctx, workspacePath, "worktree", "remove", "--force", staging); err != nil && l.logger != nil {
		l.logger.Debug("landing worktree not removed by git", "path", staging, "error", err)
	}
	if err := os.RemoveAll(staging); err != nil && l.logger != nil {
		l.logger.Warn("landing worktree left behind", "path", staging, "error", err)
	}
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

func (l *LocalGit) validateLanding(ctx context.Context, info Info, issue Issue, command, head string) error {
	if err := l.validateMergeResolution(ctx, info, issue, command); err != nil {
		return err
	}
	current, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if strings.TrimSpace(current) != head {
		return refuse(LandRefusalHeadMoved, "validation changed the reviewed landing head")
	}
	if _, err := runGitAt(ctx, info.Path, "diff", "--quiet", "HEAD", "--"); err != nil {
		return refuse(LandRefusalHeadMoved, "validation left tracked source changes")
	}
	return nil
}
