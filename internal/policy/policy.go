package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	Kind            string `json:"kind"`
	PlanEnabled     bool   `json:"plan_enabled"`
	PlanReview      string `json:"plan_review"`
	PlanStopDigest  string `json:"plan_stop_digest"`
	AutoPromote     bool   `json:"auto_promote"`
	AutomatedReview string `json:"automated_review"`
	RequiredChecks  int    `json:"required_checks"`
	Validator       bool   `json:"validator"`
	SecurityAudit   bool   `json:"security_audit"`
	MergeMethod     string `json:"merge_method"`
}

type Descriptor struct {
	Schema         int          `json:"schema"`
	ID             string       `json:"policy_id"`
	SourceRevision string       `json:"source_revision"`
	SourceDigest   string       `json:"source_digest"`
	ConfigDigest   string       `json:"config_digest"`
	Profile        string       `json:"profile,omitempty"`
	Requirements   Requirements `json:"requirements"`
	Gates          Gates        `json:"gates"`
}

type Approval struct {
	Policy     Descriptor `json:"policy"`
	ApprovedBy string     `json:"approved_by"`
	ApprovedAt string     `json:"approved_at"`
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
	if !validHash(d.SourceDigest, 64) || !validHash(d.ConfigDigest, 64) || (!validHash(d.SourceRevision, 40) && !validHash(d.SourceRevision, 64)) {
		return errors.New("policy_mismatch: source revision and configuration digests are required")
	}
	if d.Profile != "" && !ValidToken(d.Profile) {
		return errors.New("policy_mismatch: invalid runner profile name")
	}
	if err := d.Requirements.Validate(); err != nil {
		return err
	}
	g := d.Gates
	if !slices.Contains([]string{"command", "human_review", "artifact"}, g.Kind) ||
		!slices.Contains([]string{"none", "human", "automated", "both"}, g.PlanReview) ||
		!validHash(g.PlanStopDigest, 64) ||
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
	if d.ID != expected.ID {
		return fmt.Errorf("policy_mismatch: runner policy %s at revision %s differs from approved policy %s at revision %s; load the approved definition and permitted local overrides or request administrator approval", d.ID, d.SourceRevision, expected.ID, expected.SourceRevision)
	}
	return nil
}

func validHash(value string, length int) bool {
	if len(value) != length || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
