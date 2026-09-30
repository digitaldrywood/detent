package runnerauth

import (
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
		{"empty name", Routing{State: "active"}, false},
		{"invalid tag", Routing{DisplayName: "Builder", Tags: []string{"has space"}, State: "active"}, false},
		{"invalid state", Routing{DisplayName: "Builder", State: "paused"}, false},
		{"negative capacity", Routing{DisplayName: "Builder", State: "active", CapacityLimit: -1}, false},
		{"duplicate project", Routing{DisplayName: "Builder", State: "active", ProjectIDs: []tracker.ProjectID{"prj_a", "prj_a"}}, false},
		{"authorized home", Routing{DisplayName: "Builder", State: "active", ProjectIDs: []tracker.ProjectID{"prj_a"}, HomeProjectIDs: []tracker.ProjectID{"prj_a"}}, true},
		{"unauthorized home", Routing{DisplayName: "Builder", State: "active", HomeProjectIDs: []tracker.ProjectID{"prj_a"}}, false},
		{"duplicate home", Routing{DisplayName: "Builder", State: "active", ProjectIDs: []tracker.ProjectID{"prj_a"}, HomeProjectIDs: []tracker.ProjectID{"prj_a", "prj_a"}}, false},
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
		{"zero spillover", func(r *Routing) { r.Spillover = Spillover{Mode: "after", AfterMinutes: 0} }, true},
		{"negative spillover", func(r *Routing) { r.Spillover = Spillover{Mode: "after", AfterMinutes: -1} }, false},
		{"unknown spillover", func(r *Routing) { r.Spillover.Mode = "always" }, false},
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

func TestRunnerHomeWorkStatus(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name     string
		home     bool
		dry      *time.Time
		mode     string
		minutes  int
		elapsed  time.Duration
		want     string
		eligible bool
	}{
		{"no homes", false, &since, "after", 0, time.Minute, "", false},
		{"home work", true, nil, "after", 5, time.Minute, "Preferring home work", false},
		{"waiting", true, &since, "after", 5, 4 * time.Minute, "Waiting for home work (4m)", false},
		{"threshold", true, &since, "after", 5, 5 * time.Minute, "Spilled over", true},
		{"never", true, &since, "never", 0, 5 * time.Minute, "Waiting for home work (5m)", false},
		{"future clock", true, &since, "after", 0, -time.Minute, "Waiting for home work (0m)", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := Runner{HomeDrySince: test.dry, Routing: Routing{Spillover: Spillover{Mode: test.mode, AfterMinutes: test.minutes}}}
			if test.home {
				r.HomeProjectIDs = []tracker.ProjectID{"prj_a"}
			}
			now := since.Add(test.elapsed)
			if got := r.HomeWorkStatus(now); got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
			if got := r.SpilloverEligible(now); got != test.eligible {
				t.Fatalf("eligible = %v, want %v", got, test.eligible)
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
	}{
		{"empty", "", "", "2026-09-29T12:00:00Z", true, ""},
		{"timezone", "America/Chicago", "Mon-Fri 09:00-17:00", "2026-09-29T15:00:00Z", true, "2026-09-29T22:30:00Z"},
		{"closed", "America/Chicago", "Mon-Fri 09:00-17:00", "2026-09-29T22:00:00Z", false, "2026-09-29T22:30:00Z"},
		{"overnight", "Asia/Tokyo", "Mon-Fri 22:00-06:00", "2026-09-29T18:00:00Z", true, "2026-09-29T21:30:00Z"},
		{"spring DST", "America/Chicago", "Sat-Sat 22:00-06:00", "2026-03-08T07:30:00Z", true, "2026-03-08T11:30:00Z"},
		{"fall DST", "America/Chicago", "Sat-Sat 22:00-06:00", "2026-11-01T06:30:00Z", true, "2026-11-01T12:30:00Z"},
		{"continuous", "UTC", "Mon-Sun 00:00-24:00", "2026-09-29T12:00:00Z", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tt.now)
			if err != nil {
				t.Fatal(err)
			}
			a := Availability{Timezone: tt.zone}
			if tt.window != "" {
				a.Windows = []string{tt.window}
				a.HardDeadline = "30m"
			}
			status, err := a.Evaluate(now)
			if err != nil {
				t.Fatal(err)
			}
			if status.Open != tt.open {
				t.Fatalf("open = %t, want %t", status.Open, tt.open)
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
