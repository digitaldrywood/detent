package hubgithub

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	connectorgithub "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Importer struct{ client restClient }

func NewImporter(client restClient) *Importer { return &Importer{client: client} }

func (i *Importer) FetchImportPage(ctx context.Context, request hubserver.GitHubImportRequest) (result hubserver.GitHubImportPage, resultErr error) {
	defer func() {
		var status *connectorgithub.StatusError
		if errors.As(resultErr, &status) {
			result.RetryAt = time.Now().Add(max(time.Minute, status.RetryAfter))
			if status.ResetAt.After(result.RetryAt) {
				result.RetryAt = status.ResetAt
			}
		}
	}()
	ctx = scopedRequests(ctx, request.Profile, "import")
	if i == nil || i.client == nil {
		return result, errors.New("github importer is not configured")
	}
	if request.Stage == "edits" {
		return i.fetchEdits(ctx, request)
	}
	parts := strings.Split(request.Repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || request.IssueNumber <= 0 {
		return result, errors.New("github import repository and issue number are required")
	}
	path := repositoryRESTPath(parts[0], parts[1]) + "/issues/" + strconv.Itoa(request.IssueNumber)
	if request.Stage == "issue" {
		var raw json.RawMessage
		if err := i.client.REST(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return result, err
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return result, err
		}
		if _, present := fields["body"]; !present {
			return result, errors.New("github issue response omits its body; an excerpt cannot complete import")
		}
		var issue reconcileIssue
		if err := json.Unmarshal(raw, &issue); err != nil {
			return result, err
		}
		if issue.PullRequest != nil {
			return result, errors.New("github import target is a pull request")
		}
		source := issue.source()
		if err := validateIssueSource(source); err != nil {
			return result, err
		}
		result.Issue = &source
		result.Gaps = []string{"GitHub exposes only retained, authorized history. Deleted comments, inaccessible private events and redacted edit diffs may be unavailable. Pagination is not an atomic source snapshot; reimport after quiescing source edits."}
		record, err := importRecord(raw, "issue")
		if err != nil {
			return result, err
		}
		result.Records = []hubserver.GitHubImportRecord{record}
		return result, nil
	}
	var kind string
	switch request.Stage {
	case "comments":
		path += "/comments"
		kind = "comment"
	case "timeline":
		path += "/timeline"
		kind = "timeline"
	case "dependencies":
		path += "/dependencies/blocked_by"
		kind = "dependency"
	default:
		return result, errors.New("unknown github import stage")
	}
	endpoint := path
	path += "?per_page=100"
	if request.Cursor != "" {
		cursor, err := url.Parse(request.Cursor)
		if err != nil || cursor.Path != endpoint || cursor.Fragment != "" || cursor.User != nil {
			return result, errors.New("github import cursor changed endpoint")
		}
		path = request.Cursor
	}
	var records []json.RawMessage
	next, err := i.client.RESTPage(ctx, path, &records)
	if err != nil {
		return result, err
	}
	result.NextCursor = next
	for _, raw := range records {
		record, err := importRecord(raw, kind)
		if err != nil {
			return result, err
		}
		result.Records = append(result.Records, record)
		if record.Kind == "comment" && record.Body == "" {
			result.Gaps = append(result.Gaps, "Comment body was unavailable: "+record.Provenance.ExternalID)
		}
	}
	return result, nil
}

func importRecord(raw json.RawMessage, kind string) (hubserver.GitHubImportRecord, error) {
	var source struct {
		ID        json.RawMessage `json:"id"`
		NodeID    string          `json:"node_id"`
		Body      string          `json:"body"`
		Event     string          `json:"event"`
		CreatedAt time.Time       `json:"created_at"`
		UpdatedAt time.Time       `json:"updated_at"`
		User      struct {
			Login  string `json:"login"`
			NodeID string `json:"node_id"`
		} `json:"user"`
		Actor struct {
			Login  string `json:"login"`
			NodeID string `json:"node_id"`
		} `json:"actor"`
	}
	if err := json.Unmarshal(raw, &source); err != nil {
		return hubserver.GitHubImportRecord{}, err
	}
	id := source.NodeID
	if id == "" && len(source.ID) > 0 && string(source.ID) != "null" {
		id = string(source.ID)
	}
	if id == "" {
		id = fmt.Sprintf("content-sha256:%x", sha256.Sum256(raw))
	}
	if kind == "timeline" && source.Event == "commented" {
		kind = "comment"
	}
	author, name := source.User.NodeID, source.User.Login
	if author == "" {
		author = source.User.Login
	}
	if author == "" {
		author, name = source.Actor.NodeID, source.Actor.Login
	}
	if author == "" {
		author = source.Actor.Login
	}
	if author == "" {
		author = "unavailable"
	}
	updated := source.UpdatedAt
	if updated.IsZero() {
		updated = source.CreatedAt
	}
	record := hubserver.GitHubImportRecord{SourceKey: kind + ":" + id, Kind: kind, Data: raw, Provenance: tracker.Provenance{Provider: "github", ExternalID: id, AuthorID: author, AuthorDisplayName: name, CreatedAt: source.CreatedAt, UpdatedAt: updated, ObservedAt: time.Now().UTC()}}
	if kind == "comment" {
		record.Body = source.Body
	}
	if kind == "dependency" {
		record.DependencyID = source.NodeID
	}
	return record, nil
}
