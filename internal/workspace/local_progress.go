package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspace/gitdiff"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

const (
	localProgressTimeout     = 5 * time.Second
	localProgressPatchLimit  = 8 << 20
	worktreeReworkBasePrefix = "refs/worktree/detent-rework-base/"
	worktreeReworkDefaultRef = worktreeReworkBasePrefix + "default"
	worktreeReworkPendingRef = worktreeReworkBasePrefix + "pending"
)

type LocalProgressProvider interface {
	LocalProgress(context.Context, Info, Issue) (LocalProgress, error)
}

type LocalProgress struct {
	HeadSHA            string `json:"head_sha"`
	CommitFingerprint  string `json:"commit_fingerprint"`
	TrackedFingerprint string `json:"tracked_fingerprint"`
}

func (l *LocalGit) LocalProgress(ctx context.Context, info Info, issue Issue) (LocalProgress, error) {
	ctx, cancel := context.WithTimeout(ctx, localProgressTimeout)
	defer cancel()
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return LocalProgress{}, err
	}
	head, err := runGitAt(ctx, normalized.Path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return LocalProgress{}, err
	}
	head = strings.TrimSpace(head)
	base, err := l.localOnlyProgressBase(ctx, normalized.Path, issue)
	if err != nil {
		return LocalProgress{}, err
	}
	ancestor, err := runGitAt(ctx, normalized.Path, "merge-base", base, head)
	if err != nil {
		return LocalProgress{}, err
	}
	committed, err := gitProgressPatch(ctx, normalized.Path, strings.TrimSpace(ancestor), head)
	if err != nil {
		return LocalProgress{}, err
	}
	tracked, err := gitProgressPatch(ctx, normalized.Path, head)
	if err != nil {
		return LocalProgress{}, err
	}
	verifiedHead, err := runGitAt(ctx, normalized.Path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return LocalProgress{}, err
	}
	if strings.TrimSpace(verifiedHead) != head {
		return LocalProgress{}, errors.New("workspace head changed during progress observation")
	}
	return LocalProgress{HeadSHA: head, CommitFingerprint: committed, TrackedFingerprint: tracked}, nil
}

func worktreeReworkBranchRef(branch string) string {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return worktreeReworkDefaultRef
	}
	return worktreeReworkBasePrefix + "refs/heads/" + branch
}

func (l *LocalGit) localOnlyProgressBase(ctx context.Context, path string, issue Issue) (string, error) {
	if issue.ReleaseDelivery != nil {
		base, err := runGitAt(ctx, path, "rev-parse", "--verify", "--end-of-options", issue.ReleaseDelivery.BaseSHA+"^{commit}")
		return strings.TrimSpace(base), err
	}
	recordedRef := worktreeReworkBranchRef(issue.ProgressBaseRef)
	if raw, recErr := runGitAt(ctx, path, "rev-parse", "--verify", "--quiet", recordedRef+"^{commit}"); recErr == nil {
		if recorded := strings.TrimSpace(raw); recorded != "" {
			return recorded, nil
		}
	}
	base, _, err := l.configuredProgressBaseWithRef(ctx, path, issue)
	return base, err
}

func (l *LocalGit) localProgressBase(ctx context.Context, path string, issue Issue) (string, error) {
	base, _, _, _, err := l.localProgressBaseWithSource(ctx, path, issue)
	return base, err
}

func (l *LocalGit) localProgressBaseWithSource(ctx context.Context, path string, issue Issue) (string, []string, []string, bool, error) {
	if issue.ReleaseDelivery != nil {
		base, err := runGitAt(ctx, path, "rev-parse", "--verify", "--end-of-options", issue.ReleaseDelivery.BaseSHA+"^{commit}")
		return strings.TrimSpace(base), nil, nil, false, err
	}
	recordedRef := worktreeReworkBranchRef(issue.ProgressBaseRef)
	base, baseRef, err := l.configuredProgressBaseWithRef(ctx, path, issue)
	liveBase, liveTarget, verified, verifyErr := l.verifiedWorktreeReworkBase(ctx, path, issue, recordedRef)
	if verifyErr != nil {
		return "", nil, nil, false, verifyErr
	}
	if verified {
		var excluded []string
		var extraPublished []string
		if liveTarget != "" {
			var trackErr error
			excluded, extraPublished, trackErr = l.targetRemoteTrackingRefs(ctx, path, issue, defaultGitRemote, liveTarget, strings.TrimSpace(issue.ProgressBaseRef) == "")
			if trackErr != nil {
				return "", nil, nil, false, trackErr
			}
		}
		if err == nil && liveBase != base && baseRef != "" && !slices.Contains(excluded, baseRef) {
			excluded = append(excluded, baseRef)
		}
		return liveBase, excluded, extraPublished, true, nil
	}
	if err != nil {
		return "", nil, nil, false, err
	}
	return base, nil, nil, false, nil
}

func (l *LocalGit) targetRemoteTrackingRefs(ctx context.Context, path string, issue Issue, remote, target string, isDefaultTarget bool) ([]string, []string, error) {
	if remote == "" || target == "" {
		return nil, nil, nil
	}
	refs := []string{"refs/remotes/" + remote + "/" + target}
	var extraPublished []string
	matchesDefault := isDefaultTarget
	defaultBase := ""
	defaultTarget := ""
	if !isDefaultTarget {
		liveDefaultBase, liveDefaultTarget, defErr := l.resolveRemoteHeadTarget(ctx, path, remote, issue)
		if defErr != nil {
			return nil, nil, defErr
		}
		defaultBase = liveDefaultBase
		defaultTarget = liveDefaultTarget
		matchesDefault = defaultTarget == target || defaultBase == ""
		if defaultTarget != "" && defaultTarget != target {
			defaultTracking := "refs/remotes/" + remote + "/" + defaultTarget
			if !slices.Contains(refs, defaultTracking) {
				refs = append(refs, defaultTracking)
			}
		}
		if !matchesDefault && validLandingHead(defaultBase) {
			if _, existsErr := runGitAt(ctx, path, "cat-file", "-e", defaultBase+"^{commit}"); existsErr != nil {
				if fetchErr := gitexec.FetchCheckpoint(ctx, path, remote, defaultBase, "HEAD"); fetchErr != nil {
					return nil, nil, fetchErr
				}
			}
			if _, existsErr := runGitAt(ctx, path, "cat-file", "-e", defaultBase+"^{commit}"); existsErr != nil {
				return nil, nil, existsErr
			}
			extraPublished = append(extraPublished, defaultBase)
		}
	}
	remoteHeadRef := "refs/remotes/" + remote + "/HEAD"
	if matchesDefault {
		if !slices.Contains(refs, remoteHeadRef) {
			refs = append(refs, remoteHeadRef)
		}
	} else if rawHeadRef, headRefErr := runGitAt(ctx, path, "rev-parse", "--verify", "--quiet", remoteHeadRef+"^{commit}"); headRefErr == nil && strings.TrimSpace(rawHeadRef) != defaultBase {
		if !slices.Contains(refs, remoteHeadRef) {
			refs = append(refs, remoteHeadRef)
		}
	}
	raw, err := runGitAt(ctx, path, "config", "--get-all", "remote."+remote+".fetch")
	if err != nil || strings.TrimSpace(raw) == "" {
		return refs, extraPublished, nil
	}
	sourceBranches := []string{target}
	if defaultTarget != "" && defaultTarget != target {
		sourceBranches = append(sourceBranches, defaultTarget)
	}
	for spec := range strings.SplitSeq(raw, "\n") {
		spec = strings.TrimSpace(spec)
		if spec == "" || strings.HasPrefix(spec, "^") {
			continue
		}
		spec = strings.TrimPrefix(spec, "+")
		src, dst, ok := strings.Cut(spec, ":")
		if !ok || !strings.HasPrefix(dst, "refs/remotes/") {
			continue
		}
		if src == "HEAD" {
			var mapped string
			if matchesDefault {
				mapped = dst
			} else if rawDst, dstErr := runGitAt(ctx, path, "rev-parse", "--verify", "--quiet", dst+"^{commit}"); dstErr == nil && strings.TrimSpace(rawDst) != defaultBase {
				mapped = dst
			}
			if mapped != "" && !slices.Contains(refs, mapped) {
				refs = append(refs, mapped)
			}
			continue
		}
		for _, branch := range sourceBranches {
			fullSource := "refs/heads/" + branch
			var mapped string
			if src == fullSource || src == branch {
				mapped = dst
			} else if srcPrefix, srcSuffix, srcWild := strings.Cut(src, "*"); srcWild {
				if dstPrefix, dstSuffix, dstWild := strings.Cut(dst, "*"); dstWild &&
					len(fullSource) >= len(srcPrefix)+len(srcSuffix) &&
					strings.HasPrefix(fullSource, srcPrefix) &&
					strings.HasSuffix(fullSource, srcSuffix) {
					mid := fullSource[len(srcPrefix) : len(fullSource)-len(srcSuffix)]
					mapped = dstPrefix + mid + dstSuffix
				}
			}
			if mapped != "" && !slices.Contains(refs, mapped) {
				refs = append(refs, mapped)
			}
		}
	}
	return refs, extraPublished, nil
}

func (l *LocalGit) configuredProgressBaseWithRef(ctx context.Context, path string, issue Issue) (string, string, error) {
	if branch := strings.TrimSpace(issue.ProgressBaseRef); branch != "" {
		for _, ref := range []string{"refs/remotes/origin/" + branch, "refs/heads/" + branch} {
			if base, err := runGitAt(ctx, path, "rev-parse", "--verify", ref+"^{commit}"); err == nil {
				remoteRef := ""
				if strings.HasPrefix(ref, "refs/remotes/") {
					remoteRef = ref
				}
				return strings.TrimSpace(base), remoteRef, nil
			}
		}
		return "", "", fmt.Errorf("local progress base branch %q is unavailable", branch)
	}
	if ref := strings.TrimSpace(issue.BaseRef); ref != "" {
		base, err := runGitAt(ctx, path, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if err != nil {
			return "", "", err
		}
		remoteRef := ""
		if full, symErr := runGitAt(ctx, path, "rev-parse", "--symbolic-full-name", "--end-of-options", ref); symErr == nil {
			if trimmed := strings.TrimSpace(full); strings.HasPrefix(trimmed, "refs/remotes/") {
				remoteRef = trimmed
			}
		}
		return strings.TrimSpace(base), remoteRef, nil
	}
	if base, err := l.runGit(ctx, "rev-parse", "--verify", "refs/remotes/origin/HEAD^{commit}"); err == nil {
		return strings.TrimSpace(base), "refs/remotes/origin/HEAD", nil
	}
	base, err := l.runGit(ctx, "rev-parse", "--verify", "HEAD")
	return strings.TrimSpace(base), "", err
}

func (l *LocalGit) verifiedWorktreeReworkBase(ctx context.Context, path string, issue Issue, recordedRef string) (string, string, bool, error) {
	raw, err := runGitAt(ctx, path, "rev-parse", "--verify", "--quiet", recordedRef+"^{commit}")
	if err != nil {
		return "", "", false, nil
	}
	recorded := strings.TrimSpace(raw)
	if recorded == "" {
		return "", "", false, nil
	}
	liveBase, liveTarget, liveErr := l.resolveReworkTargetBase(ctx, path, issue, MergePrepareOptions{TargetBranch: issue.ProgressBaseRef})
	if liveErr != nil {
		return "", "", false, liveErr
	}
	return liveBase, strings.TrimSpace(liveTarget), true, nil
}

func gitProgressPatch(ctx context.Context, path string, revisions ...string) (string, error) {
	return gitProgressPatchBounded(ctx, path, localProgressPatchLimit, revisions...)
}

func gitProgressPatchBounded(ctx context.Context, path string, limit int64, revisions ...string) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"-C", path, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--no-relative", "--diff-algorithm=myers", "--binary", "--full-index", "--unified=0", "--src-prefix=a/", "--dst-prefix=b/"}
	args = append(args, revisions...)
	args = gitdiff.DiagnosticArgs(args...)
	diff := exec.CommandContext(ctx, "git")
	diff.Args = append(diff.Args, args...)
	diff.WaitDelay = workspaceCommandWaitDelay
	var diffError bytes.Buffer
	diff.Stderr = &diffError
	patch, err := diff.StdoutPipe()
	if err != nil {
		return "", err
	}
	wait, err := commandshell.StartCommand(ctx, diff)
	if err != nil {
		return "", err
	}
	identity := exec.CommandContext(ctx, "git", "patch-id", "--verbatim")
	identity.Dir = path
	identity.WaitDelay = workspaceCommandWaitDelay
	bounded := &io.LimitedReader{R: patch, N: limit + 1}
	identity.Stdin = bounded
	output, patchErr := commandshell.CommandOutput(ctx, identity, false)
	if bounded.N == 0 {
		patchErr = errors.Join(patchErr, errors.New("local progress patch exceeds observation limit"))
	}
	if patchErr != nil {
		cancel()
	}
	closeErr := patch.Close()
	diffErr := wait()
	if err := errors.Join(patchErr, closeErr, diffErr); err != nil {
		return "", fmt.Errorf("read local progress patch: %w: %s", err, strings.TrimSpace(diffError.String()))
	}
	fields := strings.Fields(string(output))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}
