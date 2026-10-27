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
)

type LandRequest struct {
	Validate func(context.Context) error `json:"-"`
	Info     Info
	Issue    Issue
	Options  LandOptions
}

type LandOutcome struct {
	Result LandResult
	Err    error
}

type BatchLander interface {
	LandChanges(context.Context, []LandRequest) []LandOutcome
}

func (l *LocalGit) LandChanges(ctx context.Context, requests []LandRequest) []LandOutcome {
	out := make([]LandOutcome, len(requests))
	if len(requests) == 0 {
		return out
	}
	fail := func(err error) []LandOutcome {
		for i := range out {
			if out[i].Result.MergeSHA == "" && out[i].Err == nil {
				out[i].Err = err
			}
		}
		return out
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return fail(err)
	}
	defer release()
	first, err := l.normalizeInfo(requests[0].Info, requests[0].Issue)
	if err != nil {
		return fail(err)
	}
	remote := requests[0].Options.Remote
	if remote == "" {
		remote = defaultGitRemote
	}
	target := requests[0].Options.TargetBranch
	if target == "" {
		target, err = remoteDefaultBranch(ctx, first.Path, remote)
		if err != nil {
			return fail(err)
		}
	}
	baseRef := "refs/remotes/" + remote + "/" + target
	staging := filepath.Join(l.root, "landing-"+first.Key)
	defer func() {
		if err := l.removeLandingWorktree(context.WithoutCancel(ctx), l.sourceRoot, staging); err != nil {
			l.logger.Warn("landing worktree left behind", "path", staging, "error", err)
		}
	}()
	if err := l.removeLandingWorktree(ctx, l.sourceRoot, staging); err != nil {
		return fail(err)
	}
	var rejectedBase string
	var rejectedPush error
	for cycle := 0; ; cycle++ {
		if _, err := runGitAt(ctx, first.Path, "fetch", remote, "+refs/heads/"+target+":"+baseRef); err != nil {
			return fail(err)
		}
		base, err := runGitAt(ctx, first.Path, "rev-parse", baseRef)
		if err != nil {
			return fail(err)
		}
		base = strings.TrimSpace(base)
		if rejectedPush != nil && base == rejectedBase {
			return fail(rejectedPush)
		}
		if cycle == 0 {
			if _, err := runGitAt(ctx, first.Path, "worktree", "add", "--detach", staging, base); err != nil {
				return fail(err)
			}
		} else if _, err := runGitAt(ctx, staging, "reset", "--hard", base); err != nil {
			return fail(err)
		}
		stageHead := base
		members := []int{}
		for i, request := range requests {
			if out[i].Err != nil || out[i].Result.MergeSHA != "" {
				continue
			}
			if request.Options.Authorize != nil {
				if err := request.Options.Authorize(ctx); err != nil {
					out[i].Err = err
					continue
				}
			}
			if request.Validate != nil {
				if err := request.Validate(ctx); err != nil {
					out[i].Err = err
					continue
				}
			}
			info, err := l.normalizeInfo(request.Info, request.Issue)
			if err == nil {
				err = l.verifyLandingWorktree(ctx, info, request.Issue, request.Options)
			}
			if err != nil {
				out[i].Err = err
				continue
			}
			if request.Options.Remote != "" && request.Options.Remote != remote || request.Options.TargetBranch != "" && request.Options.TargetBranch != target {
				out[i].Err = refuse(LandRefusalProtected, "batch members must identify the same remote and base branch")
				continue
			}
			if kept, ok := keptLanding(ctx, info.Path, request.Options.HeadSHA, baseRef); ok {
				out[i].Result = kept
				continue
			}
			head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
			if err != nil || strings.TrimSpace(head) != request.Options.HeadSHA {
				out[i].Err = errors.Join(err, refuse(LandRefusalHeadMoved, "the worktree no longer identifies the reviewed head"))
				continue
			}
			if err := verifyLandingValidation(ctx, info.Path, request.Options); err != nil {
				out[i].Err = err
				continue
			}
			method := request.Options.Method
			if method == "" {
				method = "squash"
			}
			if method != "squash" && method != "merge" && method != "rebase" {
				out[i].Err = fmt.Errorf("unsupported merge method %q", method)
				continue
			}
			integrationStarted := time.Now()
			if resolved := request.Options.Resolution; resolved != nil {
				if resolved.BaseSHA != stageHead {
					out[i].Err = &LandRefusal{Kind: LandRefusalBaseMoved, Reason: "the base changed after merge resolution", BaseSHA: stageHead}
					continue
				}
				same, verifyErr := sameLandingDiff(ctx, staging, request.Options.HeadSHA, resolved.HeadSHA, stageHead)
				if verifyErr != nil || !same {
					out[i].Err = errors.Join(verifyErr, refuse(LandRefusalHeadMoved, "merge resolution differs from the reviewed diff"))
					continue
				}
				if err := verifyLandingValidation(ctx, staging, LandOptions{ValidationCommand: request.Options.ValidationCommand, HeadSHA: resolved.HeadSHA, Validation: &resolved.Gate}); err != nil {
					out[i].Err = err
					continue
				}
			}
			integrationHead := request.Options.HeadSHA
			if resolved := request.Options.Resolution; resolved != nil {
				integrationHead = resolved.HeadSHA
			}
			merged, err := l.combine(ctx, staging, method, integrationHead, stageHead, request.Options.Message)
			result := LandResult{BaseBefore: stageHead, BaseRef: target, Method: method, Path: "clean_push", Pipeline: out[i].Result.Pipeline, PipelineDropped: out[i].Result.PipelineDropped}
			if resolved := request.Options.Resolution; resolved != nil {
				result.Rebased, result.Path, result.Gate = true, "merge_resolution", resolved.Gate
			}
			if receipt := request.Options.Validation; receipt != nil && request.Options.ValidationCommand != "" {
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
				if len(requests) > 1 && !request.Options.RebaseRequired {
					out[i].Err = err
					result.Path = "batch_member"
					result.Pipeline = append(result.Pipeline, gate.Interval("conflict_resolution", integrationStarted, time.Now(), "conflict"))
					out[i].Result.Path = result.Path
					out[i].Result.Pipeline = result.Pipeline
					continue
				}
				rebaseStarted := time.Now()
				merged, err = l.rebaseLanding(ctx, staging, request.Options.HeadSHA, stageHead, request.Options.Message)
				outcome := "clean"
				if err != nil {
					outcome = "conflict"
				}
				result.Pipeline = append(result.Pipeline, gate.Interval("rebase", rebaseStarted, time.Now(), outcome))
				result.Rebased, result.Path = true, "rebase_short_validation"
				if err != nil && request.Options.Native {
					result.Path = "merge_resolution_pending"
				}
			}
			if method == "rebase" && err == nil && request.Options.Resolution == nil {
				result.Rebased, result.Path = true, "rebase_short_validation"
			}
			if result.Rebased && err == nil && request.Options.Resolution == nil {
				validationInfo := info
				validationInfo.Path = staging
				command, packages, scopeErr := shortLandingCommand(ctx, staging, stageHead, merged, request.Options.ValidationCommand)
				result.Packages = packages
				if scopeErr != nil {
					err = scopeErr
				} else {
					result.Gate, err = l.validateLanding(ctx, validationInfo, request.Issue, command, merged, stageHead)
					if !result.Gate.StartedAt.IsZero() {
						result.Gate.TimingStage = "landing_validation"
						result.Pipeline = append(result.Pipeline, result.Gate.PipelineTiming("landing_validation"))
					}
				}
			}
			if err == nil && request.Options.SecurityAuditRequired {
				result.SecurityAudit, err = auditIntegratedLanding(ctx, staging, stageHead, merged, request.Options)
			}
			if err != nil {
				out[i] = LandOutcome{Result: result, Err: err}
				if _, resetErr := runGitAt(ctx, staging, "reset", "--hard", stageHead); resetErr != nil {
					return fail(resetErr)
				}
				continue
			}
			result.MergeSHA = merged
			out[i].Result = result
			stageHead = merged
			members = append(members, i)
		}
		if len(members) == 0 {
			return out
		}
		for _, i := range members {
			if authorize := requests[i].Options.Authorize; authorize != nil {
				if err := authorize(ctx); err != nil {
					for _, member := range members {
						out[member].Result.MergeSHA = ""
					}
					return fail(err)
				}
			}
			if requests[i].Validate != nil {
				if err := requests[i].Validate(ctx); err != nil {
					for _, member := range members {
						out[member].Result.MergeSHA = ""
					}
					return fail(err)
				}
			}
		}
		_, err = runGitAt(ctx, staging, "push", "--force-with-lease=refs/heads/"+target+":"+base, remote, stageHead+":refs/heads/"+target)
		if err != nil {
			pushErr := classifyLandingPush(err, target)
			var moved *LandRefusal
			for _, i := range members {
				out[i].Result.MergeSHA = ""
			}
			if errors.As(pushErr, &moved) && moved.Kind == LandRefusalBaseMoved && cycle == 0 {
				rejectedBase, rejectedPush = base, pushErr
				continue
			}
			return fail(pushErr)
		}
		for _, i := range members {
			if len(members) > 1 && !out[i].Result.Rebased {
				out[i].Result.Path = "batch_member"
			}
			if err := RecordLanding(ctx, requests[i].Info, requests[i].Options.HeadSHA, out[i].Result); err != nil {
				out[i].Err = err
			}
		}
		return out
	}
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
