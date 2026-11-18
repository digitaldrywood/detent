package statuspage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestClientFetchStrictStatuspageFeeds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		mutate       func(map[string]any, map[string]any)
		redirect     bool
		wantErr      string
		wantIncident string
	}{
		{name: "valid feeds", wantIncident: "GitHub service disruption"},
		{name: "redirects followed", redirect: true, wantIncident: "GitHub service disruption"},
		{
			name: "unknown field rejected",
			mutate: func(summary map[string]any, _ map[string]any) {
				summary["unexpected"] = true
			},
			wantErr: "unknown field",
		},
		{
			name: "wrong field type rejected",
			mutate: func(_ map[string]any, unresolved map[string]any) {
				unresolved["incidents"] = "open"
			},
			wantErr: "cannot unmarshal",
		},
		{
			name: "missing required metadata rejected",
			mutate: func(summary map[string]any, _ map[string]any) {
				summary["page"].(map[string]any)["id"] = ""
			},
			wantErr: "page metadata is invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			summary, unresolved := statuspagePayloads(t, "identified", []string{"API Requests", "Issues"}, true)
			if tt.mutate != nil {
				tt.mutate(summary, unresolved)
			}
			target := newStatuspageServer(t, summary, unresolved, nil)
			baseURL := target.URL
			if tt.redirect {
				redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, target.URL+r.URL.Path, http.StatusMovedPermanently)
				}))
				t.Cleanup(redirect.Close)
				baseURL = redirect.URL
			}
			report, err := NewClient(ClientConfig{}).Fetch(context.Background(), SourceForTracker("detent", "github", baseURL))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Fetch() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Fetch() error = %v", err)
			}
			if len(report.Incidents) != 1 || report.Incidents[0].Name != tt.wantIncident {
				t.Fatalf("Fetch().Incidents = %#v, want %q", report.Incidents, tt.wantIncident)
			}
		})
	}
}

func TestManagerCorroboratesWithoutControllingCondition(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	tests := []struct {
		name               string
		incidentStatus     string
		incidentComponents []string
		componentsHealthy  bool
		fetchErr           error
		malformed          bool
		projectCount       int
		wantState          string
		wantStatus         string
		wantComponents     []string
		wantRequests       int
	}{
		{
			name:               "incident names links and mitigation",
			incidentStatus:     "identified",
			incidentComponents: []string{"API Requests", "Issues"},
			componentsHealthy:  true,
			projectCount:       1,
			wantState:          telemetry.ProviderStatusCorroborated,
			wantStatus:         "mitigating",
			wantComponents:     []string{"API Requests", "Issues"},
			wantRequests:       2,
		},
		{
			name:               "operational summary still trusts open incident",
			incidentStatus:     "investigating",
			incidentComponents: []string{"API Requests"},
			componentsHealthy:  true,
			projectCount:       1,
			wantState:          telemetry.ProviderStatusCorroborated,
			wantStatus:         "investigating",
			wantComponents:     []string{"API Requests"},
			wantRequests:       2,
		},
		{
			name:               "unrelated incident does not match",
			incidentStatus:     "investigating",
			incidentComponents: []string{"Pages"},
			componentsHealthy:  true,
			projectCount:       1,
			wantState:          telemetry.ProviderStatusNoMatch,
			wantRequests:       2,
		},
		{
			name:         "unreachable page is non fatal",
			fetchErr:     errors.New("network unavailable"),
			projectCount: 1,
			wantState:    telemetry.ProviderStatusUnavailable,
		},
		{
			name:               "malformed payload is non fatal",
			incidentStatus:     "identified",
			incidentComponents: []string{"API Requests"},
			malformed:          true,
			projectCount:       1,
			wantState:          telemetry.ProviderStatusUnavailable,
			wantRequests:       1,
		},
		{
			name:               "shared provider is polled once per endpoint",
			incidentStatus:     "identified",
			incidentComponents: []string{"API Requests", "Issues"},
			componentsHealthy:  true,
			projectCount:       3,
			wantState:          telemetry.ProviderStatusCorroborated,
			wantStatus:         "mitigating",
			wantComponents:     []string{"API Requests", "Issues"},
			wantRequests:       2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var fetcher Fetcher
			var counts requestCounts
			baseURL := "https://status.example.com"
			if tt.fetchErr != nil {
				fetcher = errorFetcher{err: tt.fetchErr}
			} else {
				summary, unresolved := statuspagePayloads(t, tt.incidentStatus, tt.incidentComponents, tt.componentsHealthy)
				if tt.malformed {
					summary["unexpected"] = true
				}
				server := newStatuspageServer(t, summary, unresolved, &counts)
				baseURL = server.URL
				fetcher = NewClient(ClientConfig{})
			}
			manager := NewManager(ManagerConfig{}, ManagerDependencies{
				Fetcher: fetcher,
				Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			sources := make([]Source, 0, tt.projectCount)
			conditions := make([]telemetry.TrackerCondition, 0, tt.projectCount)
			for index := range tt.projectCount {
				projectID := "project-" + string(rune('a'+index))
				sources = append(sources, SourceForTracker(projectID, "github", baseURL))
				conditions = append(conditions, telemetry.TrackerCondition{
					ProjectID:  projectID,
					Connector:  "github",
					ErrorClass: "server",
				})
			}
			manager.Poll(context.Background(), sources, conditions, now)
			snapshot := manager.Enrich(telemetry.Snapshot{TrackerUnavailable: append([]telemetry.TrackerCondition(nil), conditions...)}, sources)
			if len(snapshot.TrackerUnavailable) != tt.projectCount {
				t.Fatalf("Enrich() conditions = %d, want %d", len(snapshot.TrackerUnavailable), tt.projectCount)
			}
			for _, condition := range snapshot.TrackerUnavailable {
				if condition.ProviderStatus == nil || condition.ProviderStatus.State != tt.wantState {
					t.Fatalf("Enrich().ProviderStatus = %#v, want state %q", condition.ProviderStatus, tt.wantState)
				}
				if condition.ErrorClass != "server" {
					t.Fatalf("Enrich() changed Detent condition: %#v", condition)
				}
				if tt.wantStatus == "" {
					if condition.ProviderStatus.Incident != nil {
						t.Fatalf("Enrich().Incident = %#v, want nil", condition.ProviderStatus.Incident)
					}
					continue
				}
				incident := condition.ProviderStatus.Incident
				if incident == nil || incident.Name != "GitHub service disruption" || incident.URL != "https://stspg.io/example" || incident.Status != tt.wantStatus || !equalStrings(incident.Components, tt.wantComponents) {
					t.Fatalf("Enrich().Incident = %#v", incident)
				}
			}
			if got := counts.total(); got != tt.wantRequests {
				t.Fatalf("provider requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestManagerPollingIntervals(t *testing.T) {
	t.Parallel()

	summary, unresolved := statuspagePayloads(t, "identified", []string{"API Requests"}, true)
	var counts requestCounts
	server := newStatuspageServer(t, summary, unresolved, &counts)
	manager := NewManager(ManagerConfig{
		BaselineInterval: 30 * time.Minute,
		ActiveInterval:   time.Minute,
	}, ManagerDependencies{
		Fetcher: NewClient(ClientConfig{}),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	source := SourceForTracker("detent", "github", server.URL)
	condition := telemetry.TrackerCondition{ProjectID: "detent", Connector: "github"}
	start := time.Date(2026, 8, 17, 17, 20, 0, 0, time.UTC)
	tests := []struct {
		name       string
		at         time.Time
		conditions []telemetry.TrackerCondition
		want       int
	}{
		{name: "initial baseline", at: start, want: 2},
		{name: "steady state stays quiet", at: start.Add(2 * time.Minute), want: 2},
		{name: "active condition refreshes stale baseline", at: start.Add(2 * time.Minute), conditions: []telemetry.TrackerCondition{condition}, want: 4},
		{name: "active condition remains rate limited", at: start.Add(2*time.Minute + 30*time.Second), conditions: []telemetry.TrackerCondition{condition}, want: 4},
		{name: "active interval refreshes", at: start.Add(3 * time.Minute), conditions: []telemetry.TrackerCondition{condition}, want: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager.Poll(context.Background(), []Source{source}, tt.conditions, tt.at)
			if got := counts.total(); got != tt.want {
				t.Fatalf("requests after Poll() = %d, want %d", got, tt.want)
			}
		})
	}
}

type errorFetcher struct {
	err error
}

func (f errorFetcher) Fetch(context.Context, Source) (Report, error) {
	return Report{}, f.err
}

type requestCounts struct {
	mu         sync.Mutex
	summary    int
	unresolved int
}

func (c *requestCounts) add(path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch path {
	case "/api/v2/summary.json":
		c.summary++
	case "/api/v2/incidents/unresolved.json":
		c.unresolved++
	}
}

func (c *requestCounts) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.summary + c.unresolved
}

func newStatuspageServer(t *testing.T, summary map[string]any, unresolved map[string]any, counts *requestCounts) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if counts != nil {
			counts.add(r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v2/summary.json":
			_ = json.NewEncoder(w).Encode(summary)
		case "/api/v2/incidents/unresolved.json":
			_ = json.NewEncoder(w).Encode(unresolved)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func statuspagePayloads(t *testing.T, incidentStatus string, incidentComponents []string, componentsHealthy bool) (map[string]any, map[string]any) {
	t.Helper()
	now := time.Date(2026, 8, 17, 16, 59, 38, 0, time.UTC).Format(time.RFC3339)
	page := map[string]any{
		"id": "page-id", "name": "GitHub", "url": "https://www.githubstatus.com", "time_zone": "Etc/UTC", "updated_at": now,
	}
	componentNames := []string{"API Requests", "Issues", "Pages"}
	components := make([]any, 0, len(componentNames))
	byName := map[string]map[string]any{}
	for index, name := range componentNames {
		status := "operational"
		if !componentsHealthy && name == "API Requests" {
			status = "major_outage"
		}
		component := map[string]any{
			"id": "component-" + string(rune('a'+index)), "name": name, "status": status,
			"created_at": now, "updated_at": now, "position": index + 1, "description": nil,
			"showcase": true, "start_date": nil, "group_id": nil, "page_id": "page-id", "group": false,
			"only_show_if_degraded": false,
		}
		components = append(components, component)
		byName[name] = component
	}
	incidentComponentPayloads := make([]any, 0, len(incidentComponents))
	affected := make([]any, 0, len(incidentComponents))
	for _, name := range incidentComponents {
		component, ok := byName[name]
		if !ok {
			component = map[string]any{
				"id": "component-extra", "name": name, "status": "operational", "created_at": now, "updated_at": now,
				"position": 99, "description": nil, "showcase": true, "start_date": nil, "group_id": nil,
				"page_id": "page-id", "group": false, "only_show_if_degraded": false,
			}
		}
		incidentComponentPayloads = append(incidentComponentPayloads, component)
		affected = append(affected, map[string]any{"code": component["id"], "name": name, "old_status": "operational", "new_status": "degraded_performance"})
	}
	incident := map[string]any{
		"id": "incident-id", "name": "GitHub service disruption", "status": incidentStatus,
		"created_at": now, "updated_at": now, "monitoring_at": nil, "resolved_at": nil, "impact": "critical",
		"shortlink": "https://stspg.io/example", "started_at": now, "page_id": "page-id", "reminder_intervals": nil,
		"incident_updates": []any{map[string]any{
			"id": "update-id", "status": incidentStatus, "body": "Mitigation is underway", "incident_id": "incident-id",
			"created_at": now, "updated_at": now, "display_at": now, "affected_components": affected,
			"deliver_notifications": true, "custom_tweet": nil, "tweet_id": nil,
		}},
		"components": incidentComponentPayloads,
	}
	summary := map[string]any{
		"page": page, "components": components, "incidents": []any{incident}, "scheduled_maintenances": []any{},
		"status": map[string]any{"indicator": "critical", "description": "Major Service Outage"},
	}
	unresolved := map[string]any{"page": page, "incidents": []any{incident}}
	return summary, unresolved
}

func equalStrings(got []string, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
