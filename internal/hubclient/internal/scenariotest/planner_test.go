package scenariotest

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type phaseResponses []int

func (p *phaseResponses) RoundTrip(*http.Request) (*http.Response, error) {
	status := (*p)[0]
	*p = (*p)[1:]
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(""))}, nil
}

func TestPlanLeaseLossWaitsForPublishedReceipt(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/work-items/wi_plan"):
			if err := json.NewEncoder(w).Encode(tracker.NativeIssue{State: "In Progress"}); err != nil {
				t.Error(err)
			}
		case strings.HasSuffix(req.URL.Path, "/release"):
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(req.URL.Path, "/phases"):
			w.WriteHeader(http.StatusConflict)
		default:
			t.Errorf("unexpected request: %s", req.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := api.New(api.Config{URL: server.URL, TokenSource: func() string { return "test-token" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	responses := phaseResponses{http.StatusServiceUnavailable, http.StatusOK}
	transport := nativePlanTransport{next: &responses, native: native, lost: make(chan nativePlanFinish, 1), lossPhase: "plan"}
	transport.loseLease.Store(true)
	body, err := json.Marshal(tracker.NativePhaseReceipt{Mutation: tracker.Mutation{LeaseID: "lease_plan", FencingToken: 1}, Phase: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusOK} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/work-items/wi_plan/phases", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		response, err := transport.RoundTrip(req)
		if err != nil || response.StatusCode != status {
			t.Fatalf("phase response = %v, %v, want %d", response, err, status)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case lost := <-transport.lost:
		if lost.err != nil || lost.state != "In Progress" {
			t.Fatalf("failed to retire published lease: %+v", lost)
		}
	default:
		t.Fatal("the failed POST consumed the lease-loss trigger")
	}
}
