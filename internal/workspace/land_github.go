package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type githubLandingPull struct {
	Number   int    `json:"number"`
	Body     string `json:"body"`
	State    string `json:"state"`
	Merged   bool   `json:"merged"`
	MergedAt string `json:"merged_at"`
	Head     struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

type githubLandingMerge struct {
	Merged bool   `json:"merged"`
	SHA    string `json:"sha"`
}

type GitHubPublication struct {
	Repository string                          `json:"repository"`
	BaseRef    string                          `json:"base_ref"`
	HeadSHA    string                          `json:"head_sha"`
	Branch     string                          `json:"branch"`
	External   tracker.ChangeExternalReference `json:"external"`
}

func (l *LocalGit) PrepareGitHubPublication(ctx context.Context, info Info, issue Issue, opts LandOptions) (GitHubPublication, error) {
	if opts.Authorize == nil {
		return GitHubPublication{}, refuse(LandRefusalProtected, "PR publication requires current fenced execution authority; resume on the authorized source-owning runner")
	}
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return GitHubPublication{}, err
	}
	release, err := l.acquireSourceOperation(ctx)
	if err != nil {
		return GitHubPublication{}, err
	}
	defer release()
	if err := opts.Authorize(ctx); err != nil {
		return GitHubPublication{}, err
	}
	identity, err := l.GitHubPublicationIdentity(ctx, normalized, issue, opts)
	if err != nil {
		return GitHubPublication{}, err
	}
	if opts.GitHubClient == nil {
		return GitHubPublication{}, refuse(LandRefusalProtected, "PR publication requires runner authentication; verify the authorized runner credentials")
	}
	repository, owner, _ := githubLandingRepository(opts.Repository)
	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultGitRemote
	}
	branch, base := identity.Branch, identity.BaseRef
	var pull githubLandingPull
	var created bool
	if opts.External != nil {
		pull, err = readExternalLandingPull(ctx, opts.GitHubClient, opts.Repository, opts.External, opts.HeadSHA, base)
	} else {
		pull, created, _, err = l.publishGitHubPull(ctx, normalized, issue, opts, repository, owner, remote, branch, opts.HeadSHA, base)
	}
	if err != nil {
		return GitHubPublication{}, fmt.Errorf("prepare GitHub PR; preserve source and retry publication after resolving this blocker: %w", err)
	}
	if opts.External == nil && !created && pull.Number != 0 && pull.State == "open" {
		if err := githubLandingAPI(ctx, opts.GitHubClient, &pull, "GET", "repos/"+repository+"/pulls/"+strconv.Itoa(pull.Number)); err != nil {
			return GitHubPublication{}, err
		}
	}
	if !githubPublicationMatches(pull, repository, branch, opts.HeadSHA, base) || pull.State != "open" || pull.Merged || pull.MergedAt != "" {
		return GitHubPublication{}, refuse(LandRefusalHeadMoved, "the PR does not match the source repository, branch, target and head; reconcile its delivery identity before retrying publication")
	}
	publication := GitHubPublication{Repository: opts.Repository, BaseRef: base, HeadSHA: opts.HeadSHA, Branch: branch,
		External: tracker.ChangeExternalReference{Provider: "github", ID: strconv.Itoa(pull.Number), URL: opts.Repository + "/pull/" + strconv.Itoa(pull.Number)}}
	if opts.PublicationEffect != nil {
		if err := opts.PublicationEffect(ctx, "pr_create", "confirmed", publication); err != nil {
			return GitHubPublication{}, err
		}
	}
	if err := opts.Authorize(ctx); err != nil {
		return GitHubPublication{}, err
	}
	if err := l.verifyGitHubPublicationSource(ctx, normalized, issue, opts); err != nil {
		return GitHubPublication{}, err
	}
	return publication, nil
}

func (l *LocalGit) GitHubPublicationIdentity(ctx context.Context, info Info, issue Issue, opts LandOptions) (GitHubPublication, error) {
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return GitHubPublication{}, err
	}
	_, _, ok := githubLandingRepository(opts.Repository)
	if !ok || RepositoryURL(ctx, normalized.Path) != opts.Repository {
		return GitHubPublication{}, refuse(LandRefusalProtected, "PR publication requires the configured GitHub repository and runner authentication; verify the authorized runner checkout and credentials")
	}
	if err := l.verifyGitHubPublicationSource(ctx, normalized, issue, opts); err != nil {
		return GitHubPublication{}, err
	}
	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultGitRemote
	}
	if err := verifyGitHubPublicationRemote(ctx, normalized.Path, remote, opts.Repository); err != nil {
		return GitHubPublication{}, err
	}
	base := strings.TrimSpace(opts.TargetBranch)
	if base == "" {
		base, err = remoteDefaultBranch(ctx, normalized.Path, remote)
		if err != nil {
			return GitHubPublication{}, err
		}
	}
	branch := strings.TrimSpace(normalized.Branch)
	if branch == "" {
		return GitHubPublication{}, refuse(LandRefusalProtected, "PR publication requires the assigned source branch")
	}
	if _, err := runGitAt(ctx, normalized.Path, "check-ref-format", "--branch", branch); err != nil {
		return GitHubPublication{}, refuse(LandRefusalProtected, "the assigned publication branch is invalid")
	}
	return GitHubPublication{Repository: opts.Repository, BaseRef: base, Branch: branch, HeadSHA: opts.HeadSHA}, nil
}

func (l *LocalGit) verifyGitHubPublicationSource(ctx context.Context, info Info, issue Issue, opts LandOptions) error {
	if err := l.verifyLandingWorktree(ctx, info, issue, opts); err != nil {
		return err
	}
	head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if !validLandingHead(opts.HeadSHA) || strings.TrimSpace(head) != opts.HeadSHA {
		return refuse(LandRefusalHeadMoved, "the source head changed before PR publication; finalize and validate the intended current head")
	}
	return nil
}

func verifyGitHubPublicationRemote(ctx context.Context, path, remote, repository string) error {
	urls, err := runGitAt(ctx, path, "config", "--get-all", "remote."+remote+".url")
	if err != nil || HTTPSRemoteURL(urls) != repository {
		return refuse(LandRefusalProtected, "the selected publication remote does not identify the current version repository; restore the configured project remote before retrying")
	}
	pushURLs, err := runGitAt(ctx, path, "config", "--get-all", "remote."+remote+".pushurl")
	if err == nil && HTTPSRemoteURL(pushURLs) != repository {
		return refuse(LandRefusalProtected, "the publication push remote differs from the current version repository; restore its configured push destination before retrying")
	}
	return nil
}

func githubPublicationMatches(pull githubLandingPull, repository, branch, head, base string) bool {
	return pull.Number > 0 && pull.Head.SHA == head && pull.Head.Ref == branch && pull.Head.Repo.FullName == repository && pull.Base.Ref == base && pull.Base.Repo.FullName == repository
}

func (l *LocalGit) publishGitHubPull(ctx context.Context, info Info, issue Issue, opts LandOptions, repository, owner, remote, branch, head, base string) (githubLandingPull, bool, bool, error) {
	authorize := func() error {
		if opts.Authorize != nil {
			if err := opts.Authorize(ctx); err != nil {
				return err
			}
		}
		return l.verifyGitHubPublicationSource(ctx, info, issue, opts)
	}
	if err := verifyGitHubPublicationRemote(ctx, info.Path, remote, opts.Repository); err != nil {
		return githubLandingPull{}, false, false, err
	}
	previous, exists, err := remoteBranchHead(ctx, info.Path, remote, branch)
	if err != nil {
		return githubLandingPull{}, false, false, fmt.Errorf("inspect published attempt branch: %w", err)
	}
	headChanged := exists && previous != head
	var pulls []githubLandingPull
	query := "repos/" + repository + "/pulls?state=all&head=" + url.QueryEscape(owner+":"+branch) + "&per_page=100"
	if err := githubLandingAPI(ctx, opts.GitHubClient, &pulls, "GET", query); err != nil {
		return githubLandingPull{}, false, headChanged, err
	}
	var pull githubLandingPull
	for _, candidate := range pulls {
		if candidate.State == "open" && (candidate.Number <= 0 || candidate.Head.Ref != branch || candidate.Head.Repo.FullName != repository || candidate.Base.Repo.FullName != repository) {
			return githubLandingPull{}, false, headChanged, refuse(LandRefusalHeadMoved, "an open PR does not identify the configured publication repository and branch; reconcile its delivery identity before retrying")
		}
		if candidate.State == "open" && candidate.Head.SHA != head && (!exists || candidate.Head.SHA != previous) {
			return githubLandingPull{}, false, headChanged, refuse(LandRefusalHeadMoved, "the open PR head differs from the observed publication branch; preserve both sources and reconcile before retrying")
		}
		if candidate.Base.Ref == base && (candidate.State == "open" || candidate.Head.SHA == head && (candidate.Merged || candidate.MergedAt != "")) {
			if pull.Number != 0 {
				return githubLandingPull{}, false, headChanged, refuse(LandRefusalHeadMoved, "multiple PRs match the publication branch; reconcile their delivery identity before retrying publication")
			}
			pull = candidate
			continue
		}
		if candidate.State == "open" {
			return githubLandingPull{}, false, headChanged, refuse(LandRefusalHeadMoved, "an open PR on the publication branch targets another base; reconcile its delivery identity before retrying publication")
		}
	}
	if exists && previous != head {
		if _, err := runGitAt(ctx, info.Path, "cat-file", "-e", previous+"^{commit}"); err != nil {
			if _, err := runGitAt(ctx, info.Path, "fetch", remote, "refs/heads/"+branch); err != nil {
				return githubLandingPull{}, false, false, fmt.Errorf("inspect preserved publication ancestry: %w", err)
			}
		}
		common, err := runGitAt(ctx, info.Path, "merge-base", head, previous)
		if err != nil {
			return githubLandingPull{}, false, false, fmt.Errorf("verify preserved publication ancestry: %w", err)
		}
		if strings.TrimSpace(common) == head {
			return githubLandingPull{}, false, false, refuse(LandRefusalHeadMoved, "the published branch contains newer work than the current native head; recover its exact source version before retrying publication")
		}
	}
	effect := func(kind, state string) error {
		if opts.PublicationEffect == nil {
			return nil
		}
		return opts.PublicationEffect(ctx, kind, state, GitHubPublication{Repository: opts.Repository, BaseRef: base, HeadSHA: head, Branch: branch})
	}
	push := []string{"push"}
	if exists {
		push = append(push, "--force-with-lease=refs/heads/"+branch+":"+previous)
	}
	push = append(push, remote, head+":refs/heads/"+branch)
	if previous != head {
		if err := authorize(); err != nil {
			return githubLandingPull{}, false, headChanged, err
		}
		if err := effect("git_push", "pending"); err != nil {
			return githubLandingPull{}, false, headChanged, err
		}
		if _, err := runGitAt(ctx, info.Path, push...); err != nil {
			var refusal *LandRefusal
			if classified := classifyLandingPush(err, branch); errors.As(classified, &refusal) && refusal.Kind == LandRefusalProtected {
				return githubLandingPull{}, false, headChanged, errors.Join(refuse(LandRefusalProtected, "the runner cannot publish the reviewed attempt branch "+branch+": "+strings.TrimSpace(commandErrorOutput(err))), effect("git_push", "none"))
			}
			return githubLandingPull{}, false, headChanged, errors.Join(classifyLandingPush(err, branch), effect("git_push", "ambiguous"))
		}
	}
	if err := effect("git_push", "confirmed"); err != nil {
		return githubLandingPull{}, false, headChanged, err
	}
	created := false
	if pull.Number == 0 {
		if err := authorize(); err != nil {
			return githubLandingPull{}, false, headChanged, err
		}
		if err := effect("pr_create", "pending"); err != nil {
			return githubLandingPull{}, false, headChanged, err
		}
		title, _, _ := strings.Cut(opts.Message, "\n")
		if err := githubLandingAPI(ctx, opts.GitHubClient, &pull, "POST", "repos/"+repository+"/pulls",
			"title="+title, "body="+tracker.AppendGitHubIssueClosingReferences(opts.Message, opts.SourceIssues),
			"head="+owner+":"+branch, "base="+base); err != nil {
			state := "ambiguous"
			var refusal *LandRefusal
			if errors.As(err, &refusal) && refusal.Kind == LandRefusalProtected {
				state = "none"
			}
			return githubLandingPull{}, false, headChanged, errors.Join(err, effect("pr_create", state))
		}
		created = true
	}
	return pull, created, headChanged, nil
}

func (l *LocalGit) LandChangeViaGitHub(ctx context.Context, info Info, issue Issue, opts LandOptions) (result LandResult, returnErr error) {
	rebased := false
	var validation gate.CommandResult
	var baseBefore string
	defer func() {
		result.Rebased = result.Rebased || rebased
		if result.BaseBefore == "" {
			result.BaseBefore = baseBefore
		}
		if result.Gate.Command == "" {
			result.Gate = validation
		}
	}()
	normalized, err := l.normalizeInfo(info, issue)
	if err != nil {
		return LandResult{}, err
	}
	repository, owner, ok := githubLandingRepository(opts.Repository)
	if !ok || RepositoryURL(ctx, normalized.Path) != opts.Repository {
		return LandResult{}, refuse(LandRefusalProtected, "GitHub pull request landing requires the reviewed version and checkout to name the same github.com repository")
	}
	if opts.GitHubClient == nil {
		return LandResult{}, refuse(LandRefusalProtected, "GitHub pull request landing requires GitHub authentication on the project runner")
	}
	if opts.Method != "squash" && opts.Method != "merge" && opts.Method != "rebase" {
		return LandResult{}, fmt.Errorf("unsupported merge method %q", opts.Method)
	}
	remote := strings.TrimSpace(opts.Remote)
	if remote == "" {
		remote = defaultGitRemote
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
	if gate.NormalizeLandingMode(opts.LandingMode) == gate.LandingRollingBarrier {
		opts.ValidationCommand = ""
	}
	if err := l.verifyLandingWorktree(ctx, normalized, issue, opts); err != nil {
		return LandResult{}, err
	}
	branch := strings.TrimSpace(normalized.Branch)
	if branch == "" {
		return LandResult{}, refuse(LandRefusalProtected, "GitHub pull request landing needs an attempt branch to publish")
	}
	if _, err := runGitAt(ctx, normalized.Path, "check-ref-format", "--branch", branch); err != nil {
		return LandResult{}, refuse(LandRefusalProtected, "the attempt branch is not a valid git branch")
	}
	head, err := runGitAt(ctx, normalized.Path, "rev-parse", "HEAD")
	if err != nil {
		return LandResult{}, fmt.Errorf("inspect reviewed head: %w", err)
	}
	head = strings.TrimSpace(head)
	if head != opts.HeadSHA {
		return LandResult{}, refuse(LandRefusalHeadMoved, "the worktree moved after the reviewed head "+opts.HeadSHA)
	}
	base := strings.TrimSpace(opts.TargetBranch)
	if base == "" {
		base, err = remoteDefaultBranch(ctx, normalized.Path, remote)
		if err != nil {
			return LandResult{}, fmt.Errorf("resolve remote default branch: %w", err)
		}
	}
	baseRef := "refs/remotes/" + remote + "/" + base
	if _, err := runGitAt(ctx, normalized.Path, "fetch", remote, "+refs/heads/"+base+":"+baseRef); err != nil {
		return LandResult{}, fmt.Errorf("fetch base branch: %w", err)
	}
	baseBefore, err = runGitAt(ctx, normalized.Path, "rev-parse", baseRef)
	if err != nil {
		return LandResult{}, fmt.Errorf("inspect fetched base: %w", err)
	}
	baseBefore = strings.TrimSpace(baseBefore)
	if kept, found := keptLanding(ctx, normalized.Path, head, baseRef); found {
		return kept, nil
	}
	var pull githubLandingPull
	createdPull := false
	headChanged := false
	if opts.External != nil {
		pull, err = readExternalLandingPull(ctx, opts.GitHubClient, opts.Repository, opts.External, head, base)
		if err != nil {
			return LandResult{}, err
		}
		if pull.Head.Ref != githubLandingBranch(normalized, opts) {
			return LandResult{}, refuse(LandRefusalHeadMoved, "the external pull request branch differs from the landing workspace")
		}
	}
	if strings.TrimSpace(opts.ValidationCommand) != "" {
		_, validation, err = l.prepareGitHubLanding(ctx, normalized, issue, opts, baseBefore)
		if err != nil {
			return LandResult{}, err
		}
	}
	if opts.External == nil {
		pull, createdPull, headChanged, err = l.publishGitHubPull(ctx, normalized, issue, opts, repository, owner, remote, branch, head, base)
		if err != nil {
			return LandResult{}, err
		}
	}
	var mergeSHA string
	alreadyMerged := pull.Merged || pull.MergedAt != ""
	if alreadyMerged {
		mergeSHA, err = githubLandingMergedCommit(ctx, opts.GitHubClient, repository, pull.Number, head, githubLandingBranch(normalized, opts), base)
		if err != nil {
			return LandResult{}, err
		}
		if err := verifyGitHubLandingMerge(ctx, normalized.Path, remote, base, baseRef, mergeSHA); err != nil {
			return LandResult{}, err
		}
	} else {
		if pull.State != "open" {
			return LandResult{}, refuse(LandRefusalHeadMoved, "the GitHub pull request no longer names the reviewed head")
		}
		body := tracker.AppendGitHubIssueClosingReferences(pull.Body, opts.SourceIssues)
		if !createdPull && body != pull.Body {
			if pull.Head.SHA != head || pull.Head.Ref != githubLandingBranch(normalized, opts) || pull.Head.Repo.FullName != repository || pull.Base.Ref != base || pull.Base.Repo.FullName != repository {
				return LandResult{}, refuse(LandRefusalHeadMoved, "the GitHub pull request differs from the reviewed delivery source")
			}
			if err := githubLandingAPI(ctx, opts.GitHubClient, nil, "PATCH", fmt.Sprintf("repos/%s/pulls/%d", repository, pull.Number), "body="+body); err != nil {
				return LandResult{}, err
			}
		}

		if len(opts.RequiredStatusChecks) > 0 || strings.TrimSpace(opts.CITriggerLabel) != "" {
			result.CI, err = githubLandingChecks(ctx, opts, repository, pull.Number, head, githubLandingBranch(normalized, opts), base, headChanged, createdPull)
			if err != nil {
				return result, err
			}
			if result.CI.State == "pending" {
				return result, nil
			}
			if result.CI.State == "failure" {
				return result, refuse(LandRefusalProtected, "required CI failed on "+head+": "+strings.Join(result.CI.FailedChecks, ", "))
			}
		}
		if _, err := runGitAt(ctx, normalized.Path, "fetch", remote, "+refs/heads/"+base+":"+baseRef); err != nil {
			return result, fmt.Errorf("refresh validated landing base: %w", err)
		}
		currentBase, err := runGitAt(ctx, normalized.Path, "rev-parse", baseRef)
		if err != nil {
			return result, err
		}
		if strings.TrimSpace(currentBase) != baseBefore && gate.NormalizeLandingMode(opts.LandingMode) != gate.LandingRollingBarrier {
			return result, &LandRefusal{Kind: LandRefusalBaseMoved, BaseSHA: strings.TrimSpace(currentBase), Reason: "the base branch changed after landing validation"}
		}
		if gate.NormalizeLandingMode(opts.LandingMode) == gate.LandingRollingBarrier {
			baseBefore = strings.TrimSpace(currentBase)
		}
		if opts.Authorize != nil {
			if err := opts.Authorize(ctx); err != nil {
				return result, err
			}
		}
		var merged githubLandingMerge
		mergePath := fmt.Sprintf("repos/%s/pulls/%d/merge", repository, pull.Number)
		if err := githubLandingAPI(ctx, opts.GitHubClient, &merged, "PUT", mergePath,
			"merge_method="+opts.Method, "sha="+head); err != nil {
			if githubLandingSourceRefusal(mergePath, err) {
				verified := l.verifyGitHubLandingSource(ctx, normalized, issue, opts, remote, repository, base, pull.Number, head, baseBefore, err)
				retryHead := head
				if verified != nil {
					var refusal *LandRefusal
					if !errors.As(verified, &refusal) || refusal == nil || refusal.Kind != LandRefusalBaseMoved || refusal.BaseSHA == "" {
						return result, verified
					}
					baseBefore = refusal.BaseSHA
					if len(opts.RequiredStatusChecks) > 0 || strings.TrimSpace(opts.CITriggerLabel) != "" {
						return result, verified
					}
					var retryValidation gate.CommandResult
					var retryErr error
					retryHead, retryValidation, retryErr = l.refreshGitHubLanding(ctx, normalized, issue, opts, remote, baseBefore)
					if retryValidation.Command != "" {
						validation = retryValidation
					}
					if retryErr != nil {
						return LandResult{Rebased: true}, retryErr
					}
					rebased = true
				}
				if opts.Authorize != nil {
					if err := opts.Authorize(ctx); err != nil {
						return result, err
					}
				}
				if err := githubLandingAPI(ctx, opts.GitHubClient, &merged, "PUT", mergePath,
					"merge_method="+opts.Method, "sha="+retryHead); err != nil {
					if githubLandingSourceRefusal(mergePath, err) {
						verified := l.verifyGitHubLandingSource(ctx, normalized, issue, opts, remote, repository, base, pull.Number, retryHead, baseBefore, err)
						if verified != nil {
							return result, verified
						}
						return result, fmt.Errorf("%w: %w", refuse(LandRefusalBaseMoved, "waiting for GitHub to observe the published landing head"), err)
					}
					return result, err
				}
			} else if threadsErr := resolveLandingReviewThreads(ctx, opts.GitHubClient, repository, pull.Number, head, err); threadsErr != nil {
				return result, threadsErr
			} else if err := githubLandingAPI(ctx, opts.GitHubClient, &merged, "PUT", mergePath,
				"merge_method="+opts.Method, "sha="+head); err != nil {
				return result, err
			}
		}
		if !merged.Merged {
			return LandResult{}, refuse(LandRefusalProtected, "GitHub did not merge the pull request; inspect its reviews, checks and branch protection")
		}
		mergeSHA = merged.SHA
	}
	if mergeSHA == "" {
		return LandResult{}, errors.New("GitHub reported a merged pull request without a merge commit")
	}
	result = LandResult{CI: result.CI, Gate: validation, MergeSHA: mergeSHA, BaseRef: base, BaseBefore: baseBefore, Method: opts.Method, AttemptBranchPushed: opts.External == nil, Rebased: rebased}
	if err := RecordLanding(ctx, normalized, head, result); err != nil {
		return result, fmt.Errorf("keep merged landing: %w", err)
	}
	if !alreadyMerged {
		if err := verifyGitHubLandingMerge(ctx, normalized.Path, remote, base, baseRef, mergeSHA); err != nil {
			return result, err
		}
	}
	if opts.External == nil && branch == autoBranchPrefix+"landing/"+strings.ToLower(normalized.Key)+"/"+head {
		if err := closeSupersededLandingPulls(ctx, opts.GitHubClient, repository, base, normalized.Key, pull.Number); err != nil {
			operation := "github.update_pull_request repos/" + repository + "/pulls"
			var status *github.StatusError
			if errors.As(err, &status) && status.StatusCode >= 500 && status.StatusCode <= 599 {
				err = forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: operation}, forgeavailability.ClassServer, err)
			} else if class, unavailable := forgeavailability.Classify(operation, err.Error()); unavailable {
				err = forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: operation}, class, err)
			}
			return result, fmt.Errorf("close superseded landing pull requests after merging %d: %w", pull.Number, err)
		}
	}
	return result, nil
}

func verifyGitHubLandingMerge(ctx context.Context, path, remote, base, baseRef, mergeSHA string) error {
	if _, err := runGitAt(ctx, path, "fetch", remote, "+refs/heads/"+base+":"+baseRef); err != nil {
		return fmt.Errorf("verify merged base branch: %w", err)
	}
	if _, err := runGitAt(ctx, path, "merge-base", "--is-ancestor", mergeSHA, baseRef); err != nil {
		return fmt.Errorf("GitHub merge commit %s is not on %s: %w", mergeSHA, base, err)
	}
	return nil
}

type githubLandingClient struct {
	GitHubRESTClient
}

func (c githubLandingClient) REST(ctx context.Context, method, path string, body, result any) error {
	return githubLandingREST(ctx, c.GitHubRESTClient, method, path, body, result)
}

func githubLandingChecks(ctx context.Context, opts LandOptions, repository string, number int, head, branch, base string, headChanged, createdPull bool) (*tracker.NativeLandingCIReceipt, error) {
	var pull githubLandingPull
	if err := githubLandingAPI(ctx, opts.GitHubClient, &pull, "GET", fmt.Sprintf("repos/%s/pulls/%d", repository, number)); err != nil {
		return nil, err
	}
	if pull.Number != number || pull.State != "open" || pull.Merged || pull.MergedAt != "" || pull.Head.SHA != head || pull.Head.Ref != branch || pull.Head.Repo.FullName != repository || pull.Base.Ref != base || pull.Base.Repo.FullName != repository {
		return nil, refuse(LandRefusalHeadMoved, "the GitHub pull request differs from the reviewed delivery source")
	}
	receipt, err := github.ReadLandingChecks(ctx, githubLandingClient{opts.GitHubClient}, repository, head, opts.RequiredStatusChecks)
	if err != nil {
		return nil, err
	}
	receipt.PullRequest, receipt.TriggerLabel = number, strings.TrimSpace(opts.CITriggerLabel)
	if previous := opts.PreviousCI; previous != nil && previous.HeadSHA == head && previous.PullRequest == number && previous.TriggerLabel == receipt.TriggerLabel && previous.Triggered {
		receipt.Triggered = true
	}
	needsTrigger := headChanged || len(receipt.MissingChecks) > 0 || createdPull && len(opts.RequiredStatusChecks) == 0
	if receipt.TriggerLabel == "" || receipt.State == "failure" || !needsTrigger || receipt.Triggered && !headChanged {
		return receipt, nil
	}
	if opts.CITriggerLabelStagger > 0 {
		select {
		case <-ctx.Done():
			return receipt, ctx.Err()
		case <-time.After(opts.CITriggerLabelStagger):
		}
	}
	var labels []struct {
		Name string `json:"name"`
	}
	path := fmt.Sprintf("repos/%s/issues/%d/labels", repository, number)
	present := false
	for page := 1; ; page++ {
		if err := githubLandingREST(ctx, opts.GitHubClient, http.MethodGet, fmt.Sprintf("%s?per_page=100&page=%d", path, page), nil, &labels); err != nil {
			return receipt, err
		}
		for _, label := range labels {
			present = present || label.Name == receipt.TriggerLabel
		}
		if present || len(labels) < 100 {
			break
		}
	}
	if present {
		if err := githubLandingREST(ctx, opts.GitHubClient, http.MethodDelete, path+"/"+url.PathEscape(receipt.TriggerLabel), nil, nil); err != nil {
			return receipt, err
		}
	}
	if err := githubLandingREST(ctx, opts.GitHubClient, http.MethodPost, path, map[string][]string{"labels": {receipt.TriggerLabel}}, nil); err != nil {
		return receipt, err
	}
	receipt.Triggered = true
	if len(opts.RequiredStatusChecks) > 0 {
		receipt.State = "pending"
	}
	return receipt, nil
}

func (l *LocalGit) prepareGitHubLanding(ctx context.Context, info Info, issue Issue, opts LandOptions, base string) (string, gate.CommandResult, error) {
	staging := filepath.Join(l.root, "landing-"+info.Key)
	defer func() {
		if err := l.removeLandingWorktree(context.WithoutCancel(ctx), l.sourceRoot, staging); err != nil {
			l.logger.Warn("landing worktree left behind", "path", staging, "error", err)
		}
	}()
	if err := l.removeLandingWorktree(context.WithoutCancel(ctx), l.sourceRoot, staging); err != nil {
		return "", gate.CommandResult{}, err
	}
	if _, err := runGitAt(ctx, info.Path, "worktree", "add", "--detach", staging, base); err != nil {
		return "", gate.CommandResult{}, fmt.Errorf("add landing validation worktree: %w", err)
	}
	head, err := combine(ctx, staging, opts.Method, opts.HeadSHA, base, opts.Message)
	if err != nil {
		return "", gate.CommandResult{}, err
	}
	validationInfo := info
	validationInfo.Path = staging
	validationInfo.Created = true
	if err := l.runHookUnderSourceLock(ctx, "after_create", l.hooks.AfterCreate, validationInfo, issue); err != nil {
		return "", gate.CommandResult{}, err
	}
	if err := l.runHookUnderSourceLock(ctx, "before_run", l.hooks.BeforeRun, validationInfo, issue); err != nil {
		return "", gate.CommandResult{}, err
	}
	validation, err := l.validateLanding(ctx, validationInfo, issue, opts.ValidationCommand, head)
	return head, validation, err
}

func (l *LocalGit) refreshGitHubLanding(ctx context.Context, info Info, issue Issue, opts LandOptions, remote, base string) (string, gate.CommandResult, error) {
	head, validation, err := l.prepareGitHubLanding(ctx, info, issue, opts, base)
	if err != nil {
		return "", validation, err
	}
	branch := githubLandingBranch(info, opts)
	if _, err := runGitAt(ctx, info.Path, "push", "--force-with-lease=refs/heads/"+branch+":"+opts.HeadSHA, remote, head+":refs/heads/"+branch); err != nil {
		return "", validation, classifyLandingPush(err, branch)
	}
	return head, validation, nil
}

func closeSupersededLandingPulls(ctx context.Context, client GitHubRESTClient, repository, base, key string, mergedNumber int) error {
	prefix := autoBranchPrefix + "landing/" + strings.ToLower(key) + "/"
	var superseded []githubLandingPull
	for page := 1; ; page++ {
		var pulls []githubLandingPull
		path := fmt.Sprintf("repos/%s/pulls?state=open&base=%s&per_page=100&page=%d", repository, url.QueryEscape(base), page)
		if err := client.REST(ctx, http.MethodGet, path, nil, &pulls); err != nil {
			return err
		}
		for _, pull := range pulls {
			if pull.Number <= 0 || pull.Number >= mergedNumber || pull.State != "open" || pull.Merged || pull.MergedAt != "" ||
				pull.Head.Repo.FullName != repository || pull.Base.Repo.FullName != repository || pull.Base.Ref != base ||
				!validLandingHead(pull.Head.SHA) || pull.Head.Ref != prefix+pull.Head.SHA {
				continue
			}
			superseded = append(superseded, pull)
		}
		if len(pulls) < 100 {
			break
		}
	}
	for _, pull := range superseded {
		comment := map[string]string{"body": fmt.Sprintf("Superseded by the merged landing pull request https://github.com/%s/pull/%d.", repository, mergedNumber)}
		if err := client.REST(ctx, http.MethodPost, fmt.Sprintf("repos/%s/issues/%d/comments", repository, pull.Number), comment, nil); err != nil {
			return err
		}
		if err := client.REST(ctx, http.MethodPatch, fmt.Sprintf("repos/%s/pulls/%d", repository, pull.Number), map[string]string{"state": "closed"}, nil); err != nil {
			return err
		}
	}
	return nil
}

func githubLandingMergedCommit(ctx context.Context, client GitHubRESTClient, repository string, number int, head, branch, base string) (string, error) {
	owner, name, _ := strings.Cut(repository, "/")
	var result struct {
		Repository *struct {
			NameWithOwner string `json:"nameWithOwner"`
			PullRequest   *struct {
				Number         int    `json:"number"`
				Merged         bool   `json:"merged"`
				HeadRefOID     string `json:"headRefOid"`
				HeadRefName    string `json:"headRefName"`
				BaseRefName    string `json:"baseRefName"`
				HeadRepository *struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"headRepository"`
				MergeCommit *struct {
					OID string `json:"oid"`
				} `json:"mergeCommit"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	query := `query NativeLandingMergedCommit($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){nameWithOwner pullRequest(number:$number){number merged headRefOid headRefName baseRefName headRepository{nameWithOwner} mergeCommit{oid}}}}`
	if err := client.GraphQL(ctx, query, map[string]any{"owner": owner, "name": name, "number": number}, &result); err != nil {
		return "", fmt.Errorf("read already merged pull request commit: %w", err)
	}
	if result.Repository == nil || result.Repository.NameWithOwner != repository || result.Repository.PullRequest == nil {
		return "", refuse(LandRefusalProtected, "the merged pull request repository differs from the authorized landing source")
	}
	pull := result.Repository.PullRequest
	if pull.Number != number || !pull.Merged || pull.HeadRefOID != head || pull.HeadRefName != branch || pull.BaseRefName != base || pull.HeadRepository == nil || pull.HeadRepository.NameWithOwner != repository {
		return "", refuse(LandRefusalHeadMoved, "the merged pull request no longer names the reviewed source")
	}
	if pull.MergeCommit == nil || !validLandingHead(pull.MergeCommit.OID) {
		return "", errors.New("GitHub reported a merged pull request without a merge commit")
	}
	return pull.MergeCommit.OID, nil
}

func githubLandingBranch(info Info, opts LandOptions) string {
	if opts.External != nil && info.ReviewBranch != "" {
		return info.ReviewBranch
	}
	return info.Branch
}

func (l *LocalGit) verifyGitHubLandingSource(ctx context.Context, info Info, issue Issue, opts LandOptions, remote, repository, base string, number int, publishedHead, validatedBase string, refusal error) error {
	branch := githubLandingBranch(info, opts)
	var pull githubLandingPull
	if err := githubLandingAPI(ctx, opts.GitHubClient, &pull, "GET", fmt.Sprintf("repos/%s/pulls/%d", repository, number)); err != nil {
		return fmt.Errorf("%w; original landing refusal: %s", err, refusal.Error())
	}
	if pull.Number != number || pull.State != "open" || pull.Merged || pull.MergedAt != "" ||
		pull.Head.Ref != branch || pull.Head.Repo.FullName != repository ||
		pull.Base.Ref != base || pull.Base.Repo.FullName != repository || !validLandingHead(pull.Base.SHA) {
		return fmt.Errorf("landing projection does not identify the current reviewed source: %w", refusal)
	}
	if err := l.verifyLandingWorktree(ctx, info, issue, opts); err != nil {
		return errors.Join(err, refusal)
	}
	head, err := runGitAt(ctx, info.Path, "rev-parse", "HEAD")
	if err != nil {
		return errors.Join(refusal, err)
	}
	if strings.TrimSpace(head) != opts.HeadSHA {
		return errors.Join(refuse(LandRefusalHeadMoved, "the worktree moved after the reviewed head "+opts.HeadSHA), refusal)
	}
	baseRef := "refs/remotes/" + remote + "/" + base
	if _, err := runGitAt(ctx, info.Path, "fetch", remote, "+refs/heads/"+base+":"+baseRef); err != nil {
		return githubLandingConflictReadFailure("git fetch", fmt.Errorf("refresh refused landing base: %w", err), refusal)
	}
	fetched, err := runGitAt(ctx, info.Path, "rev-parse", baseRef)
	if err != nil {
		return errors.Join(refusal, err)
	}
	fetched = strings.TrimSpace(fetched)
	refs, err := runGitAt(ctx, info.Path, "ls-remote", remote, "refs/heads/"+branch, "refs/heads/"+base)
	if err != nil {
		return githubLandingConflictReadFailure("git ls-remote", fmt.Errorf("verify refused landing refs: %w", err), refusal)
	}
	current := make(map[string]string)
	for _, line := range strings.Split(refs, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			current[fields[1]] = fields[0]
		}
	}
	if current["refs/heads/"+branch] != publishedHead {
		return fmt.Errorf("landing projection differs from the current published head or base: %w", refusal)
	}
	if current["refs/heads/"+base] != fetched || pull.Head.SHA != publishedHead {
		var status *github.StatusError
		if errors.As(refusal, &status) && status.StatusCode == http.StatusConflict {
			return fmt.Errorf("%w: %w", refuse(LandRefusalBaseMoved, "waiting for GitHub to observe the current landing head and base"), refusal)
		}
		return fmt.Errorf("landing projection does not identify the current reviewed source: %w", refusal)
	}
	_, err = runGitAt(ctx, info.Path, "merge-tree", "--write-tree", "--name-only", fetched, opts.HeadSHA)
	if err == nil {
		if fetched != validatedBase {
			return fmt.Errorf("%w; original landing refusal: %w", &LandRefusal{Kind: LandRefusalBaseMoved, BaseSHA: fetched, Reason: fmt.Sprintf("reviewed head %s requires refresh onto %s at %s", opts.HeadSHA, base, fetched)}, refusal)
		}
		var status *github.StatusError
		if errors.As(refusal, &status) && status.StatusCode == http.StatusConflict {
			return nil
		}
		return fmt.Errorf("source merge of reviewed head %s into %s at %s is clean: %w", opts.HeadSHA, base, fetched, refusal)
	}
	var commandErr *CommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
		var status *github.StatusError
		if errors.As(refusal, &status) {
			return refuse(LandRefusalConflict, fmt.Sprintf("GitHub refused the pull request merge: %v; source merge of reviewed head %s into %s at %s conflicts: %s", status, opts.HeadSHA, base, fetched, strings.TrimSpace(commandErrorOutput(err))))
		}
	}
	return errors.Join(refusal, fmt.Errorf("inspect refused landing source merge: %w", err))
}

func githubLandingSourceRefusal(path string, err error) bool {
	if errors.Is(err, github.ErrRateLimited) {
		return false
	}
	if errors.Is(err, connector.ErrPullRequestBaseOutOfDate) || GitHubLandingMergeabilityRefusal(http.MethodPut, path, err) {
		return true
	}
	var status *github.StatusError
	return errors.As(err, &status) && status != nil && status.StatusCode == http.StatusConflict
}

func githubLandingConflictReadFailure(operation string, err, refusal error) error {
	detail := err.Error() + "\n" + commandErrorOutput(err)
	class, unavailable := forgeavailability.Classify(operation, detail)
	if strings.Contains(strings.ToLower(detail), "permission denied (publickey)") || strings.Contains(strings.ToLower(detail), "authentication failed") {
		class, unavailable = forgeavailability.ClassTransport, true
	}
	cause := errors.Join(err, refusal)
	if unavailable {
		return forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: operation}, class, cause)
	}
	return cause
}

func readExternalLandingPull(ctx context.Context, client GitHubRESTClient, repositoryURL string, external *tracker.ChangeExternalReference, head, base string) (githubLandingPull, error) {
	repository, _, ok := githubLandingRepository(repositoryURL)
	if !ok || external.Provider != "github" {
		return githubLandingPull{}, refuse(LandRefusalProtected, "the external pull request must belong to the reviewed GitHub repository")
	}
	number, err := strconv.Atoi(external.ID)
	if err != nil || number <= 0 || strconv.Itoa(number) != external.ID || external.URL != repositoryURL+"/pull/"+external.ID {
		return githubLandingPull{}, refuse(LandRefusalProtected, "the external pull request identity must match the reviewed repository")
	}
	if client == nil {
		return githubLandingPull{}, refuse(LandRefusalProtected, "GitHub pull request landing requires GitHub authentication on the project runner")
	}
	var pull githubLandingPull
	if err := githubLandingAPI(ctx, client, &pull, "GET", fmt.Sprintf("repos/%s/pulls/%d", repository, number)); err != nil {
		return pull, err
	}
	if pull.Number != number || pull.Base.Repo.FullName != repository || pull.Head.Repo.FullName != repository || pull.Base.Ref != base {
		return pull, refuse(LandRefusalProtected, "the external pull request repository or base differs from the authorized landing source")
	}
	if pull.Head.SHA != head || pull.Head.Ref == "" || pull.State != "open" && !pull.Merged && pull.MergedAt == "" {
		return pull, refuse(LandRefusalHeadMoved, "the external pull request no longer names the reviewed head")
	}
	return pull, nil
}

func githubLandingRepository(repository string) (name, owner string, ok bool) {
	parsed, err := url.Parse(repository)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil || parsed.EscapedPath() != parsed.Path {
		return "", "", false
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || strings.ContainsAny(parts[0]+parts[1], " \t\r\n") {
		return "", "", false
	}
	return parts[0] + "/" + parts[1], parts[0], true
}

func githubLandingAPI(ctx context.Context, client GitHubRESTClient, result any, method, path string, fields ...string) error {
	var body any
	if len(fields) > 0 {
		values := make(map[string]string, len(fields))
		for _, field := range fields {
			key, value, _ := strings.Cut(field, "=")
			values[key] = value
		}
		body = values
	}
	return githubLandingREST(ctx, client, method, path, body, result)
}

func githubLandingREST(ctx context.Context, client GitHubRESTClient, method, path string, body, result any) error {
	err := client.REST(ctx, method, path, body, result)
	if err == nil || errors.Is(err, github.ErrRateLimited) {
		return err
	}
	err = github.ClassifyPullRequestMergeError(method, path, err)
	if errors.Is(err, connector.ErrPullRequestBaseOutOfDate) {
		return fmt.Errorf("GitHub refused %s %s: %w: %w", method, path,
			refuse(LandRefusalBaseMoved, "GitHub refused the reviewed head merge because the base branch advanced"), err)
	}
	var status *github.StatusError
	if errors.As(err, &status) {
		if method == http.MethodPut && githubLandingMergePath(path) {
			if status.StatusCode == http.StatusConflict {
				return fmt.Errorf("%w: %w", refuse(LandRefusalHeadMoved, "GitHub refused the reviewed head merge: "+status.Error()), err)
			}
			if status.StatusCode == http.StatusMethodNotAllowed {
				var response struct {
					Message string `json:"message"`
				}
				if decodeErr := json.Unmarshal([]byte(status.Body), &response); decodeErr == nil {
					message := strings.ToLower(response.Message)
					if strings.Contains(message, "conversation") && strings.Contains(message, "resolved") {
						return refuse(LandRefusalProtected, ReviewConversationsRefusalText+" GitHub refused the reviewed head merge: "+status.Error())
					}
					if strings.Contains(message, "pull request is closed") || strings.Contains(message, "pull request is not open") {
						return refuse(LandRefusalHeadMoved, "GitHub refused the reviewed head merge: "+status.Error())
					}
					if GitHubLandingMergeabilityRefusal(method, path, err) {
						return fmt.Errorf("GitHub refused %s %s: %w: %w", method, path,
							refuse(LandRefusalBaseMoved, "the pull request mergeability projection requires current source verification"), err)
					}
				}
			}
		}
		switch status.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
			return refuse(LandRefusalProtected, "GitHub refused the pull request operation: "+status.Error()+". Resolve its authentication, reviews, checks or branch protection, then approve the Change Request again.")
		}
		if status.StatusCode >= http.StatusInternalServerError && status.StatusCode <= 599 {
			return forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: "github.update_pull_request " + path}, forgeavailability.ClassServer, err)
		}
		return fmt.Errorf("GitHub pull request operation failed: %w", err)
	}
	operation := "github.update_pull_request " + path
	if class, unavailable := forgeavailability.Classify(operation, err.Error()); unavailable {
		return forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: operation}, class, err)
	}
	return fmt.Errorf("GitHub pull request operation failed: %w", err)
}

func GitHubLandingMergeabilityRefusal(method, path string, err error) bool {
	if method != http.MethodPut || !githubLandingMergePath(path) || errors.Is(err, github.ErrRateLimited) {
		return false
	}
	var status *github.StatusError
	if !errors.As(err, &status) || status.StatusCode != http.StatusMethodNotAllowed {
		return false
	}
	var response struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(status.Body), &response) != nil {
		return false
	}
	message := strings.ToLower(response.Message)
	return !strings.Contains(message, "pull request is closed") && !strings.Contains(message, "pull request is not open") &&
		(strings.Contains(message, "merge conflict") || strings.Contains(message, "pull request is not mergeable"))
}

func githubLandingMergePath(path string) bool {
	parts := strings.Split(path, "/")
	if len(parts) != 6 || parts[0] != "repos" || parts[1] == "" || parts[2] == "" || parts[3] != "pulls" || parts[5] != "merge" {
		return false
	}
	number, err := strconv.Atoi(parts[4])
	return err == nil && number > 0 && strconv.Itoa(number) == parts[4]
}
