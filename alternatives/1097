package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// One registry combines clients that use the same credential, including the
// schedule coordinator's separate client and the tracker clients of each project.
var defaultGraphQLAttribution = &graphQLAttributionRegistry{windows: make(map[string]*graphQLAttributionWindow)}

type graphQLAttributionRegistry struct {
	mu      sync.Mutex
	windows map[string]*graphQLAttributionWindow
	closed  map[string]time.Time
}

type graphQLAttributionWindow struct {
	fingerprint     string
	resetAt         time.Time
	firstUsed       int64
	lastUsed        int64
	byPurpose       map[graphQLAttributionKey]graphQLAttributionEntry
	logger          *slog.Logger
	timer           *time.Timer
	probeTimer      *time.Timer
	client          *Client
	providerUsed    int64
	hasProviderUsed bool
}

type graphQLAttributionKey struct {
	project string
	purpose string
}

type graphQLAttributionEntry struct {
	Project           string `json:"project"`
	Purpose           string `json:"purpose"`
	Queries           int64  `json:"queries"`
	Cost              int64  `json:"cost"`
	UnmeasuredQueries int64  `json:"unmeasured_queries"`
}

type graphQLCostSnapshot struct {
	Used     int64
	Cost     int64
	ResetAt  time.Time
	Measured bool
}

func graphQLAttributionCost(data json.RawMessage, headers graphQLHeaderRateLimit) (int64, graphQLCostSnapshot, bool) {
	var response struct {
		RateLimit *struct {
			Used    *int64    `json:"used"`
			Cost    *int64    `json:"cost"`
			ResetAt time.Time `json:"resetAt"`
		} `json:"rateLimit"`
	}
	if json.Unmarshal(data, &response) == nil && response.RateLimit != nil && response.RateLimit.Cost != nil {
		snapshot := graphQLCostSnapshot{Cost: *response.RateLimit.Cost, ResetAt: response.RateLimit.ResetAt, Measured: true}
		if response.RateLimit.Used != nil {
			snapshot.Used = *response.RateLimit.Used
		}
		if snapshot.ResetAt.IsZero() && headers.HasCurrent {
			snapshot.ResetAt = headers.Current.ResetAt
		}
		if response.RateLimit.Used == nil && headers.HasCurrent {
			snapshot.Used = headers.Current.Used
		}
		return snapshot.Cost, snapshot, true
	}
	if !headers.HasCurrent || !headers.HasPrimarySnapshot || headers.Current.ResetAt.IsZero() {
		return 0, graphQLCostSnapshot{}, false
	}
	if !headers.HasPrevious || !headers.Current.ResetAt.Equal(headers.Previous.ResetAt) {
		return 0, graphQLCostSnapshot{Used: headers.Current.Used, ResetAt: headers.Current.ResetAt}, true
	}
	cost := headers.Current.Used - headers.Previous.Used
	if cost < 0 {
		return 0, graphQLCostSnapshot{}, false
	}
	return cost, graphQLCostSnapshot{Used: headers.Current.Used, Cost: cost, ResetAt: headers.Current.ResetAt, Measured: true}, true
}

func (r *graphQLAttributionRegistry) record(ctx context.Context, token, project, queryType string, cost int64, snapshot graphQLCostSnapshot, logger *slog.Logger, client *Client) {
	if !snapshot.ResetAt.After(time.Now().Add(-5*time.Second)) || cost < 0 {
		return
	}
	fingerprint := graphQLCredentialFingerprint(token, client)
	if project == "" {
		project = "unknown"
	}
	purpose := graphQLPurpose(queryType)
	r.mu.Lock()
	if r.windows == nil {
		r.windows = make(map[string]*graphQLAttributionWindow)
	}
	if r.closed == nil {
		r.closed = make(map[string]time.Time)
	}
	for key, closedAt := range r.closed {
		if time.Since(closedAt) > 2*time.Hour {
			delete(r.closed, key)
		}
	}
	if !snapshot.ResetAt.After(r.closed[fingerprint]) {
		r.mu.Unlock()
		return
	}
	window := r.windows[fingerprint]
	var completed *graphQLAttributionWindow
	if window != nil && snapshot.ResetAt.Before(window.resetAt) {
		r.mu.Unlock()
		return
	}
	if window != nil && !window.resetAt.Equal(snapshot.ResetAt) {
		delete(r.windows, fingerprint)
		window.timer.Stop()
		if window.probeTimer != nil {
			window.probeTimer.Stop()
		}
		r.closed[fingerprint] = window.resetAt
		completed = window
		window = nil
	}
	if window == nil {
		window = &graphQLAttributionWindow{
			fingerprint: fingerprint, resetAt: snapshot.ResetAt,
			firstUsed: max(0, snapshot.Used-cost), lastUsed: snapshot.Used,
			byPurpose: make(map[graphQLAttributionKey]graphQLAttributionEntry), logger: logger, client: client,
		}
		r.windows[fingerprint] = window
		window.timer = time.AfterFunc(max(time.Until(snapshot.ResetAt.Add(10*time.Second)), time.Second), func() { r.finish(fingerprint, snapshot.ResetAt) })
		if client != nil && client.project != "" && time.Until(snapshot.ResetAt) > 30*time.Second && time.Until(snapshot.ResetAt) < 2*time.Hour {
			window.probeTimer = time.AfterFunc(time.Until(snapshot.ResetAt.Add(-5*time.Second)), func() { r.probe(context.WithoutCancel(ctx), fingerprint, snapshot.ResetAt) })
		}
	}
	if client != nil && client.project != "" {
		window.client = client
	}
	window.firstUsed = min(window.firstUsed, max(0, snapshot.Used-cost))
	window.lastUsed = max(window.lastUsed, snapshot.Used)
	key := graphQLAttributionKey{project: project, purpose: purpose}
	entry := window.byPurpose[key]
	entry.Project, entry.Purpose = project, purpose
	entry.Queries++
	entry.Cost += cost
	if !snapshot.Measured {
		entry.UnmeasuredQueries++
	}
	window.byPurpose[key] = entry
	r.mu.Unlock()
	if completed != nil {
		r.log(completed)
	}
}

func (r *graphQLAttributionRegistry) probe(ctx context.Context, fingerprint string, resetAt time.Time) {
	r.mu.Lock()
	window := r.windows[fingerprint]
	if window == nil || !window.resetAt.Equal(resetAt) || window.client == nil {
		r.mu.Unlock()
		return
	}
	client := window.client
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	token, err := client.tokenSource.Token(ctx)
	if err != nil {
		return
	}
	if graphQLCredentialFingerprint(token, client) != fingerprint {
		return
	}
	var response struct {
		Resources struct {
			GraphQL struct {
				Used  int64 `json:"used"`
				Reset int64 `json:"reset"`
			} `json:"graphql"`
		} `json:"resources"`
	}
	if err := client.REST(ctx, http.MethodGet, "/rate_limit", nil, &response); err != nil {
		return
	}
	if response.Resources.GraphQL.Reset != resetAt.Unix() {
		return
	}
	r.mu.Lock()
	if current := r.windows[fingerprint]; current != nil && current.resetAt.Equal(resetAt) {
		current.providerUsed = response.Resources.GraphQL.Used
		current.hasProviderUsed = true
	}
	r.mu.Unlock()
}

func graphQLCredentialFingerprint(token string, client *Client) string {
	identity := strings.TrimSpace(token)
	if client != nil {
		// Installation identity survives token rotation within a rate-limit window.
		if source, ok := client.tokenSource.(CredentialIdentitySource); ok {
			if stable := strings.TrimSpace(source.CredentialIdentity(token)); stable != "" {
				identity = stable
			}
		}
		identity = client.restEndpoint + "\x00" + identity
	}
	digest := sha256.Sum256([]byte(identity))
	return "github-graphql:" + hex.EncodeToString(digest[:8])
}

func (r *graphQLAttributionRegistry) finish(fingerprint string, resetAt time.Time) {
	r.mu.Lock()
	window := r.windows[fingerprint]
	if window == nil || !window.resetAt.Equal(resetAt) {
		r.mu.Unlock()
		return
	}
	delete(r.windows, fingerprint)
	if window.probeTimer != nil {
		window.probeTimer.Stop()
	}
	if window.timer != nil {
		window.timer.Stop()
	}
	if r.closed == nil {
		r.closed = make(map[string]time.Time)
	}
	r.closed[fingerprint] = resetAt
	r.mu.Unlock()
	r.log(window)
}

func (r *graphQLAttributionRegistry) log(window *graphQLAttributionWindow) {
	if window.logger == nil {
		return
	}
	entries := make([]graphQLAttributionEntry, 0, len(window.byPurpose))
	var attributed int64
	var queries int64
	for _, entry := range window.byPurpose {
		entries = append(entries, entry)
		attributed += entry.Cost
		queries += entry.Queries
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Project != entries[j].Project {
			return entries[i].Project < entries[j].Project
		}
		return entries[i].Purpose < entries[j].Purpose
	})
	providerUsed := window.lastUsed
	source := "graphql"
	if window.hasProviderUsed {
		providerUsed, source = max(window.lastUsed, window.providerUsed), "rest"
	}
	providerDelta := max(0, providerUsed-window.firstUsed)
	unattributed := max(0, providerDelta-attributed)
	if unattributed > 0 {
		entries = append(entries, graphQLAttributionEntry{Project: "unknown", Purpose: "unattributed", Cost: unattributed})
	}
	window.logger.Info("github graphql hourly attribution",
		"credential_fingerprint", window.fingerprint,
		"reset_at", window.resetAt,
		"by_project_purpose", entries,
		"query_count", queries,
		"attributed_cost", attributed,
		"total_cost", attributed+unattributed,
		"rate_limit_used_delta", providerDelta,
		"rate_limit_source", source,
		"unattributed_cost", unattributed,
	)
}

func graphQLPurpose(queryType string) string {
	queryType = strings.ToLower(strings.TrimSpace(queryType))
	switch {
	case strings.Contains(queryType, "coordination_lane"), strings.Contains(queryType, "lane_coordination"):
		return "lane_coordination"
	case strings.Contains(queryType, "coordination"), strings.Contains(queryType, "schedule_ownership"):
		return "schedule_ownership"
	case strings.Contains(queryType, "question"), strings.Contains(queryType, "comment"):
		return "questions"
	case strings.Contains(queryType, "audit"):
		return "audit"
	case strings.Contains(queryType, "merge"), strings.Contains(queryType, "pull_request"), strings.Contains(queryType, "enqueue"), strings.Contains(queryType, "dequeue"):
		return "merge"
	case strings.Contains(queryType, "field"), strings.Contains(queryType, "label"), strings.Contains(queryType, "reconcile"), strings.Contains(queryType, "status"):
		return "reconciliation"
	case queryType == "rate_limit_probe":
		return "audit"
	default:
		return "tracker_fetch"
	}
}
