package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunnerPolicyCompatibility(t *testing.T) {
	t.Parallel()
	shared := "tracker:\n  kind: memory\nrunners:\n  profile: build\n  profiles:\n    build:\n      required_tags: [Linux, linux, gpu]\n      machine_id: machine_abc\ngate:\n  kind: human_review\nagent:\n  auto_promote:\n    enabled: false\ndeliverable:\n  merge_method: rebase\n"
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
			if descriptor.Gates.AutoPromote || descriptor.Gates.Kind != "human_review" || descriptor.Gates.MergeMethod != "rebase" {
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
