package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type IssueExposure struct {
	Repository        string `json:"repository"`
	Number            int    `json:"number"`
	URL               string `json:"url"`
	MatchedIdentifier string `json:"matched_identifier"`
}

func (c *Connector) ScanIssueExposure(ctx context.Context, repository string, identifiers []string) ([]IssueExposure, error) {
	repository = strings.TrimSpace(repository)
	if _, ok := pullRequestRepoFromName(repository); !ok {
		return nil, fmt.Errorf("invalid GitHub repository %q", repository)
	}
	seen := map[string]IssueExposure{}
	for _, identifier := range normalizedExposureIdentifiers(identifiers) {
		for page := 1; ; page++ {
			var response restIssueSearchResponse
			if err := c.client.REST(ctx, http.MethodGet, restIssueExposureSearchPath(repository, identifier, page), nil, &response); err != nil {
				return nil, fmt.Errorf("scan github issue exposure for %s: %w", identifier, err)
			}
			for _, item := range response.Items {
				if item.PullRequest != nil || item.Number <= 0 {
					continue
				}
				key := repository + "#" + strconv.Itoa(item.Number) + "\x00" + identifier
				seen[key] = IssueExposure{
					Repository:        repository,
					Number:            item.Number,
					URL:               strings.TrimSpace(item.HTMLURL),
					MatchedIdentifier: identifier,
				}
			}
			if len(response.Items) == 0 || page*intakeIssueSearchPageSize >= response.TotalCount {
				break
			}
		}
	}
	out := make([]IssueExposure, 0, len(seen))
	for _, finding := range seen {
		out = append(out, finding)
	}
	sort.Slice(out, func(i int, j int) bool {
		if out[i].Repository != out[j].Repository {
			return out[i].Repository < out[j].Repository
		}
		if out[i].Number != out[j].Number {
			return out[i].Number < out[j].Number
		}
		return out[i].MatchedIdentifier < out[j].MatchedIdentifier
	})
	return out, nil
}

func normalizedExposureIdentifiers(identifiers []string) []string {
	out := make([]string, 0, len(identifiers))
	seen := map[string]struct{}{}
	for _, identifier := range identifiers {
		identifier = strings.TrimSpace(identifier)
		key := strings.ToLower(identifier)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, identifier)
	}
	sort.Strings(out)
	return out
}

func restIssueExposureSearchPath(repository string, identifier string, page int) string {
	values := url.Values{}
	values.Set("q", "repo:"+repository+" is:issue in:body,comments \""+identifier+"\"")
	values.Set("per_page", strconv.Itoa(intakeIssueSearchPageSize))
	values.Set("page", strconv.Itoa(page))
	return "/search/issues?" + values.Encode()
}
