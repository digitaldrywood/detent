package runnerauth

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRoutingNormalizationAndValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		routing Routing
		valid   bool
	}{
		{"normalized", Routing{DisplayName: " Builder ", Tags: []string{" Linux ", "BUILD", "linux"}, State: "active", CapacityLimit: 2}, true},
		{"organization scope", Routing{Scope: "organization", DisplayName: "Builder", State: "active"}, true},
		{"unknown scope", Routing{Scope: "all", DisplayName: "Builder", State: "active"}, false},
		{"organization with list", Routing{Scope: "organization", DisplayName: "Builder", State: "active", ProjectIDs: []tracker.ProjectID{"prj_a"}}, false},
		{"negative rank", Routing{DisplayName: "Builder", State: "active", ProjectRanks: map[tracker.ProjectID]int{"prj_a": -1}}, false},
		{"empty name", Routing{State: "active"}, false},
		{"invalid tag", Routing{DisplayName: "Builder", Tags: []string{"has space"}, State: "active"}, false},
		{"invalid state", Routing{DisplayName: "Builder", State: "paused"}, false},
		{"negative capacity", Routing{DisplayName: "Builder", State: "active", CapacityLimit: -1}, false},
		{"duplicate project", Routing{DisplayName: "Builder", State: "active", ProjectIDs: []tracker.ProjectID{"prj_a", "prj_a"}}, false},
		{"empty project", Routing{DisplayName: "Builder", State: "active", ProjectIDs: []tracker.ProjectID{""}}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			normalized := test.routing.Normalized()
			if err := normalized.Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate = %v", err)
			}
			if test.name == "normalized" {
				if normalized.DisplayName != "Builder" || !reflect.DeepEqual(normalized.Tags, []string{"build", "linux"}) {
					t.Fatalf("normalized = %#v", normalized)
				}
				if test.routing.Tags[0] != " Linux " {
					t.Fatal("normalization mutated caller tags")
				}
			}
		})
	}
}

func TestRoutingSettingsValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		settings func(*Routing)
		valid    bool
	}{
		{"defaults", func(*Routing) {}, true},
		{"trusted", func(r *Routing) { r.IsolationTier = "native-trusted" }, true},
		{"unknown tier", func(r *Routing) { r.IsolationTier = "root" }, false},
		{"loopback service", func(r *Routing) { r.HostServices = []string{"tcp:127.0.0.1:8080"} }, true},
		{"unix service", func(r *Routing) { r.HostServices = []string{"unix:/var/run/service.sock"} }, true},
		{"non-loopback service", func(r *Routing) { r.HostServices = []string{"tcp:0.0.0.0:8080"} }, false},
		{"docker socket", func(r *Routing) { r.HostServices = []string{"unix:/var/run/docker.sock"} }, false},
		{"valid window", func(r *Routing) {
			r.Availability = Availability{Timezone: "America/Chicago", Windows: []string{"Mon-Fri 09:00-17:00"}}
		}, true},
		{"counted window", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Fri 09:00-17:00": 2}}
		}, true},
		{"zero window slots", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Fri 09:00-17:00": 0}}
		}, true},
		{"negative window slots", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Fri 09:00-17:00": -1}}
		}, false},
		{"excessive window slots", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Fri 09:00-17:00": 10001}}
		}, false},
		{"count for absent window", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, WindowSlots: map[string]int{"Mon-Sun 22:00-07:00": 2}}
		}, false},
		{"count without windows", func(r *Routing) {
			r.Availability.WindowSlots = map[string]int{"Mon-Fri 09:00-17:00": 2}
		}, false},
		{"unknown timezone", func(r *Routing) {
			r.Availability = Availability{Timezone: "Nowhere/Unknown", Windows: []string{"Mon-Fri 09:00-17:00"}}
		}, false},
		{"zero window", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-09:00"}}
		}, false},
		{"overlapping windows", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00", "Wed-Wed 12:00-18:00"}}
		}, false},
		{"deadline", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, HardDeadline: "30m"}
		}, true},
		{"deadline without window", func(r *Routing) { r.Availability.HardDeadline = "30m" }, false},
		{"negative deadline", func(r *Routing) {
			r.Availability = Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}, HardDeadline: "-1m"}
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Routing{DisplayName: "Runner", State: "active", CapacityLimit: 1}
			test.settings(&r)
			if err := r.Normalized().Validate(); (err == nil) != test.valid {
				t.Fatalf("Validate() = %v, want valid %v", err, test.valid)
			}
		})
	}
}

func TestRunnerEligibility(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name         string
		change       func(*Runner)
		requirements policy.Requirements
		active       bool
		code         string
	}{
		{"empty selector", func(*Runner) {}, policy.Requirements{}, false, ""},
		{"all selectors", func(*Runner) {}, policy.Requirements{RequiredTags: []string{"build", "linux"}, RunnerID: "runner_a", MachineID: "machine_a"}, false, ""},
		{"organization scope grants unlisted project", func(r *Runner) { r.Scope = "organization"; r.ProjectIDs = nil }, policy.Requirements{}, false, ""},
		{"no access", func(r *Runner) { r.ProjectIDs = nil }, policy.Requirements{}, false, "project_access_denied"},
		{"no claim permission", func(r *Runner) { r.Operations = nil }, policy.Requirements{}, false, "claim_not_permitted"},
		{"disabled", func(r *Runner) { r.State = "disabled" }, policy.Requirements{}, false, "runner_disabled"},
		{"disabled active", func(r *Runner) { r.State = "disabled" }, policy.Requirements{}, true, "runner_disabled"},
		{"draining", func(r *Runner) { r.State = "draining" }, policy.Requirements{}, false, "runner_draining"},
		{"draining active", func(r *Runner) { r.State = "draining" }, policy.Requirements{}, true, ""},
		{"offline", func(r *Runner) { r.Health = "offline" }, policy.Requirements{}, false, "runner_offline"},
		{"revoked", func(r *Runner) { r.Health = "revoked" }, policy.Requirements{}, true, "runner_revoked"},
		{"expired", func(r *Runner) { r.Health = "expired" }, policy.Requirements{}, true, "runner_expired"},
		{"unknown tag", func(*Runner) {}, policy.Requirements{RequiredTags: []string{"macos"}}, false, "selector_no_match"},
		{"wrong host", func(*Runner) {}, policy.Requirements{MachineID: "machine_b"}, false, "selector_no_match"},
		{"wrong runner", func(*Runner) {}, policy.Requirements{RunnerID: "runner_b"}, false, "selector_no_match"},
		{"host full", func(r *Runner) { r.HostUsed = 2 }, policy.Requirements{}, false, "host_capacity"},
		{"runner full", func(r *Runner) { r.Used = 2 }, policy.Requirements{}, false, "runner_capacity"},
		{"reported pause", func(r *Runner) { r.ReportedCapacity = 0 }, policy.Requirements{}, false, "runner_capacity"},
		{"active retains slot", func(r *Runner) { r.Used = 2; r.HostUsed = 2 }, policy.Requirements{}, true, ""},
		{"provider exhausted", func(r *Runner) {
			r.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{MaxConcurrent: 2}, State: "exhausted"}}
		}, policy.Requirements{}, false, "provider_capacity"},
		{"provider fully reserved", func(r *Runner) {
			r.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{MaxConcurrent: 2}, State: "available", Used: 2}}
		}, policy.Requirements{}, false, "provider_capacity"},
		{"provider unknown", func(r *Runner) {
			r.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{MaxConcurrent: 2}, State: "unknown"}}
		}, policy.Requirements{}, false, ""},
		{"provider account limit unknown", func(r *Runner) {
			r.ProviderCapacity = []providercapacity.View{{State: "unknown", Used: 12}}
		}, policy.Requirements{}, false, ""},
		{"active provider exhaustion", func(r *Runner) {
			r.ProviderCapacity = []providercapacity.View{{Report: providercapacity.Report{MaxConcurrent: 2}, State: "exhausted"}}
		}, policy.Requirements{}, true, ""},
		{"rename", func(r *Runner) { r.DisplayName = "New name"; r.HostDisplayName = "New host" }, policy.Requirements{RunnerID: "runner_a", MachineID: "machine_a"}, false, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Runner{Binding: Binding{RunnerID: "runner_a", MachineID: "machine_a"}, Routing: Routing{DisplayName: "Runner", Tags: []string{"build", "linux"}, State: "active", CapacityLimit: 2, ProjectIDs: []tracker.ProjectID{"prj_a"}}, Health: "online", HostCapacity: 2, ReportedCapacity: 2, Operations: []string{Claim}}
			test.change(&r)
			got := r.Exclusions("prj_a", test.requirements, test.active)
			if test.code == "" {
				if len(got) != 0 {
					t.Fatalf("exclusions = %#v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Code != test.code {
				t.Fatalf("exclusions = %#v, want %s", got, test.code)
			}
		})
	}
}

func TestRunnerAvailability(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, zone, window, now string
		open                    bool
		deadline                string
		slots                   map[string]int
		otherWindows            []string
		capacity, wantCapacity  int
	}{
		{name: "empty", now: "2026-09-29T12:00:00Z", open: true, capacity: 6, wantCapacity: 6},
		{name: "timezone", zone: "America/Chicago", window: "Mon-Fri 09:00-17:00", now: "2026-09-29T15:00:00Z", open: true, deadline: "2026-09-29T22:30:00Z", capacity: 6, wantCapacity: 6},
		{name: "closed", zone: "America/Chicago", window: "Mon-Fri 09:00-17:00", now: "2026-09-29T22:00:00Z", deadline: "2026-09-29T22:30:00Z", capacity: 6},
		{name: "overnight", zone: "Asia/Tokyo", window: "Mon-Fri 22:00-06:00", now: "2026-09-29T18:00:00Z", open: true, deadline: "2026-09-29T21:30:00Z", capacity: 6, wantCapacity: 6},
		{name: "spring DST", zone: "America/Chicago", window: "Sat-Sat 22:00-06:00", now: "2026-03-08T07:30:00Z", open: true, deadline: "2026-03-08T11:30:00Z", slots: map[string]int{"Sat-Sat 22:00-06:00": 2}, capacity: 6, wantCapacity: 2},
		{name: "fall DST", zone: "America/Chicago", window: "Sat-Sat 22:00-06:00", now: "2026-11-01T06:30:00Z", open: true, deadline: "2026-11-01T12:30:00Z", slots: map[string]int{"Sat-Sat 22:00-06:00": 2}, capacity: 6, wantCapacity: 2},
		{name: "continuous", zone: "UTC", window: "Mon-Sun 00:00-24:00", now: "2026-09-29T12:00:00Z", open: true, capacity: 6, wantCapacity: 6},
		{name: "counted continuous", zone: "UTC", window: "Mon-Sun 00:00-24:00", now: "2026-09-29T12:00:00Z", open: true, slots: map[string]int{"Mon-Sun 00:00-24:00": 2}, capacity: 6, wantCapacity: 2},
		{name: "count above limit", zone: "UTC", window: "Mon-Sun 00:00-24:00", now: "2026-09-29T12:00:00Z", open: true, slots: map[string]int{"Mon-Sun 00:00-24:00": 6}, capacity: 1, wantCapacity: 1},
		{name: "paused capacity", zone: "UTC", window: "Mon-Sun 00:00-24:00", now: "2026-09-29T12:00:00Z", open: true, slots: map[string]int{"Mon-Sun 00:00-24:00": 6}},
		{name: "zero count", zone: "UTC", window: "Mon-Sun 00:00-24:00", now: "2026-09-29T12:00:00Z", open: true, slots: map[string]int{"Mon-Sun 00:00-24:00": 0}, capacity: 6},
		{name: "counted closed", zone: "UTC", window: "Mon-Fri 09:00-17:00", now: "2026-09-29T17:00:00Z", deadline: "2026-09-29T17:30:00Z", slots: map[string]int{"Mon-Fri 09:00-17:00": 2}, capacity: 6},
		{name: "counted opening", zone: "UTC", window: "Mon-Fri 09:00-17:00", now: "2026-09-29T09:00:00Z", open: true, deadline: "2026-09-29T17:30:00Z", slots: map[string]int{"Mon-Fri 09:00-17:00": 2}, capacity: 6, wantCapacity: 2},
		{name: "night opening", zone: "America/Chicago", window: "Mon-Sun 22:00-07:00", otherWindows: []string{"Mon-Sun 07:00-22:00"}, now: "2026-09-30T03:00:00Z", open: true, slots: map[string]int{"Mon-Sun 22:00-07:00": 6, "Mon-Sun 07:00-22:00": 2}, capacity: 8, wantCapacity: 6},
		{name: "day opening", zone: "America/Chicago", window: "Mon-Sun 22:00-07:00", otherWindows: []string{"Mon-Sun 07:00-22:00"}, now: "2026-09-30T12:00:00Z", open: true, slots: map[string]int{"Mon-Sun 22:00-07:00": 6, "Mon-Sun 07:00-22:00": 2}, capacity: 8, wantCapacity: 2},
		{name: "uncounted day", zone: "America/Chicago", window: "Mon-Sun 22:00-07:00", otherWindows: []string{"Mon-Sun 07:00-22:00"}, now: "2026-09-30T12:00:00Z", open: true, slots: map[string]int{"Mon-Sun 22:00-07:00": 6}, capacity: 8, wantCapacity: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			a := Availability{Timezone: tt.zone, WindowSlots: tt.slots}
			if tt.window != "" {
				a.Windows = append([]string{tt.window}, tt.otherWindows...)
				a.HardDeadline = "30m"
			}
			raw, err := json.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			var legacy struct {
				Timezone     string   `json:"timezone"`
				Windows      []string `json:"windows"`
				HardDeadline string   `json:"hard_deadline"`
			}
			if err := json.Unmarshal(raw, &legacy); err != nil {
				t.Fatalf("legacy runner cannot decode availability: %v", err)
			}
			legacyStatus, err := (Availability{Timezone: legacy.Timezone, Windows: legacy.Windows, HardDeadline: legacy.HardDeadline}).Evaluate(now)
			if err != nil || legacyStatus.Open != tt.open {
				t.Fatalf("legacy availability = %+v, %v, want open %t", legacyStatus, err, tt.open)
			}
			status, err := a.Evaluate(now)
			if err != nil {
				t.Fatal(err)
			}
			if status.Open != tt.open {
				t.Fatalf("open = %t, want %t", status.Open, tt.open)
			}
			capacity, err := a.Capacity(now, tt.capacity)
			if err != nil || capacity != tt.wantCapacity {
				t.Fatalf("capacity = %d, %v, want %d", capacity, err, tt.wantCapacity)
			}
			deadline, err := a.Deadline(now)
			if err != nil {
				t.Fatal(err)
			}
			if tt.deadline == "" {
				if !deadline.IsZero() {
					t.Fatalf("unexpected deadline %v", deadline)
				}
			} else if deadline.UTC().Format(time.RFC3339) != tt.deadline {
				t.Fatalf("deadline = %v, want %s", deadline, tt.deadline)
			}
			a.HardDeadline = ""
			deadline, err = a.Deadline(now)
			if err != nil || !deadline.IsZero() {
				t.Fatalf("no deadline = %v, %v", deadline, err)
			}
		})
	}
}

func TestRunnerStatus(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 29, 18, 0, 0, 0, time.UTC)
	for _, health := range []string{"online", "offline", "revoked", "expired"} {
		t.Run(health, func(t *testing.T) {
			r := Runner{Health: health, Routing: Routing{Availability: Availability{Timezone: "UTC", Windows: []string{"Mon-Fri 09:00-17:00"}}}}
			want := health
			if health == "online" {
				want = "outside_hours"
			}
			if got := r.Status(now); got != want {
				t.Fatalf("status = %s, want %s", got, want)
			}
			if got := r.Status(now.Add(-2 * time.Hour)); got != health {
				t.Fatalf("inside status = %s", got)
			}
		})
	}
}
