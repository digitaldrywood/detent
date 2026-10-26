package hubgithub

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type releasePickPull struct {
	Number int            `json:"number"`
	Body   string         `json:"body"`
	State  string         `json:"state"`
	Draft  bool           `json:"draft"`
	Merged bool           `json:"merged"`
	Head   releasePickRef `json:"head"`
	Base   releasePickRef `json:"base"`
}

type releasePickRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type releasePickCommit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
	Files []releasePickFile `json:"files"`
}

type releasePickFile struct {
	Filename         string `json:"filename"`
	PreviousFilename string `json:"previous_filename"`
	Status           string `json:"status"`
	Patch            string `json:"patch"`
	SHA              string `json:"sha"`
}

func (r *Reconciler) VerifyReleasePickRefresh(ctx context.Context, prior, next tracker.ReleasePickInput) error {
	var comparison struct {
		Status string `json:"status"`
	}
	if r == nil || r.client == nil {
		return errors.New("release repository transport is unavailable")
	}
	identity := next
	identity.BaseSHA, identity.HeadSHA = prior.BaseSHA, prior.HeadSHA
	if identity != prior || prior.BaseSHA == next.BaseSHA || prior.HeadSHA == next.HeadSHA {
		return fmt.Errorf("%w: refresh changes the release delivery identity", hubserver.ErrReleasePickMismatch)
	}
	repository := strings.TrimPrefix(next.Repository, "https://github.com/")
	if err := r.client.REST(scopedRequests(ctx, "native", "reconcile"), http.MethodGet, "/repos/"+repository+"/compare/"+prior.BaseSHA+"..."+next.BaseSHA, nil, &comparison); err != nil {
		return fmt.Errorf("verify release base advancement: %w", err)
	}
	if comparison.Status != "ahead" {
		return fmt.Errorf("%w: release base must advance its prior recorded head", hubserver.ErrReleasePickMismatch)
	}
	return r.VerifyReleasePick(ctx, next)
}

func (r *Reconciler) VerifyReleasePick(ctx context.Context, input tracker.ReleasePickInput) error {
	if r == nil || r.client == nil {
		return errors.New("release repository transport is unavailable")
	}
	ctx = scopedRequests(ctx, "native", "reconcile")
	repository := strings.TrimPrefix(input.Repository, "https://github.com/")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("%w: invalid release repository", hubserver.ErrReleasePickMismatch)
	}
	path := repositoryRESTPath(parts[0], parts[1])
	pullPath := fmt.Sprintf("%s/pulls/%d", path, input.PRNumber)
	checkPull := func() error {
		var pull releasePickPull
		if err := r.client.REST(ctx, http.MethodGet, pullPath, nil, &pull); err != nil {
			return fmt.Errorf("read release PR: %w", err)
		}
		if pull.Number != input.PRNumber || pull.State != "open" || pull.Draft || pull.Merged || pull.Head.Ref != input.HeadRef || pull.Head.SHA != input.HeadSHA || pull.Base.Ref != input.BaseRef || pull.Base.SHA != input.BaseSHA || pull.Head.Repo.FullName != repository || pull.Base.Repo.FullName != repository {
			return fmt.Errorf("%w: release PR identity mismatch", hubserver.ErrReleasePickMismatch)
		}
		var ref struct {
			Object struct {
				SHA string `json:"sha"`
			} `json:"object"`
		}
		if err := r.client.REST(ctx, http.MethodGet, path+"/git/ref/heads/"+url.PathEscape(input.BaseRef), nil, &ref); err != nil {
			return fmt.Errorf("read frozen branch: %w", err)
		}
		if ref.Object.SHA != input.BaseSHA {
			return fmt.Errorf("%w: frozen release head changed", hubserver.ErrReleasePickMismatch)
		}
		return nil
	}
	if err := checkPull(); err != nil {
		return err
	}
	var commits []releasePickCommit
	if err := r.client.REST(ctx, http.MethodGet, pullPath+"/commits?per_page=2", nil, &commits); err != nil {
		return fmt.Errorf("read release commits: %w", err)
	}
	if len(commits) != 1 || commits[0].SHA != input.HeadSHA || len(commits[0].Parents) != 1 || commits[0].Parents[0].SHA != input.BaseSHA {
		return fmt.Errorf("%w: release PR must contain exactly one pick from the recorded base", hubserver.ErrReleasePickMismatch)
	}
	if err := r.verifyReleaseSource(ctx, path, input, commits[0]); err != nil {
		return err
	}
	return checkPull()
}

func (r *Reconciler) verifyReleaseSource(ctx context.Context, path string, input tracker.ReleasePickInput, pick releasePickCommit) error {
	var sourceCompare struct {
		Status string `json:"status"`
	}
	if err := r.client.REST(ctx, http.MethodGet, path+"/compare/"+input.SourceMergeSHA+"...develop", nil, &sourceCompare); err != nil {
		return fmt.Errorf("verify develop ancestry: %w", err)
	}
	if sourceCompare.Status != "ahead" && sourceCompare.Status != "identical" {
		return fmt.Errorf("%w: source commit is not merged into develop", hubserver.ErrReleasePickMismatch)
	}
	var releaseCompare struct {
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := r.client.REST(ctx, http.MethodGet, path+"/compare/"+input.BaseSHA+"...develop", nil, &releaseCompare); err != nil {
		return fmt.Errorf("verify frozen source: %w", err)
	}
	if releaseCompare.MergeBaseCommit.SHA != input.FrozenSHA {
		return fmt.Errorf("%w: frozen release identity mismatch", hubserver.ErrReleasePickMismatch)
	}
	trailer := "(cherry picked from commit " + input.SourceMergeSHA + ")"
	linked := false
	for _, line := range strings.Split(pick.Commit.Message, "\n") {
		if strings.HasPrefix(line, "(cherry picked from commit ") {
			if linked || line != trailer {
				return fmt.Errorf("%w: release source trailer mismatch", hubserver.ErrReleasePickMismatch)
			}
			linked = true
		}
	}
	if !linked {
		return fmt.Errorf("%w: release commit lacks source provenance", hubserver.ErrReleasePickMismatch)
	}
	var source releasePickCommit
	for _, commit := range []struct {
		sha    string
		output *releasePickCommit
	}{{input.SourceMergeSHA, &source}, {input.HeadSHA, &pick}} {
		next, err := r.client.RESTPage(ctx, path+"/commits/"+commit.sha+"?per_page=100", commit.output)
		if err != nil {
			return fmt.Errorf("read release provenance patch: %w", err)
		}
		if next != "" {
			return fmt.Errorf("%w: truncated release provenance patch", hubserver.ErrReleasePickMismatch)
		}
	}
	if source.SHA != input.SourceMergeSHA || pick.SHA != input.HeadSHA || !equivalentReleasePatch(source.Files, pick.Files) {
		return fmt.Errorf("%w: release patch does not match the merged source", hubserver.ErrReleasePickMismatch)
	}
	return nil
}

func equivalentReleasePatch(source, pick []releasePickFile) bool {
	if len(source) == 0 || len(source) >= 300 || len(source) != len(pick) {
		return false
	}
	patches := func(files []releasePickFile) map[string]string {
		result := make(map[string]string, len(files))
		for _, file := range files {
			var delta strings.Builder
			delta.WriteString(file.Status + "\n" + file.PreviousFilename + "\n")
			if file.Patch == "" {
				if file.SHA == "" {
					return nil
				}
				delta.WriteString(file.SHA)
			} else {
				for _, line := range strings.Split(file.Patch, "\n") {
					if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") {
						delta.WriteString(line)
						delta.WriteByte('\n')
					}
				}
			}
			result[file.Filename] = delta.String()
		}
		return result
	}
	sourcePatches, pickPatches := patches(source), patches(pick)
	return sourcePatches != nil && pickPatches != nil && maps.Equal(sourcePatches, pickPatches)
}
