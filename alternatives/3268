package runnerauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

var updateCommit = regexp.MustCompile(`^[a-fA-F0-9]{7,64}$`)

var updateIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)

type BuildEvidence struct {
	Version         string    `json:"version"`
	Commit          string    `json:"commit"`
	Source          string    `json:"source"`
	SHA256          string    `json:"sha256,omitempty"`
	OS              string    `json:"os"`
	Architecture    string    `json:"architecture"`
	VerifiedRelease bool      `json:"verified_release"`
	ObservedAt      time.Time `json:"observed_at"`
}

func (b BuildEvidence) Validate() error {
	if !updateIdentifier.MatchString(b.Version) || !updateIdentifier.MatchString(b.Commit) || !updateIdentifier.MatchString(b.OS) || !updateIdentifier.MatchString(b.Architecture) || b.ObservedAt.IsZero() || !slices.Contains([]string{"release", "homebrew", "go_install", "development", "unknown", "private_patched_source"}, b.Source) {
		return errors.New("invalid build evidence")
	}
	if b.Commit != "none" && b.Commit != "unknown" && !updateCommit.MatchString(b.Commit) {
		return errors.New("invalid source commit")
	}

	if b.SHA256 != "" && !updateDigest(b.SHA256) || b.VerifiedRelease && (b.Source != "release" || !updateDigest(b.SHA256) || len(b.Commit) != 40) {
		return errors.New("invalid build provenance")
	}
	return nil
}

func updateDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32
}

type UpdateRequest struct {
	RequestedAt           time.Time `json:"requested_at"`
	ID                    string    `json:"id"`
	Service               string    `json:"service"`
	ExpectedBuildRevision string    `json:"expected_build_revision"`
	Version               string    `json:"version"`
	Release               bool      `json:"release"`
	FromRelease           bool      `json:"from_release"`
	Urgent                bool      `json:"urgent,omitempty"`
	FollowHub             bool      `json:"follow_hub,omitempty"`
}

func (r UpdateRequest) Validate() error {
	if !updateIdentifier.MatchString(r.ID) || r.Service != "detent" || r.RequestedAt.IsZero() || !r.Urgent && !updateDigest(r.ExpectedBuildRevision) || r.Urgent && (!r.Release || r.ExpectedBuildRevision != "") || r.FollowHub && (!r.Release || r.Urgent) || !updateIdentifier.MatchString(r.Version) || r.FromRelease && !r.Release {
		return errors.New("update requires the selected detent service, observed build revision and release version")
	}
	return nil
}

type UpdateFailure struct {
	Request       UpdateRequest `json:"request"`
	FailureReason string        `json:"failure_reason"`
	ObservedAt    time.Time     `json:"observed_at"`
}

type UpdateReceipt struct {
	ExecutionArtifact bool           `json:"execution_artifact,omitempty"`
	DrainReason       string         `json:"drain_reason,omitempty"`
	PreviousFailure   *UpdateFailure `json:"previous_failure,omitempty"`
	FailureReason     string         `json:"failure_reason,omitempty"`
	Running           *BuildEvidence `json:"running,omitempty"`
	VerifiedTarget    *BuildEvidence `json:"verified_target,omitempty"`
	Request           UpdateRequest  `json:"request"`
	Status            string         `json:"status"`
	Applied           *BuildEvidence `json:"applied,omitempty"`
	ObservedAt        time.Time      `json:"observed_at"`
}

func (r *UpdateReceipt) RetainPreviousFailure(previous *UpdateReceipt) {
	if r == nil || r.PreviousFailure != nil || previous == nil {
		return
	}
	if previous.Request.ID != r.Request.ID && previous.FailureReason != "" {
		r.PreviousFailure = &UpdateFailure{Request: previous.Request, FailureReason: previous.FailureReason, ObservedAt: previous.ObservedAt}
	} else if previous.PreviousFailure != nil {
		failure := *previous.PreviousFailure
		r.PreviousFailure = &failure
	}
}

func (r UpdateReceipt) Validate() error {
	if previous := r.PreviousFailure; previous != nil && (previous.Request.Validate() != nil || previous.FailureReason == "" || len(previous.FailureReason) > 512 || previous.ObservedAt.IsZero()) {
		return errors.New("invalid previous update failure")
	}
	if len(r.DrainReason) > 512 || len(r.FailureReason) > 512 || r.Request.Validate() != nil || r.ObservedAt.IsZero() || !slices.Contains([]string{"applying", "draining", "refused", "uncertain", "applied", "restart_requested", "running"}, r.Status) {
		return errors.New("invalid update receipt")
	}
	if r.Running != nil && (r.Running.Validate() != nil || r.Applied == nil || !r.Running.Matches(*r.Applied)) {
		return errors.New("invalid observed running receipt")
	}
	if r.VerifiedTarget != nil && (r.VerifiedTarget.Validate() != nil || !r.VerifiedTarget.VerifiedRelease || r.VerifiedTarget.Version != r.Request.Version) {
		return errors.New("invalid verified update target")
	}
	if r.Applied != nil && (r.Applied.Validate() != nil || strings.TrimPrefix(r.Applied.Version, "v") != strings.TrimPrefix(r.Request.Version, "v") && !r.AdvancedFollowHubRelease()) || slices.Contains([]string{"applied", "restart_requested", "running"}, r.Status) && r.Applied == nil {
		return errors.New("update receipt requires applied build evidence")
	}
	if r.ExecutionArtifact && (r.Applied == nil || !r.Applied.VerifiedRelease || r.VerifiedTarget != nil) {
		return errors.New("execution artifact receipt requires a verified applied release")
	}
	return nil
}

func (r UpdateReceipt) AdvancedFollowHubRelease() bool {
	if !r.Request.FollowHub || r.Applied == nil || !r.Applied.VerifiedRelease || r.VerifiedTarget != nil {
		return false
	}
	requested := "v" + strings.TrimPrefix(r.Request.Version, "v")
	applied := "v" + strings.TrimPrefix(r.Applied.Version, "v")
	return semver.IsValid(requested) && semver.IsValid(applied) && semver.Compare(applied, requested) > 0
}

func (r UpdateReceipt) ExecutionReady() bool {
	return r.ExecutionArtifact && r.Applied != nil && r.Applied.VerifiedRelease && r.FailureReason == "" && (r.Status == "applied" || r.Status == "running")
}

type UpdateObservation struct {
	Discovery           string         `json:"discovery"`
	Protocol            int            `json:"protocol"`
	Service             string         `json:"service"`
	Revision            string         `json:"revision"`
	Supported           bool           `json:"supported"`
	Pending             bool           `json:"pending"`
	AvailableObservedAt time.Time      `json:"available_observed_at,omitzero"`
	AvailableVersion    string         `json:"available_version,omitempty"`
	Running             BuildEvidence  `json:"running"`
	Receipt             *UpdateReceipt `json:"receipt,omitempty"`
	ObservedAt          time.Time      `json:"observed_at"`
	ReceivedAt          time.Time      `json:"received_at,omitzero"`
}

func (o UpdateObservation) BuildRevision() string {
	b := o.Running
	b.ObservedAt = time.Time{}
	raw, err := json.Marshal(struct {
		Build     BuildEvidence
		Service   string
		Supported bool
		Pending   bool
		Available string
	}{b, o.Service, o.Supported, o.Pending, o.AvailableVersion})
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func (o UpdateObservation) Validate() error {
	if !slices.Contains([]string{"unknown", "available", "up_to_date"}, o.Discovery) {
		return errors.New("invalid update discovery evidence")
	}
	if o.Protocol != 1 || o.Service != "detent" || o.ObservedAt.IsZero() || o.Running.Validate() != nil || !updateDigest(o.Revision) || o.Revision != o.BuildRevision() || o.AvailableVersion != "" && (!updateIdentifier.MatchString(o.AvailableVersion) || o.AvailableObservedAt.IsZero()) {
		return errors.New("invalid installed update observation")
	}
	if o.Receipt != nil && o.Receipt.Validate() != nil {
		return errors.New("invalid installed update receipt")
	}
	if o.Receipt != nil && o.Receipt.Status == "running" && !o.Running.Matches(*o.Receipt.Applied) {
		return errors.New("running receipt does not match the observed process build")
	}
	return nil
}

func (b BuildEvidence) Matches(other BuildEvidence) bool {
	return b.Source != "private_patched_source" && strings.TrimPrefix(b.Version, "v") == strings.TrimPrefix(other.Version, "v") && b.Commit == other.Commit && b.SHA256 != "" && b.SHA256 == other.SHA256 && b.OS == other.OS && b.Architecture == other.Architecture
}

type UpdateView struct {
	RunnerID    string             `json:"runner_id"`
	Revision    int64              `json:"revision"`
	Status      string             `json:"status"`
	Desired     *UpdateRequest     `json:"desired"`
	Observation *UpdateObservation `json:"observation"`
}

func (r Runner) UpdateView(now time.Time) UpdateView {
	view := UpdateView{RunnerID: r.RunnerID, Revision: r.Revision, Status: "unavailable", Desired: r.UpdateRequest, Observation: r.Update}
	if r.Update == nil || r.Health == "revoked" || r.Health == "expired" || r.ConnectionHealth != "online" || now.Before(r.LastHeartbeatAt) || !now.Before(r.LastHeartbeatAt.Add(HeartbeatTimeout)) || now.Before(r.Update.ReceivedAt) || !now.Before(r.Update.ReceivedAt.Add(HeartbeatTimeout)) {
		return view
	}
	if !r.Update.Supported {
		return view
	}
	view.Status = "ready"
	if r.UpdateRequest != nil {
		view.Status = "requested"
		if r.Update.Receipt != nil && r.Update.Receipt.Request == *r.UpdateRequest {
			view.Status = r.Update.Receipt.Status
			if view.Status == "refused" && r.Update.Running.VerifiedRelease && strings.TrimPrefix(r.Update.Running.Version, "v") == strings.TrimPrefix(r.UpdateRequest.Version, "v") {
				view.Status = "running"
			}
			if r.Update.Receipt.Running != nil && !r.Update.Running.Matches(*r.Update.Receipt.Running) {
				view.Status = "drifted"
			}
		}
	}
	return view
}
