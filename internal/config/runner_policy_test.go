package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
)

func TestRunnerPolicyCanonicalInputs(t *testing.T) {
	t.Parallel()
	workflow, err := ParseProjectDefinition(ProjectDefinitionSources{
		WorkflowPath: "WORKFLOW.md",
		Workflow:     []byte("---\ntracker:\n  kind: memory\nworkspace:\n  root: policy-upgrade-workspaces\nworker:\n  ssh_hosts: [local]\ngate:\n  kind: command\n  run: make check-fast\n---\nRun the work.\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*Workflow)
		match  bool
	}{
		{"unchanged upgrade with default runner setup", func(*Workflow) {}, true},
		{"changed skill prompt cap", func(w *Workflow) { w.Config.Agent.Skills.MaxSkillsInPrompt++ }, true},
		{"configured runner setup", func(w *Workflow) { w.Config.Hooks.RunnerSetup = "scripts/runner-setup.sh" }, true},
		{"host pacing off", func(w *Workflow) {
			w.Config.Agent.RateWindowPacing = RateWindowPacing{Mode: RateWindowPacingOff}.Normalized()
		}, true},
		{"host pacing floor", func(w *Workflow) {
			w.Config.Agent.RateWindowPacing = RateWindowPacing{Mode: RateWindowPacingFloor, FloorPercent: 35}.Normalized()
		}, true},
		{"host pacing freshness", func(w *Workflow) { w.Config.Agent.RateWindowPacing.StaleAfterSeconds = 600 }, true},
		{"model selection", func(w *Workflow) { w.Config.Agents.ModelSelection.NormalModel = new("gpt-6-sol") }, true},
		{"empty extra domains", func(w *Workflow) { w.Config.Worker.ExtraNetworkDomains = []string{} }, true},
		{"project domain grant", func(w *Workflow) {
			w.Config.Worker.ExtraNetworkDomains = []string{"fonts.googleapis.com", "fonts.gstatic.com"}
		}, true},
		{"explicit local binding refusal", func(w *Workflow) { value := false; w.Config.Worker.AllowLocalBinding = &value }, true},
		{"local binding grant", func(w *Workflow) { value := true; w.Config.Worker.AllowLocalBinding = &value }, true},
		{"absent host selection", func(w *Workflow) { w.Config.Worker.HostSelection = "" }, true},
		{"empty host caps", func(w *Workflow) { w.Config.Worker.HostCaps = map[string]int{} }, true},
		{"empty required checks", func(w *Workflow) { w.Config.Gate.RequiredStatusChecks = []string{} }, true},
		{"historical opt-out label", func(w *Workflow) { w.Config.Agent.AutoPromote.OptoutLabel = "requires-human-review" }, true},
		{"disabled default followups", func(w *Workflow) { w.Config.Agent.Followups.Enabled = false }, true},
		{"zero human review", func(w *Workflow) { w.Config.Review = Review{} }, true},
		{"explicit host preference", func(w *Workflow) { w.Config.Worker.HostSelection = "preference" }, true},
		{"explicit host cap", func(w *Workflow) { w.Config.Worker.HostCaps = map[string]int{"local": 2} }, true},
		{"explicit local status", func(w *Workflow) { w.Config.Gate.LocalStatus = "local-gate" }, true},
		{"explicit required check", func(w *Workflow) { w.Config.Gate.RequiredStatusChecks = []string{"build"} }, true},
		{"explicit human review", func(w *Workflow) { w.Config.Review.Human = true }, true},
		{"custom opt-out label", func(w *Workflow) { w.Config.Agent.AutoPromote.OptoutLabel = "custom-review" }, true},
		{"explicit gate command", func(w *Workflow) { w.Config.Gate.Run = "true" }, true},
		{"explicit workspace root", func(w *Workflow) { w.Config.Workspace.Root = "other-workspaces" }, true},
		{"effective prompt", func(w *Workflow) { w.Prompt += "Different instructions." }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := workflow
			test.change(&candidate)
			optoutLabel := candidate.Config.Agent.AutoPromote.OptoutLabel
			pacing := candidate.Config.Agent.RateWindowPacing
			current, err := ResolvePolicy(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if (current.ConfigDigest == approved.ConfigDigest) != test.match {
				t.Fatalf("config digest = %s, want match %t with %s", current.ConfigDigest, test.match, approved.ConfigDigest)
			}
			if err := current.Match(approved); (err == nil) != test.match {
				t.Fatalf("upgraded policy match = %v, want match %t (config digest %s)", err, test.match, current.ConfigDigest)
			}
			if candidate.Config.Agent.RateWindowPacing != pacing {
				t.Fatal("policy resolution changed runtime pacing")
			}
			if candidate.Config.Agent.AutoPromote.OptoutLabel != optoutLabel {
				t.Fatal("policy resolution changed the runtime opt-out label")
			}
		})
	}
}

func TestRunnerPolicyAuthoredDefinition(t *testing.T) {
	t.Parallel()
	for _, tracker := range []string{"memory", "hub_native"} {
		t.Run(tracker, func(t *testing.T) {
			sources := ProjectDefinitionSources{Workflow: []byte("Run the work.\n"), Config: []byte("schema: 1\ntracker:\n  kind: " + tracker + "\ngate:\n  run: echo 1\n"), HasConfig: true}
			load := func(sources ProjectDefinitionSources) policy.Descriptor {
				t.Helper()
				workflow, err := ParseProjectDefinition(sources)
				if err != nil {
					t.Fatal(err)
				}
				descriptor, err := ResolvePolicy(workflow)
				if err != nil {
					t.Fatal(err)
				}
				if tracker == "hub_native" {
					resolved, err := ResolveSharedPolicy(descriptor)
					if err != nil || !reflect.DeepEqual(resolved.Authored, descriptor.Authored) {
						t.Fatalf("authored snapshot is not idempotent: %+v %v", resolved.Authored, err)
					}
				}
				return descriptor
			}
			approved := load(sources)
			for _, test := range []struct {
				name   string
				change func(*ProjectDefinitionSources)
				match  bool
			}{
				{"same files", func(*ProjectDefinitionSources) {}, true},
				{"key order and whitespace", func(s *ProjectDefinitionSources) {
					s.Config = []byte("gate: {run: echo 1}\ntracker: {kind: " + tracker + "}\nschema: 1\n\n")
				}, true},
				{"authored YAML aliases", func(s *ProjectDefinitionSources) {
					s.Config = []byte("schema: 1\ntracker:\n  kind: " + tracker + "\nplan: &options\n  enabled: true\ngate:\n  run: true\n  validator: *options\n")
				}, false},
				{"instance-only authored tuning", func(s *ProjectDefinitionSources) {
					s.Config = append([]byte(string(s.Config)), []byte("agent:\n  max_concurrent_agents: 2\nserver:\n  port: 3030\nworker:\n  ssh_hosts: [local]\n")...)
				}, tracker == "hub_native"},
				{"one byte config change", func(s *ProjectDefinitionSources) {
					s.Config = []byte(strings.ReplaceAll(string(s.Config), "echo 1", "echo 2"))
				}, false},
				{"one byte prompt change", func(s *ProjectDefinitionSources) { s.Workflow = []byte("Run the work!\n") }, false},
				{"authored gate", func(s *ProjectDefinitionSources) {
					s.Config = []byte(strings.ReplaceAll(string(s.Config), "run: echo 1", "run: false"))
				}, false},
				{"authored selector", func(s *ProjectDefinitionSources) {
					s.Config = append([]byte(string(s.Config)), []byte("runners:\n  profile: restricted\n  profiles:\n    restricted:\n      machine_id: machine_privileged\n")...)
				}, false},
				{"authored checks", func(s *ProjectDefinitionSources) {
					s.Config = []byte(strings.ReplaceAll(string(s.Config), "run: echo 1", "run: echo 1\n  required_status_checks: [build]"))
				}, false},
				{"authored comment", func(s *ProjectDefinitionSources) {
					s.Config = append([]byte(string(s.Config)), []byte("# Operator guidance\n")...)
				}, false},
				{"explicit default", func(s *ProjectDefinitionSources) {
					s.Config = append([]byte(string(s.Config)), []byte("review:\n  human: false\n")...)
				}, false},
				{"local override", func(s *ProjectDefinitionSources) {
					s.LocalConfig = []byte("schema: 1\nreview:\n  human: true\n")
					s.HasLocalConfig = true
				}, false},
				{"local prompt", func(s *ProjectDefinitionSources) {
					s.LocalWorkflow = []byte("Operator instruction.\n")
					s.HasLocalWorkflow = true
				}, false},
				{"agent guidance independent of defaults", func(s *ProjectDefinitionSources) { s.Agents = []byte("Human guidance.\n"); s.HasAgents = true }, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					candidate := sources
					test.change(&candidate)
					actual := load(candidate)
					if (actual.ID == approved.ID) != test.match || (actual.ConfigDigest == approved.ConfigDigest) != test.match || (actual.SourceRevision == approved.SourceRevision) != test.match || (actual.Match(approved) == nil) != test.match {
						t.Fatalf("identity = %+v, want match %t", actual.Authored, test.match)
					}
				})
			}
		})
	}
}

func TestRunnerPolicyEquivalentOptoutRetainsSecurityAudit(t *testing.T) {
	t.Parallel()
	workflow, err := ParseProjectDefinition(ProjectDefinitionSources{
		WorkflowPath: "WORKFLOW.md",
		Workflow:     []byte("---\ntracker:\n  kind: github\n  api_key: example-token\n  repository: example/repo\n  github_status_source: label\n---\nRun the work.\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	approved, err := ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Config.Gate.SecurityAudit.Enabled = true
	current, err := ResolvePolicy(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if current.Match(approved) != nil || !current.Gates.SecurityAudit {
		t.Fatal("runtime projection changed identity or lost the audit")
	}
}

func TestRunnerPolicyCompatibility(t *testing.T) {
	t.Parallel()
	shared := "tracker:\n  kind: memory\nrunners:\n  profile: build\n  profiles:\n    build:\n      required_tags: [Linux, linux, gpu]\n      machine_id: machine_abc\ngate:\n  kind: human_review\nagent:\n  auto_promote:\n    enabled: false\ndeliverable:\n  merge_method: rebase\n  github_pull_request: true\n"
	for _, test := range []struct {
		name         string
		split, local bool
	}{
		{"legacy", false, false}, {"legacy local", false, true}, {"split", true, false}, {"split local external root", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			workflowPath := filepath.Join(root, "WORKFLOW.md")
			prompt := "Private workflow prose.\n"
			workflow := "---\n" + shared + "---\n" + prompt
			if test.split {
				workflow = prompt
				if err := os.WriteFile(DefinitionPath(workflowPath), []byte("schema: 1\n"+shared), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(workflowPath, []byte(workflow), 0o600); err != nil {
				t.Fatal(err)
			}
			if test.local {
				local := "runners:\n  profiles:\n    build:\n      required_tags: []\n      machine_id: ''\n"
				path, content := LocalWorkflowPath(workflowPath), "---\n"+local+"---\nPrivate overlay.\n"
				if test.split {
					path, content = LocalDefinitionPath(workflowPath), "schema: 1\n"+local
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := LoadProjectDefinition(workflowPath)
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := ResolvePolicy(loaded)
			if err != nil {
				t.Fatal(err)
			}
			if descriptor.Gates.AutoPromote || descriptor.Gates.Kind != "human_review" || descriptor.Gates.MergeMethod != "rebase" || !descriptor.Gates.GitHubPullRequest {
				t.Fatalf("lost repository gates: %#v", descriptor.Gates)
			}
			if test.local && (len(descriptor.Requirements.RequiredTags) != 0 || descriptor.Requirements.MachineID != "") {
				t.Fatalf("explicit clearing lost: %#v", descriptor.Requirements)
			}
			if !test.local && (len(descriptor.Requirements.RequiredTags) != 2 || descriptor.Requirements.MachineID != "machine_abc") {
				t.Fatalf("requirements lost: %#v", descriptor.Requirements)
			}
			raw, err := json.Marshal(descriptor)
			if err != nil {
				t.Fatal(err)
			}
			for _, private := range []string{"Private", root, "make check", "WORKFLOW.md"} {
				if strings.Contains(string(raw), private) {
					t.Fatalf("policy uploaded private content %q", private)
				}
			}
			changed := loaded
			changed.Config.Agent.AutoPromote.Enabled = true
			proposal, err := ResolvePolicy(changed)
			if err != nil {
				t.Fatal(err)
			}
			if proposal.Match(descriptor) != nil {
				t.Fatal("runtime projection changed authored identity")
			}
		})
	}
}

func TestRunnerProfileValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, yaml string
		valid      bool
	}{
		{"unknown profile", "profile: missing", false},
		{"display name", "profiles: {build: {runner_id: Build-Mac}}", false},
		{"path name", "profiles: {'../private': {}}", false},
		{"command tag", "profiles: {build: {required_tags: ['make check']}}", false},
		{"unused profile", "profiles: {build: {required_tags: [linux]}}", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workflow, err := ParseWorkflow([]byte("---\ntracker:\n  kind: memory\nrunners:\n  " + test.yaml + "\n---\nPrompt\n"))
			if err == nil {
				err = workflow.Config.Validate()
			}
			if (err == nil) != test.valid {
				t.Fatalf("validation = %v, want valid %t", err, test.valid)
			}
		})
	}
}

func TestNativeSharedPolicy(t *testing.T) {
	t.Parallel()
	load := func(host string) Workflow {
		t.Helper()
		workflow, err := ParseProjectDefinition(ProjectDefinitionSources{
			WorkflowPath: host + "/WORKFLOW.md",
			Workflow:     []byte("---\ntracker:\n  kind: hub_native\n  api_key: " + host + "-secret\nworkspace:\n  root: " + host + "/worktrees\nworker:\n  ssh_hosts: [local]\nhooks:\n  before_run: " + host + "/isolate.sh\nplan:\n  enabled: true\ngate:\n  run: true\n  validator:\n    enabled: true\nagent:\n  auto_promote:\n    enabled: true\nserver:\n  kanban:\n    allowed_transitions:\n      Todo: [In Progress]\n---\nRun the work.\n"),
		})
		if err != nil {
			t.Fatal(err)
		}
		workflow.Config.Server.Kanban.AllowedTransitions = map[string][]string{"Todo": {"In Progress"}}
		return workflow
	}
	pro := load("pro")
	air := load("air")
	if pro.SourceHash == air.SourceHash {
		t.Fatal("fixture must contain different host source bytes")
	}
	approved, err := ResolvePolicy(pro)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateSharedPolicy(approved); err != nil {
		t.Fatal(err)
	}
	if !approved.Gates.PlanEnabled || !approved.Gates.Validator || !approved.Gates.AutoPromote {
		t.Fatalf("combined gates missing: %+v", approved.Gates)
	}
	for _, tt := range []struct {
		name   string
		change func(*Workflow)
		match  bool
	}{
		{"other host", func(w *Workflow) { *w = air }, true},
		{"runtime credential", func(w *Workflow) {
			w.Config.Tracker.APIKey = "runtime-token"
			w.Config.Worker.GitHubToken = "worker-token"
		}, true},
		{"capacity and routing", func(w *Workflow) {
			w.Config.Agent.MaxConcurrentAgents = 7
			w.Config.Worker.SSHHosts = []string{"another-host"}
			w.Config.Worker.HostCaps = map[string]int{"another-host": 2}
		}, true},
		{"planning", func(w *Workflow) { w.Config.Plan.Enabled = false }, true},
		{"transitions", func(w *Workflow) { w.Config.Server.Kanban.AllowedTransitions = map[string][]string{"Todo": {"Done"}} }, true},
		{"validation", func(w *Workflow) { w.Config.Gate.Validator.Enabled = false }, true},
		{"promotion", func(w *Workflow) { w.Config.Agent.AutoPromote.Enabled = false }, true},
		{"instructions", func(w *Workflow) { w.Prompt += "Keep a human review hold." }, true},
		{"admission guidance", func(w *Workflow) { w.SharedPrompt += "Different admission criteria." }, true},
		{"empty network grants", func(w *Workflow) { w.Config.Worker.ExtraNetworkDomains = []string{} }, true},
		{"network grant", func(w *Workflow) { w.Config.Worker.ExtraNetworkDomains = []string{"example.com"} }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local := pro
			tt.change(&local)
			descriptor, err := ResolvePolicy(local)
			if err != nil {
				t.Fatal(err)
			}
			if (descriptor.Match(approved) == nil) != tt.match {
				t.Fatalf("policy match = %v, want %t", descriptor.Match(approved), tt.match)
			}
			resolved, err := ApplyNativePolicy(local, approved)
			if err != nil {
				t.Fatal(err)
			}
			resolved.Config = resolved.Config.WithWorkerDefaults(WorkerDefaults{AllowLocalBinding: new(true)})
			if resolved.Config.KanbanTransitionAllowed("Todo", "Done") || !resolved.Config.KanbanTransitionAllowed("Todo", "In Progress") {
				t.Fatal("shared configuration did not retain the approved transition policy")
			}
			actual, err := ResolvePolicy(resolved)
			if err != nil || actual.Match(approved) != nil {
				t.Fatalf("shared policy resolution = %+v, %v", actual, err)
			}
			if resolved.Config.Workspace.Root != local.Config.Workspace.Root || resolved.Config.Hooks != local.Config.Hooks || resolved.Config.Tracker.APIKey != local.Config.Tracker.APIKey || resolved.Config.Worker.GitHubToken != local.Config.Worker.GitHubToken || resolved.Config.Agent.MaxConcurrentAgents != local.Config.Agent.MaxConcurrentAgents {
				t.Fatal("shared configuration replaced host configuration")
			}
		})
	}
	raw, err := json.Marshal(approved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pro-secret") || strings.Contains(string(raw), "pro/worktrees") || strings.Contains(string(raw), "isolate.sh") {
		t.Fatal("shared descriptor exposed host configuration")
	}
	var roundtrip policy.Descriptor
	if err := json.Unmarshal(raw, &roundtrip); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSharedPolicy(roundtrip); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		change  func(*policy.Descriptor)
		invalid bool
	}{
		{"gate projection", func(d *policy.Descriptor) { d.Gates.AutoPromote = false }, false},
		{"nil configuration gate projection", func(d *policy.Descriptor) { d.Configuration = nil; d.Gates.AutoPromote = false }, false},
		{"nil configuration selector projection", func(d *policy.Descriptor) { d.Configuration = nil; d.Requirements.MachineID = "machine_privileged" }, false},
		{"nil configuration profile projection", func(d *policy.Descriptor) { d.Configuration = nil; d.Profile = "privileged" }, false},
		{"nil configuration with invented digest", func(d *policy.Descriptor) {
			d.Configuration = nil
			d.Authored.Digest = policy.Digest([]byte("invented identity"))
			d.SourceDigest, d.ConfigDigest, d.SourceRevision = d.Authored.Digest, d.Authored.Digest, d.Authored.Digest
			d.Gates.AutoPromote = false
		}, true},
		{"copied digest with changed authored gate", func(d *policy.Descriptor) {
			d.Authored.Files["WORKFLOW.md"] = strings.ReplaceAll(d.Authored.Files["WORKFLOW.md"], "run: true", "run: false")
		}, true},
		{"copied digest with changed local grant", func(d *policy.Descriptor) {
			d.Authored.Files["detent.local.yaml"] = "schema: 1\nworker:\n  allow_local_binding: true\n"
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := roundtrip
			authored := *roundtrip.Authored
			authored.Files = make(map[string]string)
			for name, content := range roundtrip.Authored.Files {
				authored.Files[name] = content
			}
			candidate.Authored = &authored
			test.change(&candidate)
			candidate = candidate.WithID()
			resolved, err := ResolveSharedPolicy(candidate)
			if test.invalid {
				if err == nil {
					t.Fatal("changed authored source accepted copied digest")
				}
				if _, err := ApplyNativePolicy(pro, candidate); err == nil {
					t.Fatal("applied changed authored source with copied digest")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resolved.ID != approved.ID || resolved.Profile != approved.Profile || !reflect.DeepEqual(resolved.Requirements, approved.Requirements) || !reflect.DeepEqual(resolved.Gates, approved.Gates) {
				t.Fatal("projection replaced authoritative authored policy")
			}
			applied, err := ApplyNativePolicy(pro, candidate)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := ResolvePolicy(applied)
			if err != nil || actual.ID != approved.ID || actual.Profile != approved.Profile || !reflect.DeepEqual(actual.Requirements, approved.Requirements) || !reflect.DeepEqual(actual.Gates, approved.Gates) {
				t.Fatalf("applied projection replaced authored policy: %+v %v", actual, err)
			}
		})
	}
}
