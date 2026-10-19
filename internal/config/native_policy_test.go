package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

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
		{"unknown behavior", func(_ *policy.Descriptor, b map[string]json.RawMessage) {
			b["RetiredBehavior"] = json.RawMessage(`{"Enabled":true}`)
		}},
		{"missing behavior defaults", func(_ *policy.Descriptor, b map[string]json.RawMessage) {
			delete(b, "Gate")
		}},
		{"older behavior shape", func(d *policy.Descriptor, b map[string]json.RawMessage) {
			d.SourceRevision = strings.Repeat("b", 64)
			b["RetiredBehavior"] = json.RawMessage(`{"Enabled":true}`)
			delete(b, "Gate")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.Tracker.Kind = TrackerHubNative
			cfg.Plan.Enabled = false
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
				t.Fatalf("approved behavior lost: %+v", applied)
			}
			if applied.Config.Gate.Validator.Enabled != descriptor.Gates.Validator || applied.Config.Gate.SecurityAudit.Enabled != descriptor.Gates.SecurityAudit {
				t.Fatalf("approved gates lost: %+v", applied.Config.Gate)
			}
		})
	}
}
