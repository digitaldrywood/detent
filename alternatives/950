package github

import (
	"strings"
)

func pullRequestRepoFromName(name string) (pullRequestRepo, bool) {
	owner, repo, ok := splitRepositoryName(name)
	if !ok {
		return pullRequestRepo{}, false
	}
	return pullRequestRepo{Owner: owner, Name: repo}, true
}

func validPullRequestRepo(repo pullRequestRepo) bool {
	return strings.TrimSpace(repo.Owner) != "" && strings.TrimSpace(repo.Name) != ""
}
