package config

import (
	"encoding/json"
	"errors"
	"maps"
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
			execution, err := NewExecutionConfiguration(cachedWorkflow, descriptor, ResolveCloudModelSelection(ModelSelection{}, ModelSelection{}))
			if err != nil {
				t.Fatal(err)
			}
			executed, err := ApplyExecutionConfiguration(Workflow{Config: Default()}, descriptor, execution)
			if err != nil || !reflect.DeepEqual(nativeProjectBehavior(executed.Config), nativeProjectBehavior(applied.Config)) || executed.Prompt != applied.Prompt {
				t.Fatalf("execution configuration changed resolved behavior: %v", err)
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

func TestNativeExecutionUsesApprovedPrompt(t *testing.T) {
	t.Parallel()
	sources := ProjectDefinitionSources{
		Workflow: []byte("  Approved instructions. \t\r\n\n"), Config: []byte("schema: 1\ntracker:\n  kind: hub_native\nbacklog_admission:\n  require_effort: true\n  effort_file: AGENTS.md\n  effort_section: Effort\n"), HasConfig: true,
		Agents: []byte("Approved effort guidance.\r\n"), HasAgents: true,
	}
	approved, err := ParseHistoricalProjectDefinition(sources)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := ResolvePolicy(approved)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewExecutionConfiguration(approved, descriptor, ResolveCloudModelSelection(ModelSelection{}, ModelSelection{})); err != nil {
		t.Fatalf("publish parsed prompt: %v", err)
	}
	hub, err := ApplyNativePolicy(Workflow{Config: Default()}, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if hub.AgentsPrompt != string(sources.Agents) {
		t.Fatalf("lost approved AGENTS.md guidance: %q", hub.AgentsPrompt)
	}
	execution, err := NewExecutionConfiguration(hub, descriptor, ResolveCloudModelSelection(ModelSelection{}, ModelSelection{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, overlay string }{
		{"clean runner", ""},
		{"conflicting runner", "Unapproved instructions.\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			local := sources
			local.LocalWorkflow, local.HasLocalWorkflow = []byte(test.overlay), test.overlay != ""
			runner, err := ParseHistoricalProjectDefinition(local)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := ApplyExecutionConfiguration(runner, descriptor, execution)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Prompt != string(sources.Workflow) || resolved.Config.Policy.ID != descriptor.ID {
				t.Fatalf("executed prompt %q with policy %s", resolved.Prompt, resolved.Config.Policy.ID)
			}
		})
	}
	for _, name := range []string{"WORKFLOW.md", "AGENTS.md"} {
		t.Run(name+" trailing whitespace", func(t *testing.T) {
			altered := descriptor
			a := *descriptor.Authored
			a.Files = maps.Clone(a.Files)
			a.Files[name] += " "
			altered.Authored = &a
			if _, err := ApplyNativePolicy(Workflow{Config: Default()}, altered); !errors.Is(err, ErrNativeAuthoredMismatch) {
				t.Fatalf("changed file accepted under the original digest: %v", err)
			}
		})
	}
	for _, field := range []string{"prompt", "agents_prompt"} {
		for _, prompt := range []string{"", "Changed instructions."} {
			changedWorkflow := hub
			if field == "prompt" {
				changedWorkflow.Prompt = prompt
			} else {
				changedWorkflow.AgentsPrompt = prompt
			}
			if _, err := NewExecutionConfiguration(changedWorkflow, descriptor, ResolveCloudModelSelection(ModelSelection{}, ModelSelection{})); !errors.Is(err, ErrNativeAuthoredMismatch) {
				t.Errorf("Hub accepted changed %s: %v", field, err)
			}
			var changed executionValues
			if err := json.Unmarshal(execution.Values, &changed); err != nil {
				t.Fatal(err)
			}
			if field == "prompt" {
				changed.Prompt = prompt
			} else {
				changed.AgentsPrompt = prompt
			}
			raw, err := json.Marshal(changed)
			if err != nil {
				t.Fatal(err)
			}
			candidate := *execution
			candidate.Values = raw
			if _, err := ApplyExecutionConfiguration(approved, descriptor, &candidate); !errors.Is(err, ErrNativeAuthoredMismatch) {
				t.Errorf("runner accepted changed %s: %v", field, err)
			}
		}
	}
}

func TestAuthoredPromptReaderVersions(t *testing.T) {
	t.Parallel()
	sources := ProjectDefinitionSources{
		Workflow: []byte("  A blue notebook. \t\n\n"), HasConfig: true,
		Config: []byte("schema: 1\ntracker:\n  kind: hub_native\nbacklog_admission:\n  require_effort: true\n  effort_file: AGENTS.md\n  effort_section: Effort\n"),
		Agents: []byte("A green notebook. \t\n"), HasAgents: true,
	}
	for _, version := range []int{1, 2, 3} {
		w, err := ParseHistoricalProjectDefinition(sources)
		if err != nil {
			t.Fatal(err)
		}
		w.Authored.Version = version
		old, err := ResolvePolicy(w)
		if err != nil {
			t.Fatal(err)
		}
		old.Authored.Files["WORKFLOW.md"] += " \t\n"
		for _, descriptor := range []policy.Descriptor{old, old.WithPinnedFiles()} {
			clean := Workflow{Config: Default()}
			stored, err := ReadNativePolicy(descriptor)
			if err != nil || stored.Prompt != "A blue notebook." || stored.AgentsPrompt != "A green notebook." {
				t.Fatalf("v%d descriptor: prompt %q, guidance %q, error %v", descriptor.Authored.Version, stored.Prompt, stored.AgentsPrompt, err)
			}
			execution, err := NewExecutionConfiguration(stored, descriptor, ResolveCloudModelSelection(ModelSelection{}, ModelSelection{}))
			if err != nil {
				t.Fatal(err)
			}
			var values executionValues
			if err := json.Unmarshal(execution.Values, &values); err != nil {
				t.Fatal(err)
			}
			values.Prompt, values.SharedPrompt = string(sources.Workflow), string(sources.Workflow)
			values.AgentsPrompt = ""
			execution.Version = 1
			execution.Values, err = json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := ApplyExecutionConfiguration(clean, descriptor, execution)
			if err != nil || actual.Prompt != stored.Prompt || actual.SharedPrompt != stored.Prompt || actual.AgentsPrompt != stored.AgentsPrompt {
				t.Fatalf("v%d execution: prompt %q, guidance %q, error %v", descriptor.Authored.Version, actual.Prompt, actual.AgentsPrompt, err)
			}
		}
	}
}

func TestLegacyNativePromptEvidence(t *testing.T) {
	t.Parallel()
	full, err := ResolvePolicy(Workflow{Config: Default().ForNativeTracker(), Prompt: "Retained instructions."})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := ResolvePolicy(Workflow{Config: Default().ForNativeTracker()})
	if err != nil {
		t.Fatal(err)
	}
	changed := *full.Configuration
	changed.Prompt = "Different instructions."
	for _, test := range []struct {
		name            string
		authored        bool
		base            policy.Descriptor
		snapshot        *policy.Configuration
		wantUnavailable bool
	}{
		{"no files or configuration", false, full, nil, true},
		{"authored digest alone", true, full, nil, true},
		{"behavior without prompt", true, full, &policy.Configuration{Behavior: json.RawMessage("{}")}, true},
		{"redacted behavior", true, full, &policy.Configuration{Behavior: json.RawMessage("null"), Prompt: full.Configuration.Prompt}, true},
		{"retained execution prompt", true, full, full.Configuration, false},
		{"unbound execution prompt", true, full, &changed, true},
		{"approved empty prompt", false, empty, empty.Configuration, false},
		{"lost execution prompt", false, full, empty.Configuration, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			d := test.base
			if test.authored {
				d.Authored = &policy.Authored{Version: 1, Digest: d.SourceDigest}
			}
			d.Configuration = test.snapshot
			d = d.WithID()
			local := Workflow{Config: Default(), Prompt: "Private local text."}
			got, err := ApplyNativePolicy(local, d)
			if test.wantUnavailable {
				if !errors.Is(err, ErrNativePromptUnavailable) {
					t.Fatalf("missing prompt evidence returned %v", err)
				}
				return
			}
			if err != nil || got.Prompt != test.snapshot.Prompt {
				t.Fatalf("retained prompt %q: %v", got.Prompt, err)
			}
			execution, err := NewExecutionConfiguration(got, d, ResolveCloudModelSelection(ModelSelection{}, ModelSelection{}))
			if err != nil {
				t.Fatal(err)
			}
			running, err := ApplyExecutionConfiguration(local, d, execution)
			if err != nil || running.Prompt != test.snapshot.Prompt || running.Config.Policy.ID != d.ID {
				t.Fatalf("execution prompt %q, policy %q: %v", running.Prompt, running.Config.Policy.ID, err)
			}
		})
	}
}

func TestLegacyApprovedContractWithoutExecutablePrompt(t *testing.T) {
	t.Parallel()
	source := ProjectDefinitionSources{
		Workflow: []byte("## Issue Contract\n\n- Acceptance criteria\n- Rollback evidence\n"),
		Config:   []byte("schema: 1\ntracker:\n  kind: hub_native\ngate:\n  run: make verify\n  validator:\n    enabled: true\n"), HasConfig: true,
		LocalWorkflow: []byte("Required private instructions."), HasLocalWorkflow: true,
	}
	w, err := ParseProjectDefinition(source)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ResolvePolicy(w)
	if err != nil {
		t.Fatal(err)
	}
	digestOnly := d
	authored := *d.Authored
	authored.Files = nil
	digestOnly.Authored = &authored
	digestOnly.Configuration = &policy.Configuration{Behavior: json.RawMessage("null"), SharedPrompt: string(source.Workflow)}
	for _, test := range []struct {
		name       string
		descriptor policy.Descriptor
	}{
		{"local instruction digest", d},
		{"stored contract and gates", digestOnly},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract, err := ResolvePolicyIssueContract(test.descriptor)
			if err != nil || !reflect.DeepEqual(contract.Sections, []string{"Acceptance criteria", "Rollback evidence"}) {
				t.Fatalf("stored contract sections = %v, error = %v", contract.Sections, err)
			}
			read, err := ReadNativePolicy(test.descriptor)
			if err != nil || !read.Config.Gate.Validator.Enabled {
				t.Fatalf("stored validator gate = %t, error = %v", read.Config.Gate.Validator.Enabled, err)
			}
			if _, err := ApplyNativePolicy(Workflow{Config: Default()}, test.descriptor); !errors.Is(err, ErrNativePromptUnavailable) {
				t.Fatalf("execution error = %v", err)
			}
		})
	}
}
