package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
)

func TestApplyApprovedNativePolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*policy.Descriptor, map[string]json.RawMessage)
	}{
		{"stored revision", func(d *policy.Descriptor, _ map[string]json.RawMessage) {
			d.SourceRevision = strings.Repeat("b", 64)
		}},
		{"stored gate metadata", func(d *policy.Descriptor, _ map[string]json.RawMessage) {
			d.Gates.Kind = gate.KindCommand
			d.Gates.AutomatedReview = gate.AutomatedReviewOff
			d.Gates.Validator = false
			d.Gates.SecurityAudit = true
		}},
		{"unknown behavior", func(_ *policy.Descriptor, b map[string]json.RawMessage) {
			b["RetiredBehavior"] = json.RawMessage(`{"Enabled":true}`)
		}},
		{"missing behavior defaults", func(_ *policy.Descriptor, b map[string]json.RawMessage) {
			delete(b, "Gate")
		}},
		{"missing nested gate fields", func(_ *policy.Descriptor, b map[string]json.RawMessage) {
			b["Gate"] = json.RawMessage(`{"SecurityAudit":{},"Validator":{}}`)
		}},
		{"missing runner requirements", func(_ *policy.Descriptor, b map[string]json.RawMessage) {
			delete(b, "Runners")
		}},
		{"missing priority map", func(_ *policy.Descriptor, b map[string]json.RawMessage) {
			delete(b, "PriorityMap")
		}},
		{"older behavior shape", func(d *policy.Descriptor, b map[string]json.RawMessage) {
			d.SourceRevision = strings.Repeat("b", 64)
			b["RetiredBehavior"] = json.RawMessage(`{"Enabled":true}`)
			delete(b, "Gate")
			delete(b, "Runners")
			delete(b, "Plan")
			delete(b, "Review")
			delete(b, "Deliverable")
			delete(b, "Agent")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.Tracker.Kind = TrackerHubNative
			cfg.Plan.Enabled = false
			cfg.Plan.Review = gate.PlanReviewAutomated
			cfg.Review.Human = true
			cfg.Gate.Kind = gate.KindArtifact
			cfg.Gate.AutomatedReview = gate.AutomatedReviewOptional
			cfg.Gate.SecurityAudit.Enabled = false
			cfg.Gate.Validator.Enabled = true
			cfg.Agent.AutoPromote.Enabled = true
			cfg.Deliverable.MergeMethod = "rebase"
			cfg.Deliverable.GitHubPullRequest = true
			cfg.Runners = Runners{Profile: "approved", Profiles: map[string]policy.Requirements{"approved": {RequiredTags: []string{"linux"}}}}
			descriptor, err := ResolvePolicy(Workflow{Config: cfg, Prompt: "Approved instructions."})
			if err != nil {
				t.Fatal(err)
			}
			var behavior map[string]json.RawMessage
			if err := json.Unmarshal(descriptor.Configuration.Behavior, &behavior); err != nil {
				t.Fatal(err)
			}
			test.change(&descriptor, behavior)
			descriptor.Configuration.Behavior, err = json.Marshal(behavior)
			if err != nil {
				t.Fatal(err)
			}
			descriptor = descriptor.WithID()
			applied, err := ApplyNativePolicy(Workflow{Config: Default()}, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if applied.Prompt != "Approved instructions." || applied.Config.Plan.Enabled || applied.Config.Runners.Profile != "approved" || !reflect.DeepEqual(applied.Config.Runners.Profiles["approved"], policy.Requirements{RequiredTags: []string{"linux"}}) {
				t.Fatalf("approved behavior lost: prompt=%q plan=%+v runners=%+v", applied.Prompt, applied.Config.Plan, applied.Config.Runners)
			}
			if !reflect.DeepEqual(applied.Config.Tracker.PriorityMap, cfg.Tracker.PriorityMap) {
				t.Fatalf("default priority map lost: %+v", applied.Config.Tracker.PriorityMap)
			}
			cached, err := ResolveNativePolicy(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			cachedWorkflow, err := cached.Workflow()
			if err != nil || !reflect.DeepEqual(cachedWorkflow, applied) {
				t.Fatalf("cached workflow differs from application: %v", err)
			}
			cachedWorkflow.Config.Runners.Profiles["approved"] = policy.Requirements{RequiredTags: []string{"changed"}}
			cachedWorkflow.Config.Tracker.PriorityMap.Map["changed"] = 1
			copyDescriptor := cached.Descriptor()
			copyDescriptor.Configuration.Behavior[0] = '!'
			copyDescriptor.Requirements.RequiredTags[0] = "changed"
			descriptor.Requirements.RequiredTags[0] = "input changed"
			again, err := cached.Workflow()
			if err != nil || !reflect.DeepEqual(again, applied) || cached.Descriptor().Requirements.RequiredTags[0] != "linux" {
				t.Fatalf("cached policy mutated: %v", err)
			}
			descriptor.Requirements.RequiredTags[0] = "linux"
			resolved, err := ResolvePolicy(applied)
			if err != nil || resolved.Gates != descriptor.Gates || !reflect.DeepEqual(resolved.Requirements, descriptor.Requirements) {
				t.Fatalf("approved metadata lost: gates=%+v requirements=%+v error=%v; want gates=%+v requirements=%+v", resolved.Gates, resolved.Requirements, err, descriptor.Gates, descriptor.Requirements)
			}
		})
	}
}

func TestNativePolicyPriorityMaps(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		priorityMap StringOrMap
	}{
		{"partial map", MapValue(map[string]any{"P0": 1, "P1": 2})},
		{"overridden rank", MapValue(map[string]any{"High": 1})},
		{"unranked priority", MapValue(map[string]any{"P0": 1, "Unranked": nil})},
		{"string", StringOrMap{IsString: true, String: "priority"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := Default()
			cfg.Tracker.Kind = TrackerHubNative
			cfg.Tracker.PriorityMap = test.priorityMap
			descriptor, err := ResolvePolicy(Workflow{Config: cfg})
			if err != nil {
				t.Fatal(err)
			}
			applied, err := ApplyNativePolicy(Workflow{Config: Default()}, descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(applied.Config.Tracker.PriorityMap, test.priorityMap) {
				t.Errorf("applied priority map = %+v; want %+v", applied.Config.Tracker.PriorityMap, test.priorityMap)
			}
			resolved, err := ResolveSharedPolicy(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			if err := resolved.Match(descriptor); err != nil {
				t.Fatal(err)
			}
		})
	}
}
