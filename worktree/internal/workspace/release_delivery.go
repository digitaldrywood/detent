package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func (l *LocalGit) prepareReleaseDelivery(ctx context.Context, issue Issue) (string, error) {
	delivery := issue.ReleaseDelivery
	if !validLandingHead(delivery.BaseSHA) || !validLandingHead(delivery.HeadSHA) || delivery.BaseRef == "" || delivery.HeadRef == "" || delivery.PRNumber <= 0 {
		return "", errors.New("release delivery requires immutable base and head identities")
	}
	repository, _, ok := githubLandingRepository(delivery.Repository)
	if !ok {
		return "", errors.New("release delivery repository is invalid")
	}
	review := issue
	review.PullRequestRepository = repository
	review.PullRequestNumber = delivery.PRNumber
	review.PullRequestBranch = delivery.HeadRef
	review.PullRequestHeadSHA = delivery.HeadSHA
	head, err := fetchReviewHead(ctx, l.sourceRoot, delivery.HeadRef, review)
	if err != nil {
		return "", err
	}
	ref := "refs/remotes/origin/" + delivery.BaseRef
	if _, err := l.runGit(ctx, "fetch", "--no-write-fetch-head", "https://github.com/"+strings.TrimSuffix(repository, ".git")+".git", "+refs/heads/"+delivery.BaseRef+":"+ref); err != nil {
		return "", fmt.Errorf("fetch release delivery base: %w", err)
	}
	if _, err := l.runGit(ctx, "merge-base", "--is-ancestor", delivery.BaseSHA, head); err != nil {
		return "", refuse(LandRefusalHeadMoved, "release delivery head does not descend from the frozen base")
	}
	return head, nil
}
