package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector/github"
)

type githubLandingPull struct {
	Number         int    `json:"number"`
	State          string `json:"state"`
	Merged         bool   `json:"merged"`
	MergedAt       string `json:"merged_at"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type githubLandingMerge struct {
	Merged bool   `json:"merged"`
	SHA    string `json:"sha"`
}

// LandChangeViaGitHub lands a reviewed head through a GitHub pull request
// on the project runner; the Hub receives only the resulting commit identity.
func (l *LocalGit) LandChangeViaGitHub(ctx context.Context, info Info, issue Issue, opts LandOptions) (LandResult, error) {
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
	baseBefore, err := runGitAt(ctx, normalized.Path, "rev-parse", baseRef)
	if err != nil {
		return LandResult{}, fmt.Errorf("inspect fetched base: %w", err)
	}
	baseBefore = strings.TrimSpace(baseBefore)
	if kept, found := keptLanding(ctx, normalized.Path, head, baseRef); found {
		return kept, nil
	}
	previous, exists, err := remoteBranchHead(ctx, normalized.Path, remote, branch)
	if err != nil {
		return LandResult{}, fmt.Errorf("inspect published attempt branch: %w", err)
	}
	push := []string{"push"}
	if exists {
		push = append(push, "--force-with-lease=refs/heads/"+branch+":"+previous)
	}
	push = append(push, remote, head+":refs/heads/"+branch)
	if _, err := runGitAt(ctx, normalized.Path, push...); err != nil {
		var refusal *LandRefusal
		if classified := classifyLandingPush(err, branch); errors.As(classified, &refusal) && refusal.Kind == LandRefusalProtected {
			return LandResult{}, refuse(LandRefusalProtected, "the runner cannot publish the reviewed attempt branch "+branch+": "+strings.TrimSpace(commandErrorOutput(err)))
		}
		return LandResult{}, classifyLandingPush(err, branch)
	}
	var pulls []githubLandingPull
	query := "repos/" + repository + "/pulls?state=all&head=" + url.QueryEscape(owner+":"+branch) + "&per_page=100"
	if err := githubLandingAPI(ctx, opts.GitHubClient, &pulls, "GET", query); err != nil {
		return LandResult{}, err
	}
	var pull githubLandingPull
	for _, candidate := range pulls {
		if candidate.Base.Ref == base && (candidate.State == "open" || candidate.Head.SHA == head && (candidate.Merged || candidate.MergedAt != "")) {
			pull = candidate
			break
		}
	}
	if pull.Number == 0 {
		title, _, _ := strings.Cut(opts.Message, "\n")
		if err := githubLandingAPI(ctx, opts.GitHubClient, &pull, "POST", "repos/"+repository+"/pulls",
			"title="+title, "body="+opts.Message,
			"head="+owner+":"+branch, "base="+base); err != nil {
			return LandResult{}, err
		}
	}
	var mergeSHA string
	if pull.Merged || pull.MergedAt != "" {
		mergeSHA = pull.MergeCommitSHA
	} else {
		if pull.State != "open" {
			return LandResult{}, refuse(LandRefusalHeadMoved, "the GitHub pull request no longer names the reviewed head")
		}
		var merged githubLandingMerge
		if err := githubLandingAPI(ctx, opts.GitHubClient, &merged, "PUT", fmt.Sprintf("repos/%s/pulls/%d/merge", repository, pull.Number),
			"merge_method="+opts.Method, "sha="+head); err != nil {
			return LandResult{}, err
		}
		if !merged.Merged {
			return LandResult{}, refuse(LandRefusalProtected, "GitHub did not merge the pull request; inspect its reviews, checks and branch protection")
		}
		mergeSHA = merged.SHA
	}
	if mergeSHA == "" {
		return LandResult{}, errors.New("GitHub reported a merged pull request without a merge commit")
	}
	if _, err := runGitAt(ctx, normalized.Path, "fetch", remote, "+refs/heads/"+base+":"+baseRef); err != nil {
		return LandResult{}, fmt.Errorf("verify merged base branch: %w", err)
	}
	if _, err := runGitAt(ctx, normalized.Path, "merge-base", "--is-ancestor", mergeSHA, baseRef); err != nil {
		return LandResult{}, fmt.Errorf("GitHub merge commit %s is not on %s: %w", mergeSHA, base, err)
	}
	return LandResult{MergeSHA: mergeSHA, BaseRef: base, BaseBefore: baseBefore, Method: opts.Method, AttemptBranchPushed: true}, nil
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
	err := client.REST(ctx, method, path, body, result)
	if err == nil || errors.Is(err, github.ErrRateLimited) {
		return err
	}
	var status *github.StatusError
	if errors.As(err, &status) {
		if method == http.MethodPut && strings.HasSuffix(path, "/merge") {
			message := strings.ToLower(status.Body)
			if status.StatusCode == http.StatusConflict && strings.Contains(message, "head branch was modified") || status.StatusCode == http.StatusMethodNotAllowed && (strings.Contains(message, "pull request is closed") || strings.Contains(message, "pull request is not open")) {
				return refuse(LandRefusalHeadMoved, "GitHub refused the reviewed head merge: "+status.Error())
			}
			if status.StatusCode == http.StatusMethodNotAllowed && (strings.Contains(message, "merge conflict") || strings.Contains(message, "pull request is not mergeable")) {
				return refuse(LandRefusalConflict, "GitHub refused the pull request merge: "+status.Error())
			}
		}
		switch status.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
			return refuse(LandRefusalProtected, "GitHub refused the pull request operation: "+status.Error()+". Resolve its authentication, reviews, checks or branch protection, then approve the Change Request again.")
		}
	}
	return fmt.Errorf("GitHub pull request operation failed: %w", err)
}
