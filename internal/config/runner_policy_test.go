package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/policy"
)

func TestRunnerPolicyUpgradeKeepsApprovedID(t *testing.T) {
	t.Parallel()
	// Pin the workspace root: the default includes os.TempDir(), which differs
	// across hosts and worker attempts and is itself an approved policy input.
	workflow, err := ParseProjectDefinition(ProjectDefinitionSources{
		WorkflowPath: "WORKFLOW.md",
		Workflow:     []byte("---\ntracker:\n  kind: memory\nworkspace:\n  root: policy-upgrade-workspaces\nworker:\n  ssh_hosts: [local]\ngate:\n  kind: command\n  run: make check-fast\n---\nRun the work.\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The historical snapshot used Unix execution defaults. Pin them so the
	// approval comparison also runs on Windows.
	workflow.Config.Codex.Shell = "sh"
	workflow.Config.Hooks.Shell = "sh"
	// Captured with v0.117.1's config and gate sources. Unlike rebuilding the
	// approval from today's Config type, these constants catch new digest inputs.
	approved := policy.Descriptor{
		SourceRevision: "a4be9735c9116bbea42695a69cb91e0bd3d89fd72875c983f709d4a7cdf86811",
		SourceDigest:   "a4be9735c9116bbea42695a69cb91e0bd3d89fd72875c983f709d4a7cdf86811",
		ConfigDigest:   "f984256fbdabf8b6ff53fa36ec96ce13e435594e630ae36452a6f769d5631a32",
		Gates: policy.Gates{
			Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")),
			AutomatedReview: "required", MergeMethod: "squash",
		},
	}.WithID()
	const approvedID = "policy_512e9d9ac6d0d92d5a6097e1194a94e8f3bfa02a70c3a536486a4a5e1550e89a"
	if approved.ID != approvedID {
		t.Fatalf("historical approval ID = %s, want %s", approved.ID, approvedID)
	}
	for _, test := range []struct {
		name   string
		change func(*Workflow)
		match  bool
	}{
		{"unchanged upgrade with default runner setup", func(*Workflow) {}, true},
		{"configured runner setup", func(w *Workflow) { w.Config.Hooks.RunnerSetup = "scripts/runner-setup.sh" }, false},
		{"host pacing off", func(w *Workflow) {
			w.Config.Agent.RateWindowPacing = RateWindowPacing{Mode: RateWindowPacingOff}.Normalized()
		}, true},
		{"host pacing floor", func(w *Workflow) {
			w.Config.Agent.RateWindowPacing = RateWindowPacing{Mode: RateWindowPacingFloor, FloorPercent: 35}.Normalized()
		}, true},
		{"host pacing freshness", func(w *Workflow) { w.Config.Agent.RateWindowPacing.StaleAfterSeconds = 600 }, true},
		{"model selection", func(w *Workflow) { w.Config.Agents.ModelSelection.NormalModel = new("gpt-6-sol") }, false},
		{"empty extra domains", func(w *Workflow) { w.Config.Worker.ExtraNetworkDomains = []string{} }, true},
		{"project domain grant", func(w *Workflow) {
			w.Config.Worker.ExtraNetworkDomains = []string{"fonts.googleapis.com", "fonts.gstatic.com"}
		}, false},
		{"explicit local binding refusal", func(w *Workflow) { value := false; w.Config.Worker.AllowLocalBinding = &value }, true},
		{"local binding grant", func(w *Workflow) { value := true; w.Config.Worker.AllowLocalBinding = &value }, false},
		{"absent host selection", func(w *Workflow) { w.Config.Worker.HostSelection = "" }, true},
		{"empty host caps", func(w *Workflow) { w.Config.Worker.HostCaps = map[string]int{} }, true},
		{"empty required checks", func(w *Workflow) { w.Config.Gate.RequiredStatusChecks = []string{} }, true},
		{"historical opt-out label", func(w *Workflow) { w.Config.Agent.AutoPromote.OptoutLabel = "requires-human-review" }, true},
		{"zero human review", func(w *Workflow) { w.Config.Review = Review{} }, true},
		{"explicit host preference", func(w *Workflow) { w.Config.Worker.HostSelection = "preference" }, false},
		{"explicit host cap", func(w *Workflow) { w.Config.Worker.HostCaps = map[string]int{"local": 2} }, false},
		{"explicit local status", func(w *Workflow) { w.Config.Gate.LocalStatus = "local-gate" }, false},
		{"explicit required check", func(w *Workflow) { w.Config.Gate.RequiredStatusChecks = []string{"build"} }, false},
		{"explicit human review", func(w *Workflow) { w.Config.Review.Human = true }, false},
		{"custom opt-out label", func(w *Workflow) { w.Config.Agent.AutoPromote.OptoutLabel = "custom-review" }, false},
		{"explicit gate command", func(w *Workflow) { w.Config.Gate.Run = "true" }, false},
		{"explicit workspace root", func(w *Workflow) { w.Config.Workspace.Root = "other-workspaces" }, false},
		{"effective prompt", func(w *Workflow) { w.Prompt += "Different instructions." }, false},
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
	for _, legacyDigest := range []string{
		"64781f2210b0388368f365317cd9eae84869403c9da2d6f517675bcd45582808",
		"433e272c428e2239be1a6ceb1275419d3f43c5a2cdcbf1c6061fecb91c54135a",
		"9042ea475c8d207ccffc8c3edca499fefb0e341264a88163767e486294d3ac4d",
	} {
		legacy := approved
		legacy.ConfigDigest = legacyDigest
		legacy = legacy.WithID()
		if err := legacy.Validate(); err != nil {
			t.Fatal(err)
		}
		current, err := ResolvePolicy(workflow)
		if err != nil {
			t.Fatal(err)
		}
		if current.Match(legacy) == nil {
			t.Fatal("legacy nondefault pacing approval matched a different policy identity")
		}
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
	if current.Match(approved) == nil || !current.Gates.SecurityAudit {
		t.Fatal("explicit security audit matched the policy without an audit")
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
			if proposal.Match(descriptor) == nil {
				t.Fatal("untrusted policy relaxation matched approved descriptor")
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
			Workflow:     []byte("---\ntracker:\n  kind: hub_native\n  api_key: " + host + "-secret\nworkspace:\n  root: " + host + "/worktrees\nworker:\n  ssh_hosts: [local]\nhooks:\n  before_run: " + host + "/isolate.sh\nplan:\n  enabled: true\ngate:\n  run: true\n  validator:\n    enabled: true\nagent:\n  auto_promote:\n    enabled: true\n---\nRun the work.\n"),
		})
		if err != nil {
			t.Fatal(err)
		}
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
		{"other host", func(*Workflow) {}, true},
		{"runtime credential", func(w *Workflow) {
			w.Config.Tracker.APIKey = "runtime-token"
			w.Config.Worker.GitHubToken = "worker-token"
		}, true},
		{"capacity and routing", func(w *Workflow) {
			w.Config.Agent.MaxConcurrentAgents = 7
			w.Config.Worker.SSHHosts = []string{"another-host"}
			w.Config.Worker.HostCaps = map[string]int{"another-host": 2}
		}, true},
		{"planning", func(w *Workflow) { w.Config.Plan.Enabled = false }, false},
		{"validation", func(w *Workflow) { w.Config.Gate.Validator.Enabled = false }, false},
		{"promotion", func(w *Workflow) { w.Config.Agent.AutoPromote.Enabled = false }, false},
		{"instructions", func(w *Workflow) { w.Prompt += "Keep a human review hold." }, false},
		{"admission guidance", func(w *Workflow) { w.SharedPrompt += "Different admission criteria." }, false},
		{"empty network grants", func(w *Workflow) { w.Config.Worker.ExtraNetworkDomains = []string{} }, true},
		{"network grant", func(w *Workflow) { w.Config.Worker.ExtraNetworkDomains = []string{"example.com"} }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			local := air
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
	forged := roundtrip
	forged.Gates.AutoPromote = false
	forged = forged.WithID()
	if err := ValidateSharedPolicy(forged); err == nil {
		t.Fatal("accepted gate metadata inconsistent with shared configuration")
	}
}
