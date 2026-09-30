package tracker

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var githubIssuePath = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9-]*)/([A-Za-z0-9_.-]+)/issues/([1-9][0-9]*)/?$`)

// ParseGitHubIssueURL accepts only an issue identity, never an API endpoint.
func ParseGitHubIssueURL(raw string) (canonical, repository string, number int, err error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return "", "", 0, errors.New("expected a github.com/OWNER/REPO/issues/NUMBER URL")
	}
	match := githubIssuePath.FindStringSubmatch(u.Path)
	if match == nil || match[2] == "." || match[2] == ".." {
		return "", "", 0, errors.New("expected a github.com/OWNER/REPO/issues/NUMBER URL")
	}
	number, err = strconv.Atoi(match[3])
	if err != nil {
		return "", "", 0, errors.New("github issue number is invalid")
	}
	repository = strings.ToLower(match[1] + "/" + match[2])
	return "https://github.com/" + repository + "/issues/" + strconv.Itoa(number), repository, number, nil
}

// LinkedIssueSource is historical intake evidence, not an ongoing sync.
type LinkedIssueSource struct {
	URL      string               `json:"url"`
	Status   string               `json:"status"`
	Snapshot *GitHubIssueSnapshot `json:"snapshot,omitempty"`
}

type GitHubIssueSnapshot struct {
	URL        string               `json:"url"`
	Title      string               `json:"title"`
	Body       string               `json:"body"`
	Provenance Provenance           `json:"provenance"`
	Comments   []GitHubIssueComment `json:"comments"`
}

type GitHubIssueComment struct {
	Body       string     `json:"body"`
	Provenance Provenance `json:"provenance"`
}

type GitHubIntake struct {
	Mutation
	Snapshot GitHubIssueSnapshot `json:"snapshot"`
}
