package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/tracker"
)

// Pull requests (decisions section 18.6). The panel joins two halves the hub
// already owns: its own change requests, which carry the native reviews and
// checks, and the GitHub connector's projection of the pull request, which
// carries state, draft, head, base and mergeability.
//
// The connector half is the `pull_requests` projection that the webhook ingest
// and the reconciler maintain (`internal/hubgithub`). The hub itself holds no
// per-project GitHub credential and, on a hosted hub, has no GitHub transport
// at all, so this endpoint never calls GitHub: `?refresh=1` asks the
// reconciler to re-fetch by queueing a hydration request, which is the only
// way the hub can make the connector view newer.

const (
	// pullRequestViewTTL is how long an assembled view is served from memory
	// before the join is run again (section 18.6, "caches the connector view
	// for 60 seconds").
	pullRequestViewTTL = 60 * time.Second
	// pullRequestRefreshInterval is the minimum spacing between forced
	// refreshes, per organization.
	pullRequestRefreshInterval = 10 * time.Second
)

// pullRequestMergeable is 18.6's three-valued mergeability: true, false, or
// the string "unknown" when the connector has not said.
type pullRequestMergeable struct {
	Known bool
	Value bool
}

func (m pullRequestMergeable) MarshalJSON() ([]byte, error) {
	if !m.Known {
		return []byte(`"unknown"`), nil
	}
	return json.Marshal(m.Value)
}

func (m *pullRequestMergeable) UnmarshalJSON(data []byte) error {
	var value bool
	if err := json.Unmarshal(data, &value); err == nil {
		m.Known, m.Value = true, value
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil || text != "unknown" {
		return errors.New(`mergeable must be true, false or "unknown"`)
	}
	m.Known, m.Value = false, false
	return nil
}

// mergeableFromState maps GitHub's mergeable_state onto 18.6's three values.
// An empty or "unknown" state is unknown; "dirty" is the only state that means
// the branch cannot merge; everything else is mergeable as far as the
// projection can tell.
func mergeableFromState(state string) pullRequestMergeable {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "unknown":
		return pullRequestMergeable{}
	case "dirty":
		return pullRequestMergeable{Known: true}
	default:
		return pullRequestMergeable{Known: true, Value: true}
	}
}

type pullRequestRef struct {
	Ref        string `json:"ref"`
	SHA        string `json:"sha,omitempty"`
	Repository string `json:"repository,omitempty"`
}

type pullRequestCheck struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"`
	Conclusion  string     `json:"conclusion"`
	URL         string     `json:"url"`
	CompletedAt *time.Time `json:"completed_at"`
}

type pullRequestReview struct {
	Author      string    `json:"author"`
	State       string    `json:"state"`
	SubmittedAt time.Time `json:"submitted_at"`
}

type pullRequestConnector struct {
	Provider       string    `json:"provider"`
	Repository     string    `json:"repository"`
	SynchronizedAt time.Time `json:"synchronized_at"`
}

// pullRequestView is one row of the panel: a change request, joined with the
// connector's projection of its pull request when there is one.
type pullRequestView struct {
	ID             string                `json:"id"`
	ChangeID       string                `json:"change_id"`
	Number         int                   `json:"number"`
	Title          string                `json:"title"`
	State          string                `json:"state"`
	Draft          bool                  `json:"draft"`
	URL            string                `json:"url"`
	Head           pullRequestRef        `json:"head"`
	Base           pullRequestRef        `json:"base"`
	Author         string                `json:"author"`
	FromFork       bool                  `json:"from_fork"`
	Mergeable      pullRequestMergeable  `json:"mergeable"`
	Checks         []pullRequestCheck    `json:"checks"`
	Reviews        []pullRequestReview   `json:"reviews"`
	ReviewDecision string                `json:"review_decision"`
	Labels         []string              `json:"labels"`
	UpdatedAt      time.Time             `json:"updated_at"`
	FetchedAt      time.Time             `json:"fetched_at"`
	Connector      *pullRequestConnector `json:"connector"`
}

// pullRequestCache serves an assembled view for pullRequestViewTTL and paces
// forced refreshes per organization. Both are in-process: a restart loses a
// cached view, which costs one extra join, and loses a refresh budget, which
// costs at most one extra hydration request.
type pullRequestCache struct {
	mu        sync.Mutex
	views     map[string]pullRequestCacheEntry
	refreshed map[tracker.OrganizationID]time.Time
}

type pullRequestCacheEntry struct {
	views    []pullRequestView
	cachedAt time.Time
}

func newPullRequestCache() *pullRequestCache {
	return &pullRequestCache{views: map[string]pullRequestCacheEntry{}, refreshed: map[tracker.OrganizationID]time.Time{}}
}

func pullRequestCacheKey(scope nativeScope, item string) string {
	return string(scope.organization) + "/" + string(scope.project) + "/" + item
}

// get returns a cached view that is still fresh. The slice is cloned so a
// caller cannot rewrite what the next reader sees.
func (c *pullRequestCache) get(key string, now time.Time) ([]pullRequestView, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.views[key]
	if !ok || now.Sub(entry.cachedAt) >= pullRequestViewTTL {
		return nil, false
	}
	return append([]pullRequestView(nil), entry.views...), true
}

func (c *pullRequestCache) set(key string, views []pullRequestView, now time.Time) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The cache is bounded by dropping everything once it grows past a page
	// of work items: the entries are cheap to rebuild and a hub that is
	// browsing thousands of issues should not hold them all.
	if len(c.views) >= 512 {
		clear(c.views)
	}
	c.views[key] = pullRequestCacheEntry{views: append([]pullRequestView(nil), views...), cachedAt: now}
}

// allowRefresh reports whether the organization may force a refresh now, and
// records the grant when it may.
func (c *pullRequestCache) allowRefresh(organization tracker.OrganizationID, now time.Time) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.refreshed[organization]; ok && now.Sub(last) < pullRequestRefreshInterval {
		return false
	}
	c.refreshed[organization] = now
	return true
}

func pullRequestRefreshLimited() error {
	return &nativeError{
		Code: "refresh_limited", Message: "Pull request refresh is limited to one per organization every ten seconds",
		status:  http.StatusTooManyRequests,
		Details: map[string]any{"retry_after_seconds": int(pullRequestRefreshInterval / time.Second)},
	}
}

// Reads.

// projectConnector reads the project's GitHub connector binding. It answers a
// nil connector for a project that has no repository bound or has the
// repository disabled, which is 18.6's "projects without a GitHub connector".
func projectConnector(ctx context.Context, query nativeQueryer, scope nativeScope) (*pullRequestConnector, int64, error) {
	var enabled bool
	var repositoryID sql.NullInt64
	var owner, name sql.NullString
	err := query.QueryRowContext(ctx, `SELECT p.github_repository_enabled, p.repository_id, r.github_owner, r.github_name
FROM projects p LEFT JOIN repositories r ON r.id = p.repository_id
WHERE p.organization_id = ? AND p.id = ?`, scope.organization, scope.project).Scan(&enabled, &repositoryID, &owner, &name)
	if err != nil {
		return nil, 0, err
	}
	if !enabled || !repositoryID.Valid || !owner.Valid || !name.Valid {
		return nil, 0, nil
	}
	return &pullRequestConnector{Provider: "github", Repository: owner.String + "/" + name.String}, repositoryID.Int64, nil
}

// readConnectorPullRequest reads the projection row for one external URL. The
// join is by URL because that is the only identity a change version records
// for its pull request.
func readConnectorPullRequest(ctx context.Context, query nativeQueryer, repositoryID int64, url string) (tracker.PullRequestSummary, time.Time, bool, error) {
	var summary tracker.PullRequestSummary
	var checksJSON, reviewsJSON, synchronized string
	err := query.QueryRowContext(ctx, `SELECT github_number, title, url, github_state, draft, head_ref, head_sha, base_ref, base_sha,
mergeable_state, checks_summary_json, reviews_summary_json, synchronized_at
FROM pull_requests WHERE repository_id = ? AND url = ?`, repositoryID, url).Scan(
		&summary.Number, &summary.Title, &summary.URL, &summary.State, &summary.Draft, &summary.HeadRef, &summary.HeadSHA,
		&summary.BaseRef, &summary.BaseSHA, &summary.Merge.State, &checksJSON, &reviewsJSON, &synchronized)
	if errors.Is(err, sql.ErrNoRows) {
		return summary, time.Time{}, false, nil
	}
	if err != nil {
		return summary, time.Time{}, false, fmt.Errorf("read connector pull request: %w", err)
	}
	if err := json.Unmarshal([]byte(checksJSON), &summary.Checks); err != nil {
		return summary, time.Time{}, false, fmt.Errorf("decode connector checks summary: %w", err)
	}
	if err := json.Unmarshal([]byte(reviewsJSON), &summary.Reviews); err != nil {
		return summary, time.Time{}, false, fmt.Errorf("decode connector reviews summary: %w", err)
	}
	fetched, err := parseTimeValue(synchronized)
	if err != nil {
		return summary, time.Time{}, false, fmt.Errorf("decode connector synchronized_at: %w", err)
	}
	return summary, fetched, true, nil
}

// changePullRequestView assembles one row from a change request and, when the
// project has a connector and the change names a pull request, the projection.
func (s *Service) changePullRequestView(ctx context.Context, scope nativeScope, item string, change tracker.ChangeRequest,
	connector *pullRequestConnector, repositoryID int64, now time.Time) (pullRequestView, error) {
	detail, err := readChangeDetail(ctx, s.database.db, scope, item, change.ID, now)
	if err != nil {
		return pullRequestView{}, err
	}
	view := pullRequestView{
		ID: change.ID, ChangeID: change.ID, Title: change.Title, State: "open",
		Head: pullRequestRef{}, Base: pullRequestRef{}, Checks: []pullRequestCheck{}, Reviews: []pullRequestReview{},
		Labels: []string{}, UpdatedAt: change.UpdatedAt, FetchedAt: change.UpdatedAt,
	}
	var current *tracker.ChangeVersion
	for index := range detail.Versions {
		if detail.Versions[index].ID == change.CurrentVersion {
			current = &detail.Versions[index]
		}
	}
	if current != nil {
		view.Head.SHA, view.Base.SHA = current.HeadSHA, current.BaseSHA
		view.Head.Repository = current.Repository
		view.Author = current.Actor.PrincipalID
	}
	for _, check := range detail.Checks {
		status := "in_progress"
		var completed *time.Time
		if !check.CompletedAt.IsZero() {
			status = "completed"
			at := check.CompletedAt
			completed = &at
		}
		name := check.WorkflowID
		if strings.TrimSpace(name) == "" {
			name = check.CheckRunID
		}
		url := ""
		if len(check.Evidence) > 0 {
			url = check.Evidence[0].URI
		}
		view.Checks = append(view.Checks, pullRequestCheck{Name: name, Status: status, Conclusion: check.Conclusion, URL: url, CompletedAt: completed})
	}
	for _, review := range detail.Reviews {
		view.Reviews = append(view.Reviews, pullRequestReview{Author: review.Actor.PrincipalID, State: review.Decision, SubmittedAt: review.CreatedAt})
	}
	view.ReviewDecision = detail.Summary.NativeReview
	if connector == nil || current == nil || current.External == nil {
		return view, nil
	}
	summary, fetched, found, err := readConnectorPullRequest(ctx, s.database.db, repositoryID, current.External.URL)
	if err != nil {
		return view, err
	}
	if !found {
		// The change names a pull request the projection has not seen yet.
		// The connector is reported, so the client shows the panel rather
		// than the unavailable state, and the rest stays native.
		bound := *connector
		view.Connector = &bound
		view.URL = current.External.URL
		return view, nil
	}
	bound := *connector
	bound.SynchronizedAt = fetched
	view.Connector = &bound
	view.Number, view.URL, view.Draft = summary.Number, summary.URL, summary.Draft
	view.State = connectorPullRequestState(summary.State)
	view.Head.Ref, view.Head.SHA = summary.HeadRef, summary.HeadSHA
	view.Base.Ref = summary.BaseRef
	if summary.BaseSHA != "" {
		view.Base.SHA = summary.BaseSHA
	}
	// The projection records no head repository and no fork flag, so a pull
	// request the hub can see is always on the bound repository.
	view.Head.Repository = connector.Repository
	view.Mergeable = mergeableFromState(summary.Merge.State)
	if summary.Reviews.Decision != "" {
		view.ReviewDecision = summary.Reviews.Decision
	}
	if summary.Title != "" {
		view.Title = summary.Title
	}
	view.FetchedAt = fetched
	return view, nil
}

// connectorPullRequestState normalizes GitHub's state onto 18.6's vocabulary.
func connectorPullRequestState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "merged":
		return "merged"
	case "closed":
		return "closed"
	default:
		return "open"
	}
}

// listWorkItemPullRequests answers the panel. It follows the issue's read
// rule: the item is resolved in the caller's scope first, so a reader who
// cannot see the issue cannot see its pull requests.
func (s *Service) listWorkItemPullRequests(c echo.Context) error {
	if err := validateNativeQuery(c.QueryParams(), "refresh"); err != nil {
		return s.nativeAPIError(c, err)
	}
	refresh := strings.TrimSpace(c.QueryParam("refresh"))
	if refresh != "" && refresh != "1" {
		return s.nativeAPIError(c, nativeInvalid("refresh accepts only 1"))
	}
	scope := nativeRequestScope(c)
	ctx := c.Request().Context()
	item := c.Param("item")
	if _, _, err := readNativeIssue(ctx, s.database.db, scope, item); err != nil {
		return s.nativeAPIError(c, err)
	}
	now := s.config.now()
	key := pullRequestCacheKey(scope, item)
	if refresh == "" {
		if cached, ok := s.pullRequests.get(key, now); ok {
			return c.JSON(http.StatusOK, cached)
		}
	} else if !s.pullRequests.allowRefresh(scope.organization, now) {
		return s.nativeAPIError(c, pullRequestRefreshLimited())
	}
	connector, repositoryID, err := projectConnector(ctx, s.database.db, scope)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	changes, err := changeRows[tracker.ChangeRequest](ctx, s.database.db, `SELECT c.record_json FROM change_requests c
JOIN change_issue_links l ON l.change_id = c.id
WHERE c.organization_id = ? AND c.project_id = ? AND l.work_item_id = ? ORDER BY c.rowid`, scope.organization, scope.project, item)
	if err != nil {
		return s.nativeAPIError(c, err)
	}
	views := []pullRequestView{}
	for _, change := range changes {
		view, err := s.changePullRequestView(ctx, scope, item, change, connector, repositoryID, now)
		if err != nil {
			return s.nativeAPIError(c, err)
		}
		views = append(views, view)
	}
	if refresh == "1" && connector != nil {
		if err := s.requestPullRequestHydration(ctx, repositoryID, views, now); err != nil {
			return s.nativeAPIError(c, err)
		}
	}
	s.pullRequests.set(key, views, now)
	return c.JSON(http.StatusOK, views)
}

// requestPullRequestHydration asks the reconciler to re-fetch every pull
// request the panel showed. The hub holds no GitHub credential of its own, so
// this queue is the only way it can make the connector view newer; the request
// is coalesced by the table's own uniqueness, so a burst of refreshes costs one
// fetch.
func (s *Service) requestPullRequestHydration(ctx context.Context, repositoryID int64, views []pullRequestView, now time.Time) error {
	if repositoryID == 0 {
		return nil
	}
	return s.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
		var fullName string
		if err := tx.QueryRowContext(ctx, "SELECT github_owner || '/' || github_name FROM repositories WHERE id = ?", repositoryID).Scan(&fullName); err != nil {
			return fmt.Errorf("read repository for pull request refresh: %w", err)
		}
		for _, view := range views {
			if view.Number <= 0 {
				continue
			}
			target := hydrationTarget{
				RepositoryID: &repositoryID, RepositoryFullName: fullName,
				ObjectKind: "pull_request", ObjectKey: strconv.Itoa(view.Number),
				GitHubNumber: view.Number, HeadSHA: view.Head.SHA, Reason: "client_refresh",
				Source: sourceStamp{UpdatedAt: view.UpdatedAt, Version: formatHubTime(now)},
			}
			if err := enqueueHydration(ctx, tx, "", target, now); err != nil {
				return err
			}
		}
		return nil
	})
}
