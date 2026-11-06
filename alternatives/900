package github

import (
	"strconv"
	"strings"
)

type issueRef struct {
	Owner  string
	Name   string
	Number int
}

func splitRepositoryName(repo string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner := strings.TrimSpace(parts[0])
	name := strings.TrimSpace(parts[1])
	if owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

func pullRequestRepoFromIdentifier(identifier string) (pullRequestRepo, bool) {
	repo, _, ok := splitIssueIdentifier(identifier)
	if !ok {
		return pullRequestRepo{}, false
	}
	owner, name, ok := splitRepositoryName(repo)
	if !ok {
		return pullRequestRepo{}, false
	}
	return pullRequestRepo{Owner: owner, Name: name}, true
}

func splitIssueIdentifier(identifier string) (string, int, bool) {
	identifier = strings.TrimSpace(identifier)
	index := strings.LastIndex(identifier, "#")
	if index <= 0 || index == len(identifier)-1 {
		return "", 0, false
	}
	number, err := strconv.Atoi(identifier[index+1:])
	if err != nil || number <= 0 {
		return "", 0, false
	}
	repo := strings.TrimSpace(identifier[:index])
	if repo == "" {
		return "", 0, false
	}
	return repo, number, true
}
