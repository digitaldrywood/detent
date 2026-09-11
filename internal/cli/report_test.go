package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/operations"
)

func TestRunReportHTMLWritesSelfContainedPage(t *testing.T) {
	t.Parallel()
	median := 3600.0
	recorded := operations.Report{
		DataTime: time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC),
		Instance: "mac-studio",
		Stats: []operations.Window{{
			Label: "24h", Merges: 4, Closes: 5, MergesPerDay: 4, ClosesPerDay: 5, CycleMedianSeconds: &median,
			BlockedNights: []operations.Night{{Date: "2026-09-10", Issues: 1}},
			Projects:      []operations.Project{{ID: "detent", Dispatches: 9, SkipReasons: []operations.Reason{{Reason: "capacity", Count: 2}}}},
		}},
		QueueDepth: 2,
		Actions:    []operations.Action{{ID: 7, ProjectID: "detent", Issue: "#2483", Kind: "route", Reason: "operator_routine:stale_merging", EvidenceURL: "/api/v1/projects/detent/issues/explanation?reference=%232483"}},
		Decisions:  []operations.Decision{{ProjectID: "detent", Issue: "#2482", Question: "Merge or close?", URL: "https://github.com/digitaldrywood/detent/issues/2482"}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/operations" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer operator-token" {
			t.Errorf("Authorization = %q", got)
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(recorded)
	}))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(serverURL.Port())
	if err != nil {
		t.Fatal(err)
	}
	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: "/tmp/global.yaml", Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.read = func(string) (globalconfig.Config, error) {
		return globalconfig.Config{APIToken: "operator-token"}, nil
	}
	opts.httpDo = server.Client().Do
	client, err := newDashboardReadClient(t.Context(), "/tmp/global.yaml", serverURL.Hostname(), port, true, opts)
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "nested", "Detent Status.html")
	result, err := runReportHTML(t.Context(), client, path)
	if err != nil {
		t.Fatalf("runReportHTML() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := string(content)
	if result.Instance != "mac-studio" || result.DataTime != "2026-09-11T18:00:00Z" || result.Bytes != len(content) {
		t.Fatalf("result = %#v", result)
	}
	for _, want := range []string{
		`id="operations-stats"`, `id="operations-actions"`, `id="operations-decisions"`,
		"mac-studio", "2026-09-11T18:00:00Z", "<style>", "operator_routine:stale_merging", "Merge or close?",
		`href="` + server.URL + `/api/v1/projects/detent/issues/explanation?reference=%232483"`,
		`href="https://github.com/digitaldrywood/detent/issues/2482"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, forbidden := range []string{"<link", "<script", "hx-get", "/static/"} {
		if strings.Contains(page, forbidden) {
			t.Errorf("page contains external or live asset %q", forbidden)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestReportCommandRequiresHTMLPath(t *testing.T) {
	t.Parallel()
	cmd := newReportCommand(new(string), new(string), new(int), defaultOptions())
	cmd.SetArgs([]string{})
	cmd.SetOut(&strings.Builder{})
	cmd.SetErr(&strings.Builder{})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--html is required") {
		t.Fatalf("Execute() error = %v", err)
	}
}
