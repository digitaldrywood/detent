package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/forgeavailability"
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
	baseBefore, err := runGitAt(ctx, normalized.Path, "rev-parse", baseRef)
	if err != nil {
		return LandResult{}, fmt.Errorf("inspect fetched base: %w", err)
	}
	baseBefore = strings.TrimSpace(baseBefore)
	var pull githubLandingPull
	createdPull := false
	if opts.External != nil {
		pull, err = readExternalLandingPull(ctx, opts.GitHubClient, opts.Repository, opts.External, head, base)
		if err != nil {
			return LandResult{}, err
		}
		if pull.Head.Ref != githubLandingBranch(normalized, opts) {
			return LandResult{}, refuse(LandRefusalHeadMoved, "the external pull request branch differs from the landing workspace")
		}
	}
	if kept, found := keptLanding(ctx, normalized.Path, head, baseRef); found {
		return kept, nil
	}
	if err := l.validateLanding(ctx, normalized, issue, opts.ValidationCommand, head); err != nil {
		return LandResult{}, err
	}
	if opts.External == nil {
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
		for _, candidate := range pulls {
			if candidate.Base.Ref == base && (candidate.State == "open" || candidate.Head.SHA == head && (candidate.Merged || candidate.MergedAt != "")) {
				pull = candidate
				break
			}
		}
		if pull.Number == 0 {
			title, _, _ := strings.Cut(opts.Message, "\n")
			if err := githubLandingAPI(ctx, opts.GitHubClient, &pull, "POST", "repos/"+repository+"/pulls",
				"title="+title, "body="+tracker.AppendGitHubIssueClosingReferences(opts.Message, opts.SourceIssues),
				"head="+owner+":"+branch, "base="+base); err != nil {
				return LandResult{}, err
			}
			createdPull = true
		}
	}
	var mergeSHA string
	if pull.Merged || pull.MergedAt != "" {
		mergeSHA, err = githubLandingMergedCommit(ctx, opts.GitHubClient, repository, pull.Number, head, githubLandingBranch(normalized, opts), base)
		if err != nil {
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
		var merged githubLandingMerge
		if err := githubLandingAPI(ctx, opts.GitHubClient, &merged, "PUT", fmt.Sprintf("repos/%s/pulls/%d/merge", repository, pull.Number),
			"merge_method="+opts.Method, "sha="+head); err != nil {
			if errors.Is(err, connector.ErrPullRequestBaseOutOfDate) || GitHubLandingMergeabilityRefusal(http.MethodPut, fmt.Sprintf("repos/%s/pulls/%d/merge", repository, pull.Number), err) {
				return LandResult{}, l.verifyGitHubLandingSource(ctx, normalized, issue, opts, remote, repository, base, pull.Number, err)
			}
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
	if opts.External == nil && branch == autoBranchPrefix+"landing/"+strings.ToLower(normalized.Key)+"/"+head {
		if err := closeSupersededLandingPulls(ctx, opts.GitHubClient, repository, base, normalized.Key, pull.Number); err != nil {
			operation := "github.update_pull_request repos/" + repository + "/pulls"
			var status *github.StatusError
			if errors.As(err, &status) && status.StatusCode >= 500 && status.StatusCode <= 599 {
				err = forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: operation}, forgeavailability.ClassServer, err)
			} else if class, unavailable := forgeavailability.Classify(operation, err.Error()); unavailable {
				err = forgeavailability.NewError(forgeavailability.Scope{Host: "github.com", Operation: operation}, class, err)
			}
			return LandResult{}, fmt.Errorf("close superseded landing pull requests after merging %d: %w", pull.Number, err)
		}
	}
	return LandResult{MergeSHA: mergeSHA, BaseRef: base, BaseBefore: baseBefore, Method: opts.Method, AttemptBranchPushed: opts.External == nil}, nil
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

func (l *LocalGit) verifyGitHubLandingSource(ctx context.Context, info Info, issue Issue, opts LandOptions, remote, repository, base string, number int, refusal error) error {
	branch := githubLandingBranch(info, opts)
	var pull githubLandingPull
	if err := githubLandingAPI(ctx, opts.GitHubClient, &pull, "GET", fmt.Sprintf("repos/%s/pulls/%d", repository, number)); err != nil {
		return fmt.Errorf("%w; original landing refusal: %s", err, refusal.Error())
	}
	if pull.Number != number || pull.State != "open" || pull.Merged || pull.MergedAt != "" ||
		pull.Head.SHA != opts.HeadSHA || pull.Head.Ref != branch || pull.Head.Repo.FullName != repository ||
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
	if current["refs/heads/"+branch] != opts.HeadSHA || current["refs/heads/"+base] != fetched {
		return fmt.Errorf("landing projection differs from the current published head or base: %w", refusal)
	}
	output, err := runGitAt(ctx, info.Path, "merge-tree", "--write-tree", "--name-only", fetched, opts.HeadSHA)
	if err == nil {
		_, ancestryErr := runGitAt(ctx, info.Path, "merge-base", "--is-ancestor", fetched, opts.HeadSHA)
		var commandErr *CommandError
		if errors.As(ancestryErr, &commandErr) && commandErr.ExitCode == 1 {
			return fmt.Errorf("%w; original landing refusal: %w", &LandRefusal{Kind: LandRefusalBaseMoved, BaseSHA: fetched, Reason: fmt.Sprintf("reviewed head %s requires refresh onto %s at %s", opts.HeadSHA, base, fetched)}, refusal)
		}
		if ancestryErr != nil {
			return errors.Join(refusal, fmt.Errorf("inspect refused landing base ancestry: %w", ancestryErr))
		}
		return fmt.Errorf("source merge of reviewed head %s into %s at %s is clean: %w", opts.HeadSHA, base, fetched, refusal)
	}
	var commandErr *CommandError
	if errors.As(err, &commandErr) && commandErr.ExitCode == 1 {
		var status *github.StatusError
		if errors.As(refusal, &status) {
			return refuse(LandRefusalConflict, fmt.Sprintf("GitHub refused the pull request merge: %v; source merge of reviewed head %s into %s at %s conflicts: %s", status, opts.HeadSHA, base, fetched, strings.TrimSpace(output)))
		}
	}
	return errors.Join(refusal, fmt.Errorf("inspect refused landing source merge: %w", err))
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
				return refuse(LandRefusalHeadMoved, "GitHub refused the reviewed head merge: "+status.Error())
			}
			if status.StatusCode == http.StatusMethodNotAllowed {
				var response struct {
					Message string `json:"message"`
				}
				if decodeErr := json.Unmarshal([]byte(status.Body), &response); decodeErr == nil {
					message := strings.ToLower(response.Message)
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
