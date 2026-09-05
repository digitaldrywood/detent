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
