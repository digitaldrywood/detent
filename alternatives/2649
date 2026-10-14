package runnerauth

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
)

const DiagnosticRecordLimit = 256

type DiagnosticAttempt struct {
	Key                 string            `json:"key"`
	IssueID             string            `json:"issue_id"`
	LocalAttemptID      int64             `json:"local_attempt_id,omitempty"`
	NativeAttemptID     string            `json:"native_attempt_id,omitempty"`
	Generation          uint64            `json:"generation,omitempty"`
	Fence               uint64            `json:"fence,omitempty"`
	Stage               string            `json:"stage,omitempty"`
	Membership          []string          `json:"membership"`
	ProviderCompletedAt time.Time         `json:"provider_completed_at,omitzero"`
	HostOperation       string            `json:"host_operation,omitempty"`
	OperationPending    bool              `json:"operation_pending"`
	OperationStartedAt  time.Time         `json:"operation_started_at,omitzero"`
	OperationObservedAt time.Time         `json:"operation_observed_at,omitzero"`
	LatestErrorCode     string            `json:"latest_error_code,omitempty"`
	LatestError         string            `json:"latest_error,omitempty"`
	DeferredAt          time.Time         `json:"deferred_at,omitzero"`
	RetryAt             time.Time         `json:"retry_at,omitzero"`
	RetryAttempt        int               `json:"retry_attempt,omitempty"`
	RecoveryOwner       string            `json:"recovery_owner,omitempty"`
	LeaseRenewedAt      time.Time         `json:"lease_renewed_at,omitzero"`
	LeaseExpiresAt      time.Time         `json:"lease_expires_at,omitzero"`
	Unavailable         map[string]string `json:"unavailable"`
}

type DiagnosticAdmission struct {
	IssueID                string                        `json:"issue_id"`
	ObservedAt             time.Time                     `json:"observed_at"`
	Result                 string                        `json:"result"`
	Predicate              string                        `json:"predicate"`
	RepositoryReservations []string                      `json:"repository_reservations,omitempty"`
	HostLandingOccupancy   int                           `json:"host_landing_occupancy"`
	ProviderRequirement    *providercapacity.Requirement `json:"provider_requirement,omitempty"`
	Unavailable            map[string]string             `json:"unavailable"`
}

type ProjectDiagnostics struct {
	RuntimeObservedAt time.Time             `json:"runtime_observed_at,omitzero"`
	ObservedAt        time.Time             `json:"observed_at"`
	Source            string                `json:"source"`
	RunningBuild      *BuildEvidence        `json:"running_build,omitempty"`
	Records           []DiagnosticAttempt   `json:"records"`
	Admissions        []DiagnosticAdmission `json:"admissions"`
	Counts            map[string]int        `json:"counts"`
	Truncated         bool                  `json:"truncated"`
	Unavailable       map[string]string     `json:"unavailable"`
}

type DiagnosticPage struct {
	RuntimeObservedAt time.Time             `json:"runtime_observed_at,omitzero"`
	ProjectID         string                `json:"project_id"`
	RunnerID          string                `json:"runner_id,omitempty"`
	ObservedAt        time.Time             `json:"observed_at"`
	ReceivedAt        time.Time             `json:"received_at,omitzero"`
	ReadAt            time.Time             `json:"read_at"`
	Status            string                `json:"status"`
	Source            string                `json:"source"`
	RunningBuild      *BuildEvidence        `json:"running_build,omitempty"`
	Records           []DiagnosticAttempt   `json:"records"`
	Admissions        []DiagnosticAdmission `json:"admissions"`
	Counts            map[string]int        `json:"counts,omitempty"`
	NextCursor        string                `json:"next_cursor,omitempty"`
	Truncated         bool                  `json:"truncated"`
	Deduplication     string                `json:"deduplication"`
	Unavailable       map[string]string     `json:"unavailable"`
}

func DiagnosticError(value string) string {
	if value == "" {
		return ""
	}
	for _, public := range []string{"context deadline exceeded", "context canceled", "connection refused", "connection reset by peer", "unexpected EOF", "no such host", "execution authority unavailable", "tracker unavailable"} {
		if strings.Contains(value, public) {
			return public + " [private error text omitted]"
		}
	}
	return "[private error text omitted]"
}

func (d *ProjectDiagnostics) Validate() error {
	if d == nil {
		return nil
	}
	if !diagnosticUnavailable(d.Unavailable) {
		return errors.New("invalid diagnostic unavailability")
	}
	if d.ObservedAt.IsZero() || d.Source != "runner_runtime_and_durable_attempt_owners" || len(d.Records) > DiagnosticRecordLimit || len(d.Admissions) > DiagnosticRecordLimit || len(d.Counts) > 8 || len(d.Unavailable) > 16 {
		return errors.New("invalid runner diagnostic snapshot")
	}
	if d.RunningBuild != nil {
		if err := d.RunningBuild.Validate(); err != nil {
			return err
		}
	}
	for key, n := range d.Counts {
		if !slices.Contains([]string{"running", "claimed", "deferred", "durable_active", "runtime_unsettled", "distinct_records", "native_claim"}, key) {
			return errors.New("invalid diagnostic count source")
		}
		if n < 0 {
			return errors.New("invalid diagnostic count")
		}
	}
	for _, r := range d.Records {
		if !diagnosticUnavailable(r.Unavailable) {
			return errors.New("invalid diagnostic attempt unavailability")
		}
		if !diagnosticIdentifier(r.Key) || !diagnosticIdentifier(r.IssueID) || r.LocalAttemptID < 0 || r.RetryAttempt < 0 || len(r.Membership) > 5 || len(r.Unavailable) > 16 || len(r.LatestError) > 256 || !diagnosticOptional(r.LatestErrorCode) || !diagnosticOptional(r.NativeAttemptID) || !diagnosticOptional(r.Stage) || !diagnosticOptional(r.HostOperation) || !diagnosticOptional(r.RecoveryOwner) {
			return errors.New("invalid diagnostic attempt")
		}
		for _, m := range r.Membership {
			if !slices.Contains([]string{"running", "claimed", "deferred", "durable_active", "native_claim"}, m) {
				return errors.New("invalid diagnostic membership")
			}
		}
	}
	for _, a := range d.Admissions {
		if !diagnosticUnavailable(a.Unavailable) {
			return errors.New("invalid diagnostic admission unavailability")
		}
		for _, id := range a.RepositoryReservations {
			if !diagnosticIdentifier(id) {
				return errors.New("invalid diagnostic reservation")
			}
		}
		if !diagnosticIdentifier(a.IssueID) || a.ObservedAt.IsZero() || !diagnosticIdentifier(a.Predicate) || !diagnosticIdentifier(a.Result) || len(a.Unavailable) > 16 || len(a.RepositoryReservations) > DiagnosticRecordLimit || a.HostLandingOccupancy < 0 {
			return errors.New("invalid diagnostic admission")
		}
		if a.ProviderRequirement != nil {
			if err := a.ProviderRequirement.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func diagnosticIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("_.:-", c) {
			return false
		}
	}
	return true
}

func diagnosticUnavailable(value map[string]string) bool {
	if len(value) > 16 {
		return false
	}
	for key, reason := range value {
		if !diagnosticIdentifier(key) || !slices.Contains([]string{"unavailable", "unrecorded", "not_reported_by_local_owner", "claim_has_no_attempt_identity", "not_probed_by_read", "owner_operation_in_progress", "stopped", "not_resolved_at_this_predicate", "not_in_project_owner"}, reason) {
			return false
		}
	}
	return true
}

func diagnosticOptional(value string) bool { return value == "" || diagnosticIdentifier(value) }

func (d *ProjectDiagnostics) Page(project, runner, issue, attempt, cursor string, limit int, now time.Time) (DiagnosticPage, error) {
	p := DiagnosticPage{ProjectID: project, RunnerID: runner, ReadAt: now, Status: "unavailable", Source: "runner_project_configuration_heartbeat", Records: []DiagnosticAttempt{}, Admissions: []DiagnosticAdmission{}, Deduplication: "local_attempt_id; native_attempt_id when no local identity; claimed-only issue identity is not an attempt", Unavailable: map[string]string{}}
	if d == nil {
		p.Unavailable["snapshot"] = "older_runner_or_unreported"
		return p, nil
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		return p, errors.New("diagnostic page limit exceeds 100")
	}
	p.RuntimeObservedAt = d.RuntimeObservedAt
	p.ObservedAt, p.RunningBuild, p.Counts, p.Truncated = d.ObservedAt, d.RunningBuild, d.Counts, d.Truncated
	p.Status = "available"
	if now.Sub(d.ObservedAt) > HeartbeatTimeout {
		p.Status = "stale"
	}
	for k, v := range d.Unavailable {
		p.Unavailable[k] = v
	}
	type item struct {
		key       string
		record    DiagnosticAttempt
		admission DiagnosticAdmission
		isAttempt bool
	}
	items := make([]item, 0, len(d.Records)+len(d.Admissions))
	for _, r := range d.Records {
		if issue != "" && r.IssueID != issue || attempt != "" && r.NativeAttemptID != attempt && strconv.FormatInt(r.LocalAttemptID, 10) != attempt {
			continue
		}
		items = append(items, item{key: "attempt:" + r.Key, record: r, isAttempt: true})
	}
	if attempt == "" {
		for _, a := range d.Admissions {
			if issue == "" || a.IssueID == issue {
				items = append(items, item{key: "admission:" + a.IssueID, admission: a})
			}
		}
	}
	slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.key, b.key) })
	start := 0
	if cursor != "" {
		prefix, key, ok := strings.Cut(cursor, "|")
		if !ok || prefix != strconv.FormatInt(d.ObservedAt.UnixNano(), 36) {
			return p, errors.New("diagnostic observation changed; restart pagination")
		}
		cursor = key
		found := false
		for i, v := range items {
			if v.key == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			return p, errors.New("diagnostic cursor is unavailable in this observation")
		}
	}
	end := min(len(items), start+limit)
	for _, v := range items[start:end] {
		if v.isAttempt {
			p.Records = append(p.Records, v.record)
		} else {
			p.Admissions = append(p.Admissions, v.admission)
		}
	}
	if end < len(items) {
		p.NextCursor = strconv.FormatInt(d.ObservedAt.UnixNano(), 36) + "|" + items[end-1].key
	}
	if issue != "" && attempt == "" && len(p.Admissions) == 0 && p.NextCursor == "" {
		p.Unavailable["admission_decision"] = "unrecorded_in_bounded_snapshot"
	}
	if attempt != "" && len(items) == 0 {
		p.Unavailable["selected_attempt"] = "not_in_bounded_snapshot"
	}
	return p, nil
}

func (d *ProjectDiagnostics) Redact() {
	if d == nil {
		return
	}
	for i := range d.Records {
		r := &d.Records[i]
		if r.LatestError != "" {
			r.LatestError = DiagnosticError(r.LatestError)
		}
	}
}

func (d *ProjectDiagnostics) Bound() {
	slices.SortFunc(d.Records, func(a, b DiagnosticAttempt) int { return strings.Compare(a.Key, b.Key) })
	slices.SortFunc(d.Admissions, func(a, b DiagnosticAdmission) int { return b.ObservedAt.Compare(a.ObservedAt) })
	if len(d.Records) > DiagnosticRecordLimit {
		d.Truncated = true
		d.Records = d.Records[:DiagnosticRecordLimit]
	}
	if len(d.Admissions) > DiagnosticRecordLimit {
		d.Truncated = true
		d.Admissions = d.Admissions[:DiagnosticRecordLimit]
	}
	d.Redact()
}

func DiagnosticKey(local int64, native, issue string) string {
	if local > 0 {
		return "local:" + strconv.FormatInt(local, 10)
	}
	if native != "" {
		return "native:" + native
	}
	return "issue:" + issue
}
