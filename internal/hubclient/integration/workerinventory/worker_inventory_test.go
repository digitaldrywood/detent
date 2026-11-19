package workerinventory_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestAttemptWorkerPageIsFreshAndWithinResponseBudget(t *testing.T) {
	t.Parallel()
	const responseBudget = 1 << 20
	path := filepath.Join(t.TempDir(), "private", "runner.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_test"
	file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	project := tracker.ProjectID("prj_" + strings.Repeat("a", 32))
	snapshot := runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 3}}
	if err := runnerauth.SaveRoutingCache(path, snapshot); err != nil {
		t.Fatal(err)
	}
	page := runnerauth.AttemptWorkerPage{ProjectID: project, Workers: make([]runnerauth.AttemptWorker, runnerauth.AttemptWorkerPageSize)}
	for i := range page.Workers {
		page.Workers[i] = runnerauth.AttemptWorker{
			ProjectID: project, WorkItemID: tracker.NativeWorkItemID("wi_" + strings.Repeat("b", 32)),
			AttemptID: fmt.Sprintf("attempt_%032x", i), LeaseID: tracker.LeaseID("lease_" + strings.Repeat("c", 32)),
			MachineID: file.Identity.MachineID, FencingToken: 1<<63 - 1, LeaseState: runnerauth.AttemptLeaseFinished, ExpiresAt: file.Identity.ExpiresAt,
			Worker: tracker.NativeAttemptWorker{PID: 1<<31 - 1, PGID: 1<<31 - 1, StartedAt: time.Date(2026, 11, 19, 12, 0, 0, 123456000, time.UTC), StartKey: strings.Repeat("\x01", tracker.MaxAttemptWorkerStartKeyBytes)},
		}
	}
	page.Next = page.Workers[len(page.Workers)-1].AttemptID
	inventory := runnerauth.AttemptWorkerInventory{RunnerID: file.Identity.RunnerID, Page: &page}
	wire, err := json.Marshal(inventory)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) >= responseBudget {
		t.Fatalf("maximum inventory response is %d bytes, limit %d", len(wire), responseBudget)
	}
	legacyWire, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("Hub offline")
	mode := "online"
	calls := 0
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		wantPath := "/api/v2/organizations/org_test/projects/" + string(project) + "/machines/" + string(file.Identity.MachineID) + "/heartbeat"
		if request.URL.Path != wantPath || request.Method != http.MethodPost || !request.URL.Query().Has("attempt_worker_after") {
			t.Errorf("attempt worker request = %s %s", request.Method, request.URL)
		}
		response := wire
		switch mode {
		case "offline":
			return nil, unavailable
		case "old Hub":
			response = legacyWire
		case "online":
			if cursor := request.URL.Query().Get("attempt_worker_after"); cursor != "" {
				if cursor != page.Next {
					t.Errorf("next cursor = %q, want %q", cursor, page.Next)
				}
				response, err = json.Marshal(runnerauth.AttemptWorkerInventory{RunnerID: file.Identity.RunnerID, Page: &runnerauth.AttemptWorkerPage{ProjectID: project, Workers: []runnerauth.AttemptWorker{}}})
				if err != nil {
					return nil, err
				}
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(response)), Request: request}, nil
	})
	open := func() (*api.Client, *api.NativeClient) {
		t.Helper()
		client, err := api.New(api.Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
		if err != nil {
			t.Fatal(err)
		}
		native, err := client.Native("org_test", project)
		if err != nil {
			t.Fatal(err)
		}
		return client, native
	}
	_, native := open()
	first, err := native.AttemptWorkers(t.Context(), "")
	if err != nil || first.Next != page.Next || len(first.Workers) != len(page.Workers) || first.Workers[0].Worker.StartKey != page.Workers[0].Worker.StartKey {
		t.Fatalf("maximum worker page = %d workers, next %q, error %v", len(first.Workers), first.Next, err)
	}
	last, err := native.AttemptWorkers(t.Context(), first.Next)
	if err != nil || last.ProjectID != project || last.Next != "" || len(last.Workers) != 0 {
		t.Fatalf("complete page = %+v, error %v", last, err)
	}

	cache, err := os.ReadFile(runnerauth.RoutingCachePath(path))
	if err != nil {
		t.Fatal(err)
	}
	var olderReader struct {
		Schema    int    `json:"schema"`
		HubURL    string `json:"hub_url"`
		MachineID string `json:"machine_id"`
		Snapshot  struct {
			RunnerID string             `json:"runner_id"`
			Revision int64              `json:"revision"`
			Routing  runnerauth.Routing `json:"routing"`
		} `json:"snapshot"`
	}
	decoder := json.NewDecoder(bytes.NewReader(cache))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&olderReader); err != nil || olderReader.Snapshot.Revision != snapshot.Revision {
		t.Fatalf("older routing cache reader = %+v, error %v", olderReader, err)
	}
	mode = "offline"
	restarted, native := open()
	if _, _, err := restarted.AvailabilityState(); err != nil {
		t.Fatal(err)
	}
	if stale, err := native.AttemptWorkers(t.Context(), ""); !errors.Is(err, unavailable) || len(stale.Workers) != 0 {
		t.Fatalf("offline restart returned a worker page: %+v, error %v", stale, err)
	}
	mode = "old Hub"
	if missing, err := native.AttemptWorkers(t.Context(), ""); !errors.Is(err, api.ErrUnavailable) || len(missing.Workers) != 0 {
		t.Fatalf("old Hub was interpreted as a complete page: %+v, error %v", missing, err)
	}
	if calls != 4 {
		t.Fatalf("fresh page requests = %d, want 4", calls)
	}
}
