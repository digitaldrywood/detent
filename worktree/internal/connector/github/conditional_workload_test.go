package github

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The endpoint counts come from the 2026-09-29 warm label-status refresh.
// Keeping the path order fixed exposes the cascading misses of an undersized cache.
func TestClientRESTConditionalFullRefreshWorkload(t *testing.T) {
	if testing.Short() {
		t.Skip("network listener integration")
	}

	repo := pullRequestRepo{Owner: "fixture", Name: "detent"}
	paths := make([]string, 0, 443+399+247+263)
	for n := 1; n <= 443; n++ {
		paths = append(paths, restIssueCommentsListPath(issueRef{Owner: repo.Owner, Name: repo.Name, Number: n}))
	}
	for n := 1; n <= 399; n++ {
		paths = append(paths, restIssueBlockedByDependenciesListPath(issueRef{Owner: repo.Owner, Name: repo.Name, Number: n}))
	}
	for n := 1; n <= 247; n++ {
		paths = append(paths, restIssuePath(issueRef{Owner: repo.Owner, Name: repo.Name, Number: n}))
	}
	for n := 1; n <= 263; n++ {
		paths = append(paths, restPullRequestPath(repo, n))
	}
	if len(paths) <= 1024 {
		t.Fatal("fixture must exceed the old cache capacity")
	}

	type evidence struct {
		Path      string `json:"path"`
		Version   int    `json:"version"`
		Lane      string `json:"lane"`
		Pull      string `json:"pull"`
		BlockedBy string `json:"blocked_by"`
		Comment   string `json:"comment"`
	}
	versions := map[string]int{}
	var serverMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverMu.Lock()
		defer serverMu.Unlock()
		path := r.URL.RequestURI()
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			if r.Method == http.MethodPost {
				path += "?per_page=100"
			}
			versions[path] = 2
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected method %s", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		version := versions[path]
		if version == 0 {
			version = 1
		}
		etag := `"` + strconv.Itoa(version) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		lane, pull, blockedBy := "Todo", "fixture/detent#263", "fixture/detent#42"
		if version == 2 {
			lane, pull, blockedBy = "In Progress", "fixture/detent#264", "fixture/detent#43"
		}
		if err := json.NewEncoder(w).Encode(evidence{Path: path, Version: version, Lane: lane, Pull: pull, BlockedBy: blockedBy, Comment: fmt.Sprintf("revision-%d", version)}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	client, err := NewClient(ClientConfig{Endpoint: server.URL, TokenSource: StaticTokenSource("fixture"), HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}

	refresh := func(t *testing.T, changed map[string]bool) (time.Duration, int) {
		t.Helper()
		start := time.Now()
		evictions := 0
		for _, path := range paths {
			// In this serial workload, inserting a missing key into a full cache
			// causes exactly one eviction. Replacing changed evidence does not.
			if _, ok := client.restCache[restCacheKey(http.MethodGet, path)]; !ok && len(client.restCache) == restConditionalCacheMaxEntries {
				evictions++
			}
			var got evidence
			if err := client.REST(t.Context(), http.MethodGet, path, nil, &got); err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			wantVersion := 1
			lane, pull, blockedBy := "Todo", "fixture/detent#263", "fixture/detent#42"
			if changed[path] {
				wantVersion = 2
				lane, pull, blockedBy = "In Progress", "fixture/detent#264", "fixture/detent#43"
			}
			if got.Path != path || got.Version != wantVersion || got.Lane != lane || got.Pull != pull || got.BlockedBy != blockedBy || got.Comment != fmt.Sprintf("revision-%d", wantVersion) {
				t.Fatalf("GET %s evidence = %+v", path, got)
			}
		}
		return time.Since(start), evictions
	}
	coldDuration, coldEvictions := refresh(t, nil)
	coldUsage := client.FlushRESTRateLimitUsage()
	coldEntries := len(client.restCache)
	coldBytes := restCacheFixtureBytes(client.restCache)
	t.Logf("distinct_paths=%d cold: conditional=%d billable=%d evictions=%d retained_entries=%d retained_bytes=%d duration=%s", len(paths), coldUsage.ConditionalRequests, coldUsage.BillableRequests, coldEvictions, coldEntries, coldBytes, coldDuration)
	if coldUsage.TotalRequests != int64(len(paths)) || coldUsage.BillableRequests != int64(len(paths)) || coldUsage.ConditionalRequests != 0 {
		t.Fatalf("cold REST usage = %+v", coldUsage)
	}
	changed := map[string]bool{}
	for _, phase := range []struct {
		name      string
		mutations []string
	}{
		{name: "unchanged"},
		{name: "changed comment", mutations: paths[:1]},
		{name: "changed issue dependency and PR", mutations: []string{paths[443], paths[443+399], paths[443+399+247]}},
		{name: "unchanged after mutations"},
	} {
		t.Run(phase.name, func(t *testing.T) {
			for _, path := range phase.mutations {
				method, writePath := http.MethodPatch, path
				if path == paths[0] || path == paths[443] {
					method, writePath = http.MethodPost, path[:len(path)-len("?per_page=100")]
				}
				if err := client.REST(t.Context(), method, writePath, map[string]string{"body": "revision-2"}, nil); err != nil {
					t.Fatalf("%s %s: %v", method, writePath, err)
				}
				changed[path] = true
			}
			client.FlushRESTRateLimitUsage()
			duration, evictions := refresh(t, changed)
			usage := client.FlushRESTRateLimitUsage()
			t.Logf("conditional=%d billable=%d 304=%d evictions=%d retained_entries=%d retained_bytes=%d duration=%s", usage.ConditionalRequests, usage.BillableRequests, usage.NotModifiedRequests, evictions, len(client.restCache), restCacheFixtureBytes(client.restCache), duration)
			if usage.TotalRequests != int64(len(paths)) || usage.ConditionalRequests != int64(len(paths)) || usage.BillableRequests != int64(len(phase.mutations)) || usage.NotModifiedRequests != int64(len(paths)-len(phase.mutations)) || evictions != 0 {
				t.Errorf("warm REST usage = %+v, evictions = %d; want %d conditional, %d billable, %d 304, zero evictions", usage, evictions, len(paths), len(phase.mutations), len(paths)-len(phase.mutations))
			}
		})
	}
}

func restCacheFixtureBytes(entries map[string]restCacheEntry) int {
	bytes := 0
	for key, entry := range entries {
		bytes += len(key) + len(entry.etag) + len(entry.body)
		for name, values := range entry.headers {
			bytes += len(name)
			for _, value := range values {
				bytes += len(value)
			}
		}
	}
	return bytes
}
