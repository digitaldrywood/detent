package runnerauth

import (
	"errors"
	"regexp"
	"time"
)

const RepositoryListLimit = 2000

type Repository struct {
	Owner    string     `json:"owner"`
	Name     string     `json:"name"`
	Private  bool       `json:"private"`
	PushedAt *time.Time `json:"pushed_at"`
}

type RepositoryList struct {
	RequestID    string       `json:"request_id"`
	Repositories []Repository `json:"repositories"`
	Reason       string       `json:"reason,omitempty"`
	ListedAt     string       `json:"listed_at,omitempty"`
	Pending      bool         `json:"pending"`
}

var repositoryPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (r RepositoryList) Validate() error {
	if r.RequestID == "" || len(r.RequestID) > 128 || len(r.Repositories) > RepositoryListLimit {
		return errors.New("invalid runner repository report")
	}
	switch r.Reason {
	case "", "no GitHub CLI sign-in", "repository listing unavailable":
	default:
		return errors.New("invalid runner repository report reason")
	}
	seen := make(map[string]bool)
	for _, repo := range r.Repositories {
		key := repo.Owner + "/" + repo.Name
		if len(repo.Owner) > 100 || len(repo.Name) > 100 || !repositoryPart.MatchString(repo.Owner) || !repositoryPart.MatchString(repo.Name) || seen[key] {
			return errors.New("invalid runner repository entry")
		}
		seen[key] = true
	}
	return nil
}
