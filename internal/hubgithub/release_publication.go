package hubgithub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func (r *Reconciler) PublishReleasePick(ctx context.Context, item tracker.NativeWorkItemID, input tracker.ReleasePickInput, authorize func(context.Context) error) (tracker.ReleasePickPublication, error) {
	var result tracker.ReleasePickPublication
	if r == nil || r.client == nil {
		return result, errors.New("release repository transport is unavailable")
	}
	ctx = scopedRequests(ctx, "native", "reconcile")
	repository := strings.TrimPrefix(input.Repository, "https://github.com/")
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || input.PRNumber != 0 {
		return result, fmt.Errorf("%w: invalid publication identity", hubserver.ErrReleasePickMismatch)
	}
	path := repositoryRESTPath(parts[0], parts[1])
	checkRefs := func() error {
		for _, ref := range []struct{ name, sha string }{{input.HeadRef, input.HeadSHA}, {input.BaseRef, input.BaseSHA}} {
			var actual struct {
				Object struct {
					SHA string `json:"sha"`
				} `json:"object"`
			}
			if err := r.client.REST(ctx, http.MethodGet, path+"/git/ref/heads/"+url.PathEscape(ref.name), nil, &actual); err != nil {
				return err
			}
			if actual.Object.SHA != ref.sha {
				return fmt.Errorf("%w: prepared branch moved", hubserver.ErrReleasePickMismatch)
			}
		}
		return nil
	}
	if err := checkRefs(); err != nil {
		return result, err
	}
	var pick releasePickCommit
	next, err := r.client.RESTPage(ctx, path+"/commits/"+input.HeadSHA+"?per_page=100", &pick)
	if err != nil {
		return result, err
	}
	if next != "" || pick.SHA != input.HeadSHA || len(pick.Parents) != 1 || pick.Parents[0].SHA != input.BaseSHA {
		return result, fmt.Errorf("%w: prepared head must be one pick from the exact base", hubserver.ErrReleasePickMismatch)
	}
	if err := r.verifyReleaseSource(ctx, path, input, pick); err != nil {
		return result, err
	}
	body := fmt.Sprintf("Detent release pick for %s\n\nSource issue: %s\nSource Change: %s\nSource merge: %s\nPrepared head: %s\nRelease base: %s at %s\nFrozen source: %s", item, input.SourceIssueID, input.SourceChangeID, input.SourceMergeSHA, input.HeadSHA, input.BaseRef, input.BaseSHA, input.FrozenSHA)
	var pulls []releasePickPull
	next, err = r.client.RESTPage(ctx, path+"/pulls?"+url.Values{"state": {"all"}, "head": {parts[0] + ":" + input.HeadRef}, "per_page": {"100"}}.Encode(), &pulls)
	if err != nil {
		return result, err
	}
	if next != "" || len(pulls) > 1 {
		return result, fmt.Errorf("%w: ambiguous existing release PRs", hubserver.ErrReleasePickMismatch)
	}
	if err := checkRefs(); err != nil {
		return result, err
	}
	if err := authorize(ctx); err != nil {
		return result, err
	}
	var pull releasePickPull
	if len(pulls) == 1 {
		pull = pulls[0]
	} else {
		request := struct {
			Title               string `json:"title"`
			Body                string `json:"body"`
			Head                string `json:"head"`
			Base                string `json:"base"`
			Draft               bool   `json:"draft"`
			MaintainerCanModify bool   `json:"maintainer_can_modify"`
		}{"Release pick: " + string(input.SourceIssueID) + " → " + input.BaseRef, body, input.HeadRef, input.BaseRef, false, false}
		if err := r.client.REST(ctx, http.MethodPost, path+"/pulls", request, &pull); err != nil {
			return result, fmt.Errorf("publish release PR: %w", err)
		}
	}
	if pull.Number < 1 || pull.Body != body || pull.State != "open" || pull.Draft || pull.Merged || pull.Head.Ref != input.HeadRef || pull.Head.SHA != input.HeadSHA || pull.Base.Ref != input.BaseRef || pull.Base.SHA != input.BaseSHA || pull.Head.Repo.FullName != repository || pull.Base.Repo.FullName != repository {
		return result, fmt.Errorf("%w: differing existing release PR", hubserver.ErrReleasePickMismatch)
	}
	input.PRNumber = pull.Number
	if err := r.VerifyReleasePick(ctx, input); err != nil {
		return result, err
	}
	return tracker.ReleasePickPublication{ReleasePickInput: input, WorkItemID: item, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repository, pull.Number)}, nil
}
