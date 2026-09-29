package runnerauth

import (
	"reflect"
	"testing"

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
