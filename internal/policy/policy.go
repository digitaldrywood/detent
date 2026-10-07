package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

const Schema = 1

var tokenPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

type Requirements struct {
	RequiredTags []string `yaml:"required_tags,omitempty" json:"required_tags,omitempty"`
	RunnerID     string   `yaml:"runner_id,omitempty" json:"runner_id,omitempty"`
	MachineID    string   `yaml:"machine_id,omitempty" json:"machine_id,omitempty"`
}

type Gates struct {
	Kind              string `json:"kind"`
	HumanReview       bool   `json:"human_review,omitempty"`
	PlanEnabled       bool   `json:"plan_enabled"`
	PlanReview        string `json:"plan_review"`
	PlanStopDigest    string `json:"plan_stop_digest"`
	AutoPromote       bool   `json:"auto_promote"`
	AutomatedReview   string `json:"automated_review"`
	RequiredChecks    int    `json:"required_checks"`
	Validator         bool   `json:"validator"`
	SecurityAudit     bool   `json:"security_audit"`
	MergeMethod       string `json:"merge_method"`
	GitHubPullRequest bool   `json:"github_pull_request,omitempty"`
	GitHubCIDigest    string `json:"github_ci_digest,omitempty"`
}

type Descriptor struct {
	Authored       *Authored      `json:"authored,omitempty"`
	Schema         int            `json:"schema"`
	ID             string         `json:"policy_id"`
	SourceRevision string         `json:"source_revision"`
	SourceDigest   string         `json:"source_digest"`
	ConfigDigest   string         `json:"config_digest"`
	Profile        string         `json:"profile,omitempty"`
	Requirements   Requirements   `json:"requirements"`
	Gates          Gates          `json:"gates"`
	Configuration  *Configuration `json:"configuration,omitempty"`
	Workflow       *Workflow      `json:"workflow,omitempty"`
}

type Authored struct {
	Version int               `json:"version"`
	Digest  string            `json:"digest"`
	Files   map[string]string `json:"files,omitempty"`
}

type Configuration struct {
	DefinitionDigest string          `json:"definition_digest,omitempty"`
	Behavior         json.RawMessage `json:"behavior"`
	Prompt           string          `json:"prompt"`
	SharedPrompt     string          `json:"shared_prompt,omitempty"`
	AgentsPrompt     string          `json:"agents_prompt,omitempty"`
}

type State struct {
	OperatorOnly bool     `json:"operator_only,omitempty"`
	Name         string   `json:"name"`
	Terminal     bool     `json:"terminal"`
	Dispatchable bool     `json:"dispatchable"`
	Transitions  []string `json:"transitions"`
}

type Workflow struct {
	Source   string  `json:"source"`
	Revision string  `json:"revision,omitempty"`
	States   []State `json:"states"`
}

func ValidateStates(states []State) error {
	if len(states) == 0 || len(states) > 50 {
		return errors.New("between 1 and 50 workflow states are required")
	}
	names := make(map[string]bool, len(states))
	for _, state := range states {
		if strings.TrimSpace(state.Name) == "" || state.Name != strings.TrimSpace(state.Name) || len(state.Name) > 100 || names[state.Name] || state.Terminal && state.Dispatchable {
			return errors.New("workflow states must be unique and valid")
		}
		if len(state.Transitions) > 50 {
			return errors.New("workflow states may contain at most 50 transitions")
		}
		names[state.Name] = true
	}
	for _, state := range states {
		for _, target := range state.Transitions {
			if !names[target] {
				return errors.New("workflow transition target does not exist")
			}
		}
	}
	return nil
}

type RepositorySource struct {
	Repository             string `json:"repository"`
	Commit                 string `json:"commit"`
	DefaultBranch          string `json:"default_branch,omitempty"`
	DefaultBranchHead      string `json:"default_branch_head,omitempty"`
	DefaultBranchReachable bool   `json:"default_branch_reachable"`
}

type Observation struct {
	Descriptor
	Source *RepositorySource `json:"repository_source,omitempty"`
}

type WorkflowApply struct {
	PreviousDefinition       *Descriptor `json:"previous_definition,omitempty"`
	Definition               *Descriptor `json:"definition,omitempty"`
	ID                       int64       `json:"id,string"`
	Repository               string      `json:"repository"`
	Commit                   string      `json:"commit"`
	PreviousDefinitionDigest string      `json:"previous_definition_digest"`
	DefinitionDigest         string      `json:"definition_digest"`
	RunnerID                 string      `json:"runner_id"`
	AppliedBy                string      `json:"applied_by"`
	AppliedAt                string      `json:"applied_at"`
}

type Approval struct {
	History     []WorkflowApply `json:"history,omitempty"`
	HistoryNext string          `json:"history_next,omitempty"`
	Policy      Descriptor      `json:"policy"`
	ApprovedBy  string          `json:"approved_by"`
	ApprovedAt  string          `json:"approved_at"`
}

type ObservedPolicy struct {
	Source             *RepositorySource `json:"repository_source,omitempty"`
	Policy             Descriptor        `json:"policy"`
	RunnerID           string            `json:"runner_id"`
	RunnerIDs          []string          `json:"runner_ids,omitempty"`
	ObservedAt         string            `json:"observed_at"`
	Conflict           bool              `json:"conflict"`
	PreviouslyApproved bool              `json:"previously_approved"`
}

type Change struct {
	ExpectedID string     `json:"expected_policy_id"`
	Policy     Descriptor `json:"policy"`
}

func ValidToken(value string) bool {
	return tokenPattern.MatchString(value)
}

func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (r Requirements) Normalized() Requirements {
	r.RequiredTags = slices.Clone(r.RequiredTags)
	for i, tag := range r.RequiredTags {
		r.RequiredTags[i] = strings.ToLower(strings.TrimSpace(tag))
	}
	slices.Sort(r.RequiredTags)
	r.RequiredTags = slices.Compact(r.RequiredTags)
	r.RunnerID = strings.TrimSpace(r.RunnerID)
	r.MachineID = strings.TrimSpace(r.MachineID)
	return r
}

func (r Requirements) Validate() error {
	if len(r.RequiredTags) > 32 {
		return errors.New("runner requirements may contain at most 32 tags")
	}
	for _, tag := range r.RequiredTags {
		if !ValidToken(tag) {
			return errors.New("runner tags must be lowercase ASCII tokens of at most 64 characters")
		}
	}
	for prefix, value := range map[string]string{"runner_": r.RunnerID, "machine_": r.MachineID} {
		if value != "" && (!strings.HasPrefix(value, prefix) || !ValidToken(value) || value == prefix) {
			return fmt.Errorf("runner selector must be a stable %s ID", strings.TrimSuffix(prefix, "_"))
		}
	}
	return nil
}

func (r Requirements) Match(runnerID, machineID string, authorizedTags []string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.RunnerID != "" && r.RunnerID != runnerID {
		return errors.New("selector_no_match: required runner ID is not authorized for this executor")
	}
	if r.MachineID != "" && r.MachineID != machineID {
		return errors.New("selector_no_match: required machine ID does not match this host")
	}
	for _, tag := range r.RequiredTags {
		if !slices.Contains(authorizedTags, tag) {
			return fmt.Errorf("selector_no_match: runner lacks administrator-authorized tag %q", tag)
		}
	}
	return nil
}

func (d Descriptor) WithID() Descriptor {
	d.Schema = Schema
	d.ID = ""
	if d.Authored != nil {
		raw, err := json.Marshal(struct {
			Version int    `json:"version"`
			Digest  string `json:"digest"`
		}{d.Authored.Version, d.Authored.Digest})
		if err != nil {
			return Descriptor{}
		}
		d.ID = "policy_" + Digest(raw)
		return d
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return Descriptor{}
	}
	d.ID = "policy_" + Digest(raw)
	return d
}

func (d Descriptor) Validate() error {
	if d.Schema != Schema || d.ID != d.WithID().ID {
		return errors.New("policy_mismatch: invalid policy schema or identity digest")
	}
	if d.Authored != nil && (d.Authored.Version < 1 || !validHash(d.Authored.Digest, 64) || d.SourceDigest != d.Authored.Digest || d.ConfigDigest != d.Authored.Digest || d.SourceRevision != d.Authored.Digest) {
		return errors.New("policy_mismatch: invalid authored policy inputs")
	}
	if d.Authored != nil && len(d.Authored.Files) > 0 {
		if _, present := d.Authored.Files["WORKFLOW.md"]; !present {
			return errors.New("policy_mismatch: authored workflow file is required")
		}
		for name := range d.Authored.Files {
			if !slices.Contains([]string{"WORKFLOW.md", "detent.yaml", "WORKFLOW.local.md", "detent.local.yaml", "AGENTS.md"}, name) {
				return errors.New("policy_mismatch: unknown authored definition file")
			}
		}
	}
	if !validHash(d.SourceDigest, 64) || !validHash(d.ConfigDigest, 64) || (!validHash(d.SourceRevision, 40) && !validHash(d.SourceRevision, 64)) {
		return errors.New("policy_mismatch: source revision and configuration digests are required")
	}
	if d.Profile != "" && !ValidToken(d.Profile) {
		return errors.New("policy_mismatch: invalid runner profile name")
	}
	if d.Configuration != nil && d.Configuration.DefinitionDigest != "" && !validHash(d.Configuration.DefinitionDigest, 64) {
		return errors.New("policy_mismatch: invalid definition digest")
	}
	if d.Workflow != nil {
		if d.Workflow.Revision != "" && !validHash(d.Workflow.Revision, 40) {
			return errors.New("policy_mismatch: invalid repository workflow revision")
		}
		if strings.TrimSpace(d.Workflow.Source) == "" || len(d.Workflow.Source) > 1024 || strings.ContainsAny(d.Workflow.Source, "\r\n") {
			return errors.New("policy_mismatch: repository workflow source is required and limited to 1024 bytes on one line")
		}
		if err := ValidateStates(d.Workflow.States); err != nil {
			return fmt.Errorf("policy_mismatch: %w", err)
		}
	}
	if err := d.Requirements.Validate(); err != nil {
		return err
	}
	g := d.Gates
	if !slices.Contains([]string{"command", "human_review", "artifact"}, g.Kind) ||
		!slices.Contains([]string{"none", "human", "automated", "both"}, g.PlanReview) ||
		!validHash(g.PlanStopDigest, 64) || g.GitHubCIDigest != "" && (!g.GitHubPullRequest || !validHash(g.GitHubCIDigest, 64)) ||
		(g.Kind == "command" && !slices.Contains([]string{"required", "optional", "off"}, g.AutomatedReview)) ||
		(g.Kind != "command" && g.AutomatedReview != "") ||
		!slices.Contains([]string{"squash", "merge", "rebase"}, g.MergeMethod) || g.RequiredChecks < 0 || g.RequiredChecks > 256 {
		return errors.New("policy_mismatch: invalid gate metadata")
	}
	return nil
}

func (d Descriptor) Match(expected Descriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	if d.ID != expected.ID && !d.SameAuthoredInputs(expected) || d.ID == expected.ID && d.Authored != nil && expected.Authored != nil && len(d.Authored.Files) > 0 && len(expected.Authored.Files) > 0 && !sameAuthoredFiles(d, expected) {
		return fmt.Errorf("policy_mismatch: runner policy %s at revision %s differs from approved policy %s at revision %s; load the approved definition and permitted local overrides or request administrator approval", d.ID, d.SourceRevision, expected.ID, expected.SourceRevision)
	}
	return nil
}

func (d Descriptor) SameAuthoredInputs(expected Descriptor) bool {
	if d.Authored == nil || expected.Authored == nil || len(d.Authored.Files) == 0 || len(expected.Authored.Files) == 0 {
		return false
	}
	if (d.Workflow == nil) != (expected.Workflow == nil) {
		return false
	}
	if d.Workflow != nil && d.Workflow.Source != expected.Workflow.Source {
		return false
	}
	return sameAuthoredFiles(d, expected)
}

func validHash(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sameAuthoredFiles(d, expected Descriptor) bool {
	return reflect.DeepEqual(d.Authored.Files, expected.Authored.Files)
}
