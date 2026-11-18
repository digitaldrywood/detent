package githubwebhook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Payload struct {
	Sender      *Actor       `json:"sender"`
	Comment     *Comment     `json:"comment"`
	Repository  *Repository  `json:"repository"`
	Issue       *Issue       `json:"issue"`
	PullRequest *PullRequest `json:"pull_request"`
	Review      *Review      `json:"review"`
	CheckRun    *Check       `json:"check_run"`
	CheckSuite  *Check       `json:"check_suite"`
	SHA         *string      `json:"sha"`
}

type Repository struct {
	DefaultBranch *string         `json:"default_branch"`
	DatabaseID    *int64          `json:"id"`
	NodeID        *string         `json:"node_id"`
	Name          *string         `json:"name"`
	FullName      *string         `json:"full_name"`
	Owner         *Actor          `json:"owner"`
	UpdatedAt     json.RawMessage `json:"updated_at"`
}

type Actor struct {
	Login *string `json:"login"`
}

type Comment struct {
	NodeID    string    `json:"node_id"`
	Body      string    `json:"body"`
	User      *Actor    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Issue struct {
	PullRequest json.RawMessage `json:"pull_request"`
	DatabaseID  *int64          `json:"id"`
	NodeID      *string         `json:"node_id"`
	Number      *int            `json:"number"`
	Title       *string         `json:"title"`
	Body        json.RawMessage `json:"body"`
	HTMLURL     *string         `json:"html_url"`
	State       *string         `json:"state"`
	User        *Actor          `json:"user"`
	Labels      json.RawMessage `json:"labels"`
	Assignees   json.RawMessage `json:"assignees"`
	CreatedAt   json.RawMessage `json:"created_at"`
	UpdatedAt   json.RawMessage `json:"updated_at"`
}

type PullRequest struct {
	MergeSHA   *string         `json:"merge_commit_sha"`
	MergedAt   json.RawMessage `json:"merged_at"`
	MergedBy   *Actor          `json:"merged_by"`
	DatabaseID *int64          `json:"id"`
	NodeID     *string         `json:"node_id"`
	Number     *int            `json:"number"`
	Title      *string         `json:"title"`
	HTMLURL    *string         `json:"html_url"`
	State      *string         `json:"state"`
	Draft      *bool           `json:"draft"`
	Head       *Ref            `json:"head"`
	Base       *Ref            `json:"base"`
	CreatedAt  json.RawMessage `json:"created_at"`
	UpdatedAt  json.RawMessage `json:"updated_at"`
}

type Ref struct {
	Ref *string `json:"ref"`
	SHA *string `json:"sha"`
}

type Review struct {
	SubmittedAt json.RawMessage `json:"submitted_at"`
}

type Check struct {
	HeadSHA      *string         `json:"head_sha"`
	PullRequests []PullRequest   `json:"pull_requests"`
	UpdatedAt    json.RawMessage `json:"updated_at"`
	CompletedAt  json.RawMessage `json:"completed_at"`
}

type normalizedIssue struct {
	NodeID     string    `json:"node_id"`
	DatabaseID *int64    `json:"database_id,omitempty"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	AuthorID   string    `json:"author_id"`
	Labels     []string  `json:"labels"`
	Assignees  []string  `json:"assignees"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type normalizedPullRequest struct {
	MergeSHA   string    `json:"merge_sha,omitempty"`
	MergedAt   time.Time `json:"merged_at,omitzero"`
	MergedBy   string    `json:"merged_by,omitempty"`
	NodeID     string    `json:"node_id"`
	DatabaseID *int64    `json:"database_id,omitempty"`
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	State      string    `json:"state"`
	Draft      bool      `json:"draft"`
	HeadRef    string    `json:"head_ref"`
	HeadSHA    string    `json:"head_sha"`
	BaseRef    string    `json:"base_ref"`
	BaseSHA    string    `json:"base_sha"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type sourceStamp struct {
	UpdatedAt time.Time
	Version   string
}

func normalizeWebhookIssue(issue *Issue) (normalizedIssue, sourceStamp, bool) {
	if issue == nil || issue.NodeID == nil || issue.Number == nil || issue.Title == nil || issue.HTMLURL == nil || issue.State == nil {
		return normalizedIssue{}, sourceStamp{}, false
	}
	body, bodyOK := webhookNullableString(issue.Body)
	labels, labelsOK := webhookStringList(issue.Labels, "name")
	assignees, assigneesOK := webhookStringList(issue.Assignees, "login")
	authorID := ""
	if issue.User != nil && issue.User.Login != nil {
		authorID = strings.TrimSpace(*issue.User.Login)
	}
	createdAt, createdOK := parseWebhookTime(issue.CreatedAt)
	updatedAt, updatedOK := parseWebhookTime(issue.UpdatedAt)
	if strings.TrimSpace(*issue.NodeID) == "" || *issue.Number <= 0 || strings.TrimSpace(*issue.Title) == "" ||
		strings.TrimSpace(*issue.HTMLURL) == "" || strings.TrimSpace(*issue.State) == "" ||
		!bodyOK || !labelsOK || !assigneesOK || !createdOK || !updatedOK {
		return normalizedIssue{}, sourceStamp{}, false
	}
	normalized := normalizedIssue{
		NodeID:     strings.TrimSpace(*issue.NodeID),
		DatabaseID: positiveInt64(issue.DatabaseID),
		Number:     *issue.Number,
		Title:      *issue.Title,
		Body:       body,
		URL:        strings.TrimSpace(*issue.HTMLURL),
		State:      strings.ToLower(strings.TrimSpace(*issue.State)),
		AuthorID:   authorID,
		Labels:     labels,
		Assignees:  assignees,
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}
	stamp, err := newSourceStamp(updatedAt, normalized)
	if err != nil {
		return normalizedIssue{}, sourceStamp{}, false
	}
	return normalized, stamp, true
}

func normalizeWebhookPullRequest(pullRequest *PullRequest) (normalizedPullRequest, sourceStamp, bool) {
	if pullRequest == nil || pullRequest.NodeID == nil || pullRequest.Number == nil || pullRequest.Title == nil ||
		pullRequest.HTMLURL == nil || pullRequest.State == nil || pullRequest.Draft == nil || pullRequest.Head == nil || pullRequest.Base == nil ||
		pullRequest.Head.Ref == nil || pullRequest.Head.SHA == nil || pullRequest.Base.Ref == nil || pullRequest.Base.SHA == nil {
		return normalizedPullRequest{}, sourceStamp{}, false
	}
	createdAt, createdOK := parseWebhookTime(pullRequest.CreatedAt)
	updatedAt, updatedOK := parseWebhookTime(pullRequest.UpdatedAt)
	if strings.TrimSpace(*pullRequest.NodeID) == "" || *pullRequest.Number <= 0 || strings.TrimSpace(*pullRequest.Title) == "" ||
		strings.TrimSpace(*pullRequest.HTMLURL) == "" || strings.TrimSpace(*pullRequest.State) == "" ||
		strings.TrimSpace(*pullRequest.Head.Ref) == "" || strings.TrimSpace(*pullRequest.Head.SHA) == "" ||
		strings.TrimSpace(*pullRequest.Base.Ref) == "" || strings.TrimSpace(*pullRequest.Base.SHA) == "" || !createdOK || !updatedOK {
		return normalizedPullRequest{}, sourceStamp{}, false
	}
	normalized := normalizedPullRequest{
		NodeID:     strings.TrimSpace(*pullRequest.NodeID),
		DatabaseID: positiveInt64(pullRequest.DatabaseID),
		Number:     *pullRequest.Number,
		Title:      *pullRequest.Title,
		URL:        strings.TrimSpace(*pullRequest.HTMLURL),
		State:      strings.ToLower(strings.TrimSpace(*pullRequest.State)),
		Draft:      *pullRequest.Draft,
		HeadRef:    strings.TrimSpace(*pullRequest.Head.Ref),
		HeadSHA:    strings.TrimSpace(*pullRequest.Head.SHA),
		BaseRef:    strings.TrimSpace(*pullRequest.Base.Ref),
		BaseSHA:    strings.TrimSpace(*pullRequest.Base.SHA),
		CreatedAt:  createdAt,
		UpdatedAt:  updatedAt,
	}
	if mergedAt, ok := parseWebhookTime(pullRequest.MergedAt); ok {
		normalized.MergedAt = mergedAt
	}
	if pullRequest.MergeSHA != nil {
		normalized.MergeSHA = strings.TrimSpace(*pullRequest.MergeSHA)
	}
	if pullRequest.MergedBy != nil && pullRequest.MergedBy.Login != nil {
		normalized.MergedBy = strings.TrimSpace(*pullRequest.MergedBy.Login)
	}
	stamp, err := newSourceStamp(updatedAt, normalized)
	if err != nil {
		return normalizedPullRequest{}, sourceStamp{}, false
	}
	return normalized, stamp, true
}

func newSourceStamp(updatedAt time.Time, canonical any) (sourceStamp, error) {
	return newSourceStampWithVersion(updatedAt, "1", canonical)
}

func newSourceStampWithVersion(updatedAt time.Time, version string, canonical any) (sourceStamp, error) {
	value, err := json.Marshal(canonical)
	if err != nil {
		return sourceStamp{}, fmt.Errorf("encode GitHub webhook source version: %w", err)
	}
	digest := sha256.Sum256(value)
	return sourceStamp{UpdatedAt: updatedAt.UTC(), Version: version + ":" + hex.EncodeToString(digest[:])}, nil
}

func webhookRepositoryFullName(repository *Repository) string {
	if repository == nil {
		return ""
	}
	if repository.FullName != nil && strings.TrimSpace(*repository.FullName) != "" {
		return strings.TrimSpace(*repository.FullName)
	}
	if repository.Owner == nil || repository.Owner.Login == nil || repository.Name == nil {
		return ""
	}
	owner := strings.TrimSpace(*repository.Owner.Login)
	name := strings.TrimSpace(*repository.Name)
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}

func SplitRepositoryFullName(fullName string) (string, string, bool) {
	parts := strings.Split(strings.TrimSpace(fullName), "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", "", false
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]), true
}

func parseWebhookTime(raw json.RawMessage) (time.Time, bool) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return time.Time{}, false
	}
	var text string
	if err := json.Unmarshal(value, &text); err == nil {
		parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(text))
		return parsed.UTC(), err == nil
	}
	var number json.Number
	if err := json.Unmarshal(value, &number); err != nil {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	if seconds > 1_000_000_000_000 {
		return time.UnixMilli(seconds).UTC(), true
	}
	return time.Unix(seconds, 0).UTC(), true
}

func webhookNullableString(raw json.RawMessage) (string, bool) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 {
		return "", false
	}
	if bytes.Equal(value, []byte("null")) {
		return "", true
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return "", false
	}
	return text, true
}

func webhookStringList(raw json.RawMessage, objectField string) ([]string, bool) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		return nil, false
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(value, &entries); err != nil {
		return nil, false
	}
	values := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var text string
		if err := json.Unmarshal(entry, &text); err != nil {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(entry, &object); err != nil {
				return nil, false
			}
			field, ok := object[objectField]
			if !ok || json.Unmarshal(field, &text) != nil {
				return nil, false
			}
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, false
		}
		if _, ok := seen[text]; ok {
			continue
		}
		seen[text] = struct{}{}
		values = append(values, text)
	}
	sort.Strings(values)
	return values, true
}

func positiveInt64(value *int64) *int64 {
	if value == nil || *value <= 0 {
		return nil
	}
	copy := *value
	return &copy
}
