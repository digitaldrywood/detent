package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

func (l *LocalGit) landNativeChange(ctx context.Context, info Info, issue Issue, opts LandOptions) (LandResult, error) {
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return LandResult{}, err
	}
	defer release()
	info, err = l.normalizeInfo(info, issue)
	if err != nil {
		return LandResult{}, err
	}
	remote := opts.Remote
	if remote == "" {
		remote = defaultGitRemote
	}
	destination := ""
	if opts.PublicationRepository != "" || opts.Repository != "" {
		destination, err = landingPublicationRepository(ctx, info.Path, remote, opts)
		if err != nil {
			return LandResult{}, err
		}
	}
	target := opts.TargetBranch
	if target == "" {
		if destination != "" {
			target, err = gitexec.PublicationDefaultBranch(ctx, destination)
		} else {
			target, err = remoteDefaultBranch(ctx, info.Path, remote)
		}
		if err != nil {
			return LandResult{}, err
		}
	}
	method := opts.Method
	if method == "" {
		method = "squash"
	}
	if method != "squash" && method != "merge" && method != "rebase" {
		return LandResult{}, fmt.Errorf("unsupported merge method %q", method)
	}
	baseRef := "refs/remotes/" + remote + "/" + target
	staging := filepath.Join(l.root, "landing-"+info.Key)
	defer func() {
		if err := l.removeLandingWorktree(context.WithoutCancel(ctx), l.sourceRoot, staging); err != nil {
			l.logger.Warn("landing worktree left behind", "path", staging, "error", err)
		}
	}()
	if err := l.removeLandingWorktree(ctx, l.sourceRoot, staging); err != nil {
		return LandResult{}, err
	}
	var rejectedBase string
	var rejectedPush error
	for {
		if _, err := runGitAt(ctx, info.Path, "fetch", remote, "+refs/heads/"+target+":"+baseRef); err != nil {
			return LandResult{}, err
		}
		base, err := runGitAt(ctx, info.Path, "rev-parse", baseRef)
		if err != nil {
			return LandResult{}, err
		}
		base = strings.TrimSpace(base)
		if rejectedPush != nil && base == rejectedBase {
			return LandResult{}, rejectedPush
		}
		if opts.Authorize != nil {
			if err := opts.Authorize(ctx); err != nil {
				return LandResult{}, err
			}
		}
		if err := l.verifyLandingWorktree(ctx, info, issue, opts); err != nil {
			return LandResult{}, err
		}
		if kept, ok := keptLanding(ctx, info.Path, opts.HeadSHA, baseRef); ok && (!opts.SecurityAuditRequired || kept.SecurityAudit != nil) {
			return kept, nil
		}
		head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(head) != opts.HeadSHA {
			return LandResult{}, errors.Join(err, refuse(LandRefusalHeadMoved, "the worktree no longer identifies the reviewed head"))
		}
		if err := verifyLandingValidation(ctx, info.Path, opts); err != nil {
			return LandResult{}, err
		}
		delivered, err := deliveredLanding(ctx, info.Path, opts, base, target, method)
		if err != nil {
			return LandResult{}, err
		}
		if delivered.MergeSHA != "" {
			if opts.SecurityAuditRequired {
				delivered.BaseBefore = opts.SourceBaseSHA
				auditHead := delivered.MergeSHA
				if delivered.Incorporation != nil {
					auditHead = delivered.Incorporation.SourceHeadSHA
				}
				delivered.SecurityAudit, err = l.auditDeliveredLanding(ctx, info, delivered.BaseBefore, auditHead, opts)
				if err != nil {
					delivered.MergeSHA = ""
					return delivered, err
				}
			}
			return delivered, RecordLanding(ctx, info, opts.HeadSHA, delivered)
		}
		if rejectedPush == nil {
			_, err = runGitAt(ctx, info.Path, "worktree", "add", "--detach", staging, base)
		} else {
			_, err = runGitAt(ctx, staging, "reset", "--hard", base)
		}
		if err != nil {
			return LandResult{}, err
		}
		result, err := l.integrateNativeChange(ctx, info, issue, opts, staging, base, target, method)
		if err != nil {
			return result, err
		}
		if opts.Authorize != nil {
			if err := opts.Authorize(ctx); err != nil {
				result.MergeSHA = ""
				return result, err
			}
		}
		lease := "--force-with-lease=refs/heads/" + target + ":" + base
		refspec := result.MergeSHA + ":refs/heads/" + target
		if destination != "" {
			err = gitexec.PushPublication(ctx, staging, destination, refspec, lease)
		} else {
			_, err = runGitAt(ctx, staging, "push", lease, remote, refspec)
		}
		if err == nil {
			return result, RecordLanding(ctx, info, opts.HeadSHA, result)
		}
		pushErr := classifyLandingPush(err, target)
		var moved *LandRefusal
		if rejectedPush == nil && errors.As(pushErr, &moved) && moved.Kind == LandRefusalBaseMoved {
			rejectedBase, rejectedPush = base, pushErr
			continue
		}
		result.MergeSHA = ""
		return result, pushErr
	}
}

func (l *LocalGit) integrateNativeChange(ctx context.Context, info Info, issue Issue, opts LandOptions, staging, base, target, method string) (LandResult, error) {
	integrationStarted := time.Now()
	integrationHead := opts.HeadSHA
	if resolved := opts.Resolution; resolved != nil {
		if resolved.BaseSHA != base {
			return LandResult{}, &LandRefusal{Kind: LandRefusalBaseMoved, Reason: "the base changed after merge resolution", BaseSHA: base}
		}
		same, err := sameLandingDiff(ctx, staging, opts.HeadSHA, resolved.HeadSHA, base)
		if err != nil || !same {
			return LandResult{}, errors.Join(err, refuse(LandRefusalHeadMoved, "merge resolution differs from the reviewed diff"))
		}
		if err := verifyLandingValidation(ctx, staging, LandOptions{ValidationCommand: opts.ValidationCommand, HeadSHA: resolved.HeadSHA, Validation: &resolved.Gate}); err != nil {
			return LandResult{}, err
		}
		integrationHead = resolved.HeadSHA
	}
	merged, err := l.combine(ctx, staging, method, integrationHead, base, opts.Message)
	result := LandResult{BaseBefore: base, BaseRef: target, Method: method, Path: "clean_push"}
	if resolved := opts.Resolution; resolved != nil {
		result.Rebased, result.Path, result.Gate = true, "merge_resolution", resolved.Gate
	}
	if receipt := opts.Validation; receipt != nil && opts.ValidationCommand != "" {
		result.Pipeline = append(result.Pipeline, receipt.Reused("landing_validation", time.Now().UTC()).PipelineTiming("landing_validation"))
	}
	if method == "rebase" {
		outcome := "clean"
		if err != nil {
			outcome = "conflict"
		}
		result.Pipeline = append(result.Pipeline, gate.Interval("rebase", integrationStarted, time.Now(), outcome))
	}
	var conflict *LandRefusal
	if errors.As(err, &conflict) && conflict.Kind == LandRefusalConflict {
		rebaseStarted := time.Now()
		merged, err = l.rebaseLanding(ctx, staging, opts.HeadSHA, base, opts.Message)
		outcome := "clean"
		if err != nil {
			outcome = "conflict"
			result.Path = "merge_resolution_pending"
		} else {
			result.Path = "rebase_short_validation"
		}
		result.Pipeline = append(result.Pipeline, gate.Interval("rebase", rebaseStarted, time.Now(), outcome))
		result.Rebased = true
	}
	if method == "rebase" && err == nil && opts.Resolution == nil {
		result.Rebased, result.Path = true, "rebase_short_validation"
	}
	if result.Rebased && err == nil && opts.Resolution == nil {
		var command string
		command, result.Packages, err = shortLandingCommand(ctx, staging, base, merged, opts.ValidationCommand)
		if err == nil {
			info.Path = staging
			result.Gate, err = l.validateLanding(ctx, info, issue, command, merged, base)
		}
		if !result.Gate.StartedAt.IsZero() {
			result.Gate.TimingStage = "landing_validation"
			result.Pipeline = append(result.Pipeline, result.Gate.PipelineTiming("landing_validation"))
		}
	}
	if err == nil && opts.SecurityAuditRequired {
		result.SecurityAudit, err = auditIntegratedLanding(ctx, staging, base, merged, opts)
	}
	if err == nil {
		result.MergeSHA = merged
	}
	return result, err
}

func verifyLandingValidation(ctx context.Context, path string, options LandOptions) error {
	if options.ValidationCommand == "" {
		return nil
	}
	receipt := options.Validation
	if !ValidationCoversHead(receipt, options.ValidationCommand, options.HeadSHA) {
		return refuse(LandRefusalHeadMoved, "the reviewed head has no successful validation receipt for the approved command")
	}
	tree, err := runGitAt(ctx, path, "rev-parse", options.HeadSHA+"^{tree}")
	if err != nil {
		return err
	}
	if receipt.TreeSHA != strings.TrimSpace(tree) {
		return refuse(LandRefusalHeadMoved, "the validation receipt does not cover the reviewed tree")
	}
	return nil
}

func ValidationCoversHead(receipt *gate.CommandResult, command, head string) bool {
	return receipt != nil && receipt.Command == command && receipt.HeadSHA == head && receipt.ExitCode == 0 && receipt.DurationNS > 0
}

func (l *LocalGit) rebaseLanding(ctx context.Context, path, head, base, message string) (string, error) {
	fork, err := runGitAt(ctx, path, "merge-base", head, base)
	if err != nil {
		return "", err
	}
	if _, err := runGitAt(ctx, path, "checkout", "--detach", head); err != nil {
		return "", err
	}
	if _, err := l.runCommit(ctx, path, nil, "rebase", "--onto", base, strings.TrimSpace(fork)); err != nil {
		files, filesErr := runGitAt(ctx, path, "diff", "--name-only", "--diff-filter=U", "-z")
		refusal := &LandRefusal{Kind: LandRefusalConflict, Reason: "rebasing the reviewed head conflicts: " + commandErrorOutput(err), ConflictFiles: nonemptyLandingPaths(files)}
		return "", abandon(refusal, errors.Join(filesErr, gitErr(ctx, path, "rebase", "--abort")))
	}
	rebased, err := runGitAt(ctx, path, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	if _, err := runGitAt(ctx, path, "reset", "--hard", base); err != nil {
		return "", err
	}
	return l.combine(ctx, path, "squash", strings.TrimSpace(rebased), base, message)
}

func shortLandingCommand(ctx context.Context, path, base, head, approvedCommand string) (string, []string, error) {
	if _, err := os.Stat(filepath.Join(path, "go.mod")); errors.Is(err, os.ErrNotExist) {
		return approvedCommand, nil, nil
	} else if err != nil {
		return "", nil, err
	}
	changed, err := runGitAt(ctx, path, "diff", "--name-only", "-z", base, head)
	if err != nil {
		return "", nil, err
	}
	packages := []string{}
	for _, file := range strings.Split(changed, "\x00") {
		if file == "go.mod" || file == "go.sum" {
			packages = []string{"./..."}
			break
		}
		if !strings.HasSuffix(file, ".go") {
			continue
		}
		dir := filepath.Dir(file)
		files, err := filepath.Glob(filepath.Join(path, dir, "*.go"))
		if err != nil {
			return "", nil, err
		}
		if len(files) != 0 {
			pkg := "./" + filepath.ToSlash(dir)
			if dir == "." {
				pkg = "."
			}
			packages = append(packages, pkg)
		}
	}
	slices.Sort(packages)
	packages = slices.Compact(packages)
	command := "go build -p ${TEST_PROCS:-4} ./..."
	if len(packages) > 0 {
		quoted := make([]string, len(packages))
		for i, pkg := range packages {
			quoted[i] = "'" + strings.ReplaceAll(pkg, "'", "'\\''") + "'"
		}
		scope := strings.Join(quoted, " ")
		command += " && go test -p ${TEST_PROCS:-4} -short -timeout=60s " + scope + " && golangci-lint run --allow-parallel-runners --concurrency=${TEST_PROCS:-4} --timeout=5m --new-from-rev=" + base + " --whole-files " + scope
	}
	return command, packages, nil
}

func (l *LocalGit) auditDeliveredLanding(ctx context.Context, info Info, base, head string, opts LandOptions) (*tracker.SecurityAuditReview, error) {
	if !validLandingHead(base) || base == head {
		return nil, errors.New("landing security audit requires the reviewed source base")
	}
	path := filepath.Join(l.root, "landing-audit-"+info.Key)
	if err := l.removeLandingWorktree(ctx, l.sourceRoot, path); err != nil {
		return nil, err
	}
	if _, err := runGitAt(ctx, info.Path, "worktree", "add", "--detach", path, head); err != nil {
		return nil, err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := l.removeLandingWorktree(cleanup, l.sourceRoot, path); err != nil {
			l.logger.Warn("landing audit worktree left behind", "path", path, "error", err)
		}
	}()
	return auditIntegratedLanding(ctx, path, base, head, opts)
}

func auditIntegratedLanding(ctx context.Context, path, base, head string, opts LandOptions) (*tracker.SecurityAuditReview, error) {
	if opts.SecurityAudit == nil {
		return nil, errors.New("landing security audit producer is unavailable")
	}
	if _, err := runGitAt(ctx, path, "diff", "--quiet", head, "--"); err != nil {
		return nil, fmt.Errorf("landing security audit requires the integrated source tree: %w", err)
	}
	diff, err := GitFileDiffs(ctx, path, base, tracker.MaxChangeDiffBytes)
	if err != nil {
		return nil, err
	}
	if diff.Truncated {
		return nil, errors.New("landing security audit diff exceeds the source capture limit")
	}
	for _, file := range diff.Files {
		if file.Patch == "" {
			return nil, errors.New("landing security audit source patch is incomplete")
		}
	}
	if diff.BaseSHA != base || diff.HeadSHA != head {
		return nil, errors.New("landing security audit diff differs from the integrated change")
	}
	diff.WorkspacePath = path
	audit, err := opts.SecurityAudit(ctx, diff)
	if err != nil {
		return nil, err
	}
	if audit == nil {
		return nil, errors.New("landing security audit evidence is unavailable")
	}
	run := audit.Run
	evaluation := securityaudit.Evaluate(run, nil, securityaudit.Key{ProjectID: run.ProjectID, Repository: opts.Repository, PRNumber: run.PRNumber, BaseSHA: base, HeadSHA: head}, "", []string{"none"})
	if !evaluation.Allowed {
		return nil, errors.New("landing security audit evidence differs from the integrated change")
	}
	if audit.Verdict != "pass" {
		return audit, &ValidationError{Stage: ValidationStageLanding, Err: fmt.Errorf("security audit requires %s", audit.Verdict)}
	}
	return audit, nil
}
