package providercapacity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testReport() Report {
	return Report{Provider: "openai", Backend: "codex", AccountAlias: "work", SharedAccountAlias: "team", Models: []string{"sol"}, MaxConcurrent: 2, Availability: "available", ObservedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
}

func TestReportObservation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, state, want string
		age, reset        time.Duration
	}{
		{"available", "available", "available", 0, 0},
		{"unknown", "unknown", "unknown", 0, 0},
		{"exhausted", "exhausted", "exhausted", time.Second, time.Minute},
		{"stale available", "available", "unknown", MaxAge, 0},
		{"stale exhausted", "exhausted", "unknown", MaxAge, time.Hour},
		{"future observation", "available", "unknown", -time.Second, 0},
		{"reset equality", "exhausted", "unknown", time.Minute, time.Minute},
		{"reset past", "available", "unknown", time.Minute, time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testReport()
			r.Availability = test.state
			if test.reset != 0 {
				r.ResetAt = r.ObservedAt.Add(test.reset)
			}
			if got := r.State(r.ObservedAt.Add(test.age)); got != test.want {
				t.Fatalf("state = %s, want %s", got, test.want)
			}
		})
	}
}

func TestReportValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*Report)
	}{
		{"provider", func(r *Report) { r.Provider = "account@example.com" }},
		{"backend", func(r *Report) { r.Backend = "two words" }},
		{"account", func(r *Report) { r.AccountAlias = "email@example.com" }},
		{"shared account", func(r *Report) { r.SharedAccountAlias = strings.Repeat("a", 65) }},
		{"zero concurrency", func(r *Report) { r.MaxConcurrent = 0 }},
		{"oversized concurrency", func(r *Report) { r.MaxConcurrent = 10001 }},
		{"missing models", func(r *Report) { r.Models = nil }},
		{"too many models", func(r *Report) { r.Models = make([]string, 129) }},
		{"invalid model", func(r *Report) { r.Models = []string{"raw prompt"} }},
		{"availability", func(r *Report) { r.Availability = "unlimited" }},
		{"missing observation", func(r *Report) { r.ObservedAt = time.Time{} }},
		{"reset before observation", func(r *Report) { r.ResetAt = r.ObservedAt.Add(-time.Second) }},
		{"detail for an unreported model", func(r *Report) { r.ModelDetails = []ModelDetail{{ID: "astra"}} }},
		{"detail with an invalid identifier", func(r *Report) {
			r.Models = append(r.Models, "raw prompt")
			r.ModelDetails = []ModelDetail{{ID: "raw prompt"}}
		}},
		{"two details for one model", func(r *Report) {
			r.ModelDetails = []ModelDetail{{ID: "sol"}, {ID: "sol"}}
		}},
		{"more details than models", func(r *Report) {
			r.ModelDetails = []ModelDetail{{ID: "sol"}, {ID: "sol"}}
		}},
		{"detail label carrying prose", func(r *Report) {
			r.ModelDetails = []ModelDetail{{ID: "sol", Label: strings.Repeat("a", 129)}}
		}},
		{"detail provider that is not a token", func(r *Report) {
			r.ModelDetails = []ModelDetail{{ID: "sol", Provider: "two words"}}
		}},
		{"too many reasoning efforts", func(r *Report) {
			efforts := make([]string, 17)
			for index := range efforts {
				efforts[index] = "effort" + string(rune('a'+index))
			}
			r.ModelDetails = []ModelDetail{{ID: "sol", ReasoningEfforts: efforts}}
		}},
		{"repeated reasoning effort", func(r *Report) {
			r.ModelDetails = []ModelDetail{{ID: "sol", ReasoningEfforts: []string{"low", "low"}}}
		}},
		{"reasoning effort that is not a token", func(r *Report) {
			r.ModelDetails = []ModelDetail{{ID: "sol", ReasoningEfforts: []string{"as much as you like"}}}
		}},
		{"default effort outside the ladder", func(r *Report) {
			r.ModelDetails = []ModelDetail{{ID: "sol", ReasoningEfforts: []string{"low"}, DefaultReasoningEffort: "max"}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testReport()
			test.change(&r)
			if Validate([]Report{r}) == nil {
				t.Fatal("accepted invalid report")
			}
		})
	}
	if err := Validate([]Report{testReport()}); err != nil {
		t.Fatal(err)
	}
	for _, reports := range [][]Report{{testReport(), testReport()}, make([]Report, 33)} {
		if Validate(reports) == nil {
			t.Fatal("accepted duplicate or excessive backends")
		}
	}
	detailed := testReport()
	detailed.ModelDetails = []ModelDetail{{
		ID: "sol", Label: "Sol", Provider: "openai", Default: true,
		ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high", Legacy: true,
	}}
	if err := Validate([]Report{detailed}); err != nil {
		t.Fatalf("rejected a well-formed detail: %v", err)
	}
}

// TestReportModelDetail covers the lookup the hub uses to publish a model's
// ladder, and the answer a report that carries no detail gives.
func TestReportModelDetail(t *testing.T) {
	t.Parallel()
	bare := testReport()
	if _, ok := bare.Detail("sol"); ok {
		t.Fatal("a report with no detail claimed to have some")
	}
	detailed := testReport()
	detailed.Models = []string{"sol", "astra"}
	detailed.ModelDetails = []ModelDetail{{ID: "astra", Label: "Astra", ReasoningEfforts: []string{"low", "max"}}}
	for _, test := range []struct {
		name, model string
		wantOK      bool
		wantLabel   string
	}{
		{name: "described model", model: "astra", wantOK: true, wantLabel: "Astra"},
		{name: "reported model with no detail", model: "sol"},
		{name: "model outside the report", model: "opus"},
	} {
		t.Run(test.name, func(t *testing.T) {
			detail, ok := detailed.Detail(test.model)
			if ok != test.wantOK || detail.Label != test.wantLabel {
				t.Fatalf("Detail(%q) = %#v, %t, want label %q and %t", test.model, detail, ok, test.wantLabel, test.wantOK)
			}
		})
	}
}

// TestLoadReportsAcceptsModelDetail proves a capacity file may carry the
// per-model detail, and that a file written before it existed still loads —
// the loader refuses unknown fields, so both halves matter.
func TestLoadReportsAcceptsModelDetail(t *testing.T) {
	t.Parallel()
	report := testReport()
	report.ModelDetails = []ModelDetail{{ID: "sol", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "low"}}
	raw, err := json.Marshal([]Report{report})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "capacity.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	detail, ok := loaded[0].Detail("sol")
	if !ok || detail.DefaultReasoningEffort != "low" || len(detail.ReasoningEfforts) != 2 {
		t.Fatalf("loaded detail = %#v, %t", detail, ok)
	}
}

func TestLoadReports(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal([]Report{testReport()})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, raw string
		valid     bool
	}{
		{"valid", string(raw), true},
		{"empty", "[]", false},
		{"null", "null", false},
		{"syntax", "[", false},
		{"trailing", string(raw) + " {}", false},
		{"secret field", strings.Replace(string(raw), "\"provider\":", "\"api_key\":\"private\",\"provider\":", 1), false},
		{"invalid bound", strings.Replace(string(raw), "\"max_concurrent\":2", "\"max_concurrent\":0", 1), false},
		{"oversized", strings.Repeat(" ", 256*1024) + string(raw), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reports.json")
			if err := os.WriteFile(path, []byte(test.raw), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if (err == nil) != test.valid {
				t.Fatalf("Load() = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "private") {
				t.Fatal("report error leaked input")
			}
		})
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestProviderCompatibility(t *testing.T) {
	t.Parallel()
	r := testReport()
	for _, test := range []struct {
		requirement      Requirement
		valid, supported bool
	}{
		{Requirement{Role: "code", Backend: "codex", Model: "sol"}, true, true},
		{Requirement{Role: "plan", Backend: "codex", Model: "astra"}, true, false},
		{Requirement{Role: "code", Backend: "claude", Model: "sol"}, true, false},
		{Requirement{Role: "code", Backend: "codex", Model: ""}, false, false},
	} {
		if (test.requirement.Validate() == nil) != test.valid || r.Supports(test.requirement) != test.supported {
			t.Fatalf("invalid compatibility: %+v", test)
		}
	}
	if r.Pool() != "openai/shared/team" {
		t.Fatal(r.Pool())
	}
	r.SharedAccountAlias = ""
	if r.Pool() != "openai/unknown" {
		t.Fatal(r.Pool())
	}
	if got := (View{Report: r, State: "unknown", Reason: "stale"}).Summary(); !strings.Contains(got, "stale") || !strings.Contains(got, "0 / 2") {
		t.Fatal(got)
	}
}
