package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type claimPreparationKey struct{}
type claimWriterKey struct{}

type claimPreparation struct {
	versions      map[string]preparedClaimVersion
	checkpoints   map[string]preparedClaimCheckpoint
	details       map[string]preparedClaimDetail
	decisions     []preparedClaimDecision
	hydrating     bool
	references    map[string]preparedClaimReference
	runs          map[string]tracker.NativeRunData
	selected      tracker.WorkItemID
	candidateRows int
	passes        int
	selection     map[tracker.ProjectID]*runnerSelectionProgress
}

const claimPreparationLimit = 100
const claimEvidenceLimit = 1000

type claimHydration struct {
	run func(context.Context, nativeQueryer) error
}

func (*claimHydration) Error() string { return "claim content requires preparation" }

type preparedClaimDecision struct {
	scope nativeScope
	id    tracker.WorkItemID
	item  string
	data  tracker.CollaborationData
}

type preparedClaimReference struct {
	raw       string
	reference tracker.NativeChangeReference
}

type preparedClaimVersion struct {
	version       tracker.ChangeVersion
	source        bool
	sourceChecked bool
	facts         claimPublicationFacts
}

type claimPublicationFacts struct {
	change, version, repository, base, branch, head, policy, source, recorder, provider, external, url string
	attempt, attachment                                                                                string
	sequence                                                                                           int64
	sourceKey, sourceDigest                                                                            string
	sourceBytes, contentBytes                                                                          int64
	sourcePresent                                                                                      bool
}

type preparedClaimCheckpoint struct {
	sequence   int64
	attachment string
	checkpoint *tracker.NativeCheckpoint
}

type preparedClaimDetail struct {
	sequence               int64
	policyID, reviewID     string
	evaluatedAt, recheckAt time.Time
	detail                 tracker.ChangeDetail
}

func claimPrepared(ctx context.Context) *claimPreparation {
	if prepared, ok := ctx.Value(claimPreparationKey{}).(*claimPreparation); ok {
		return prepared
	}
	return nil
}

func claimWriting(ctx context.Context) bool {
	writing, ok := ctx.Value(claimWriterKey{}).(bool)
	return ok && writing
}

func (d *database) claimNativePrepared(ctx context.Context, request tracker.ClaimRequest, query claimCandidateQuery, interval time.Duration) (tracker.Lease, error) {
	prepared := &claimPreparation{versions: make(map[string]preparedClaimVersion), checkpoints: make(map[string]preparedClaimCheckpoint), details: make(map[string]preparedClaimDetail)}
	pass := context.WithValue(ctx, claimPreparationKey{}, prepared)
	for prepared.passes < claimPreparationLimit {
		lease, err := d.prepareClaim(pass, request, query, interval)
		var selected *claimSelected
		mode := claimMode{}
		switch {
		case errors.As(err, &selected):
			mode.hint = selected
		case err == nil:
			return lease, nil
		case preflightAnswered(err):
			if len(prepared.decisions) == 0 && len(prepared.selection) == 0 {
				return tracker.Lease{}, err
			}
			mode.answer = err
		default:
			return tracker.Lease{}, err
		}
		lease, err = d.claimNextOn(pass, d.db, nil, mode, request, query, interval)
		if !errors.Is(err, errClaimHintStale) {
			return lease, err
		}
		for _, progress := range prepared.selection {
			if progress.head == prepared.selected {
				progress.head = 0
				progress.done = false
			}
		}
		prepared.selected = 0
	}
	return tracker.Lease{}, ErrNoClaimableWork
}

func (d *database) prepareClaim(ctx context.Context, request tracker.ClaimRequest, query claimCandidateQuery, interval time.Duration) (tracker.Lease, error) {
	prepared := claimPrepared(ctx)
	if prepared == nil {
		return tracker.Lease{}, errClaimHintStale
	}
	for prepared.passes < claimPreparationLimit {
		prepared.passes++
		candidateRows := prepared.candidateRows
		lease, err := d.claimPreflight(ctx, request, query, interval)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil || d.claimReader == nil {
			lease, err = d.claimNextOn(ctx, d.reader, &sql.TxOptions{ReadOnly: true}, claimMode{preflight: true}, request, query, interval)
		}
		var hydration *claimHydration
		if !errors.As(err, &hydration) {
			return lease, err
		}
		prepared.candidateRows = candidateRows
		prepared.hydrating = true
		err = hydration.run(ctx, d.reader)
		prepared.hydrating = false
		if err != nil {
			return tracker.Lease{}, err
		}
	}
	return tracker.Lease{}, ErrNoClaimableWork
}

func readClaimChange(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (tracker.ChangeRequest, bool, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT c.record_json FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id
WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? ORDER BY c.rowid DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return tracker.ChangeRequest{}, false, nil
	}
	if err != nil {
		return tracker.ChangeRequest{}, false, err
	}
	var change tracker.ChangeRequest
	if err := json.Unmarshal([]byte(raw), &change); err != nil {
		return change, false, err
	}
	return reporterChange(ctx, q, scope, change)
}

func readClaimPublicationFacts(ctx context.Context, q nativeQueryer, change, version string) (claimPublicationFacts, error) {
	var facts claimPublicationFacts
	err := q.QueryRowContext(ctx, `SELECT p.change_id,p.version_id,p.repository,p.base_ref,p.branch,p.head_sha,p.policy_id,p.source_attempt_id,p.recorder_attempt_id,p.provider,p.external_id,p.external_url
FROM native_pr_publications p WHERE p.change_id=? AND p.version_id=?`, change, version).Scan(&facts.change, &facts.version, &facts.repository, &facts.base, &facts.branch, &facts.head, &facts.policy, &facts.source, &facts.recorder, &facts.provider, &facts.external, &facts.url)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return facts, err
	}
	err = q.QueryRowContext(ctx, `SELECT a.id,a.sequence,COALESCE(a.detail_attachment_id,'') FROM native_attempts a JOIN change_requests c ON c.id=?
AND c.organization_id=a.organization_id AND c.project_id=a.project_id AND c.work_item_id=a.work_item_id
WHERE a.finalization_version_id=? ORDER BY a.fencing_token DESC LIMIT 1`, change, version).Scan(&facts.attempt, &facts.sequence, &facts.attachment)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return facts, err
	}
	err = q.QueryRowContext(ctx, `SELECT object_key,object_sha256,object_bytes,content_bytes FROM change_sources WHERE version_id=?`, version).Scan(&facts.sourceKey, &facts.sourceDigest, &facts.sourceBytes, &facts.contentBytes)
	facts.sourcePresent = err == nil
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return facts, err
}

func readClaimVersion(ctx context.Context, q nativeQueryer, change, version string) (tracker.ChangeVersion, error) {
	prepared := claimPrepared(ctx)
	if prepared == nil {
		return readChangeVersion(ctx, q, change, version)
	}
	facts, err := readClaimPublicationFacts(ctx, q, change, version)
	if err != nil {
		return tracker.ChangeVersion{}, err
	}
	if value, ok := prepared.versions[version]; ok && value.version.ChangeID == change && value.facts == facts {
		return value.version, nil
	}
	if claimWriting(ctx) {
		return tracker.ChangeVersion{}, fmt.Errorf("claim version facts changed: %w", errClaimHintStale)
	}
	if !prepared.hydrating {
		return tracker.ChangeVersion{}, &claimHydration{run: func(ctx context.Context, q nativeQueryer) error {
			_, err := readClaimVersion(ctx, q, change, version)
			return err
		}}
	}
	value, err := readChangeVersion(ctx, q, change, version)
	if err != nil {
		return value, err
	}
	prepared.versions[version] = preparedClaimVersion{version: value, facts: facts}
	return value, nil
}

func readClaimSourceStored(ctx context.Context, q nativeQueryer, version tracker.ChangeVersion) (bool, error) {
	prepared := claimPrepared(ctx)
	if prepared == nil {
		return nativeChangeSourceStored(ctx, q, version)
	}
	value, ok := prepared.versions[version.ID]
	if ok && value.sourceChecked {
		return value.source, nil
	}
	if claimWriting(ctx) {
		return false, fmt.Errorf("claim source was not prepared: %w", errClaimHintStale)
	}
	if !prepared.hydrating {
		return false, &claimHydration{run: func(ctx context.Context, q nativeQueryer) error {
			_, err := readClaimSourceStored(ctx, q, version)
			return err
		}}
	}
	stored, err := nativeChangeSourceStored(ctx, q, version)
	if err == nil {
		value.source, value.sourceChecked = stored, true
		prepared.versions[version.ID] = value
	}
	return stored, err
}

func readClaimCheckpoint(ctx context.Context, q nativeQueryer, scope nativeScope, item, attempt string) (*tracker.NativeCheckpoint, error) {
	var raw sql.NullString
	var sequence int64
	var attachment string
	err := q.QueryRowContext(ctx, `SELECT checkpoint_json,sequence,COALESCE(checkpoint_attachment_id,'') FROM native_attempts
WHERE id=? AND organization_id=? AND project_id=? AND work_item_id=?`, attempt, scope.organization, scope.project, item).Scan(&raw, &sequence, &attachment)
	if err != nil || !raw.Valid {
		return nil, err
	}
	prepared := claimPrepared(ctx)
	if prepared != nil {
		if value, ok := prepared.checkpoints[attempt]; ok && value.sequence == sequence && value.attachment == attachment {
			return value.checkpoint, nil
		}
		if claimWriting(ctx) {
			return nil, fmt.Errorf("claim checkpoint identity changed: %w", errClaimHintStale)
		}
	}
	if prepared != nil && !prepared.hydrating {
		return nil, &claimHydration{run: func(ctx context.Context, q nativeQueryer) error {
			checkpoint, err := decodeRunCheckpoint(ctx, q, raw.String)
			if err == nil {
				prepared.checkpoints[attempt] = preparedClaimCheckpoint{sequence: sequence, attachment: attachment, checkpoint: checkpoint}
			}
			return err
		}}
	}
	checkpoint, err := decodeRunCheckpoint(ctx, q, raw.String)
	if err == nil && prepared != nil {
		prepared.checkpoints[attempt] = preparedClaimCheckpoint{sequence: sequence, attachment: attachment, checkpoint: checkpoint}
	}
	return checkpoint, err
}

func readClaimLandingDetail(ctx context.Context, q nativeQueryer, scope nativeScope, change tracker.ChangeRequest, version tracker.ChangeVersion, now time.Time) (tracker.ChangeDetail, error) {
	detail := tracker.ChangeDetail{Change: change}
	var sequence int64
	err := q.QueryRowContext(ctx, `SELECT COALESCE((SELECT sequence FROM change_evidence WHERE change_id=? AND version_id=? ORDER BY sequence DESC LIMIT 1),0)`, change.ID, version.ID).Scan(&sequence)
	if err != nil {
		return detail, err
	}
	current, err := readChangeSummaryPolicy(ctx, q, scope)
	if err != nil {
		return detail, err
	}
	prepared := claimPrepared(ctx)
	if prepared != nil {
		if value, ok := prepared.details[version.ID]; ok && value.sequence == sequence && value.policyID == current.approval.Policy.ID && value.reviewID == current.rules.ID && !now.Before(value.evaluatedAt) && (value.recheckAt.IsZero() || now.Before(value.recheckAt)) {
			detail = value.detail
			detail.Change = change
			return detail, nil
		}
		if claimWriting(ctx) {
			return detail, fmt.Errorf("claim evidence or policy changed: %w", errClaimHintStale)
		}
	}
	if prepared != nil && !prepared.hydrating {
		return detail, &claimHydration{run: func(ctx context.Context, q nativeQueryer) error {
			_, err := readClaimLandingDetail(ctx, q, scope, change, version, now)
			return err
		}}
	}
	detail.Versions = []tracker.ChangeVersion{version}
	detail.Reviews, err = readClaimReviews(ctx, q, change.ID, version.ID)
	if err != nil {
		return detail, err
	}
	for _, expected := range version.Checks {
		checks, readErr := claimEvidenceRows[tracker.ChangeCheck](ctx, q, `SELECT record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='check' AND source_key=? ORDER BY sequence`, change.ID, version.ID, expected.CheckRunID)
		if readErr != nil {
			return detail, readErr
		}
		detail.Checks = append(detail.Checks, checks...)
	}
	detail.Summary = current.summarize(detail, now, false)
	var recheckAt time.Time
	for _, expected := range version.Checks {
		for _, result := range detail.Checks {
			if result.VersionID != version.ID || result.CheckRunID != expected.CheckRunID {
				continue
			}
			for _, boundary := range []time.Time{result.ReceivedAt, result.CompletedAt.Add(time.Duration(expected.MaxAgeSeconds) * time.Second)} {
				if boundary.After(now) && (recheckAt.IsZero() || boundary.Before(recheckAt)) {
					recheckAt = boundary
				}
			}
		}
	}
	if prepared != nil {
		prepared.details[version.ID] = preparedClaimDetail{sequence: sequence, policyID: current.approval.Policy.ID, reviewID: current.rules.ID, evaluatedAt: now, recheckAt: recheckAt, detail: detail}
	}
	return detail, nil
}

func claimEvidenceRows[T any](ctx context.Context, q nativeQueryer, statement string, args ...any) ([]T, error) {
	args = append(args, claimEvidenceLimit+1)
	rows, err := q.QueryContext(ctx, statement+" LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []string
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		records = append(records, raw)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	if len(records) > claimEvidenceLimit {
		return nil, fmt.Errorf("claim evidence exceeds bounded preparation: %w", errClaimHintStale)
	}
	result := make([]T, len(records))
	for i, raw := range records {
		if err := decodeStorageJSON(ctx, []byte(raw), &result[i]); err != nil {
			return nil, err
		}
		if err := hydrateChangeRecord(ctx, q, &result[i]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func readClaimReference(ctx context.Context, q nativeQueryer, scope nativeScope, item string) (tracker.NativeChangeReference, error) {
	var ref tracker.NativeChangeReference
	var version, head sql.NullString
	var raw string
	err := q.QueryRowContext(ctx, `SELECT c.id,c.claim_version_id,c.claim_head_sha,c.record_json FROM change_requests c JOIN change_issue_links l ON l.change_id=c.id
WHERE c.organization_id=? AND c.project_id=? AND l.work_item_id=? ORDER BY c.rowid DESC LIMIT 1`, scope.organization, scope.project, item).Scan(&ref.ChangeID, &version, &head, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ref, nil
	}
	if err != nil {
		return ref, err
	}
	if version.Valid && head.Valid {
		ref.VersionID, ref.HeadSHA = version.String, head.String
		return ref, nil
	}
	prepared := claimPrepared(ctx)
	if prepared != nil {
		if value, ok := prepared.references[ref.ChangeID]; ok && value.raw == raw {
			return value.reference, nil
		}
		if claimWriting(ctx) {
			return ref, fmt.Errorf("claim reference changed: %w", errClaimHintStale)
		}
		if !prepared.hydrating {
			return ref, &claimHydration{run: func(ctx context.Context, q nativeQueryer) error {
				_, err := readClaimReference(ctx, q, scope, item)
				return err
			}}
		}
	}
	ref.VersionID, ref.HeadSHA, err = claimChangeIdentity(ctx, q, ref.ChangeID, raw)
	if err == nil && prepared != nil {
		if prepared.references == nil {
			prepared.references = make(map[string]preparedClaimReference)
		}
		prepared.references[ref.ChangeID] = preparedClaimReference{raw: raw, reference: ref}
	}
	return ref, err
}

func writePreparedClaimDecisions(ctx context.Context, tx *sql.Tx, selected *claimSelected, now time.Time) error {
	prepared := claimPrepared(ctx)
	if prepared == nil {
		return errClaimHintStale
	}
	if err := writeRunnerSelectionProgress(ctx, tx, prepared.selection); err != nil {
		return err
	}
	for _, value := range prepared.decisions {
		if selected != nil && value.id == selected.id {
			continue
		}
		var revision int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM issues WHERE id=? AND organization_id=? AND project_id=?`, value.id, value.scope.organization, value.scope.project).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if tracker.Revision(revision) != value.data.Decision.WorkItemRevision {
			continue
		}
		reference, err := readClaimReference(ctx, tx, value.scope, value.item)
		if errors.Is(err, errClaimHintStale) {
			continue
		}
		if err != nil {
			return err
		}
		var previous tracker.NativeChangeReference
		if value.data.Change != nil {
			previous = *value.data.Change
		}
		if reference != previous {
			continue
		}
		if err := recordNativeSchedulingOutcome(ctx, tx, &value.scope, value.id, *value.data.Decision, now); err != nil {
			return err
		}
	}
	return nil
}

func readClaimReviews(ctx context.Context, q nativeQueryer, change, version string) ([]tracker.ChangeReview, error) {
	var unknown bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM change_evidence INDEXED BY change_evidence_claim_unknown WHERE change_id=? AND version_id=? AND kind='review' AND claim_review_current IS NULL)`, change, version).Scan(&unknown); err != nil {
		return nil, err
	}
	if unknown {
		return claimEvidenceRows[tracker.ChangeReview](ctx, q, `SELECT record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' ORDER BY sequence`, change, version)
	}
	type record struct {
		sequence int64
		raw      string
	}
	records := make(map[int64]string)
	for _, selection := range []struct {
		kind, decision string
		current        bool
	}{
		{kind: "implementation", current: true}, {kind: "validator", current: true}, {kind: "audit", current: true},
		{kind: "audit", decision: "changes_requested"}, {kind: "landing_audit", decision: "changes_requested"},
		{kind: "human", decision: "approved", current: true}, {kind: "human", decision: "changes_requested", current: true},
	} {
		statement := `SELECT sequence,record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' AND claim_review_kind=?`
		args := []any{change, version, selection.kind}
		if selection.current {
			statement += ` AND claim_review_current=1`
		}
		if selection.decision != "" {
			statement += ` AND claim_review_decision=?`
			args = append(args, selection.decision)
		} else {
			statement += ` AND claim_review_actor=''`
		}
		var value record
		err := q.QueryRowContext(ctx, statement+` ORDER BY sequence DESC LIMIT 1`, args...).Scan(&value.sequence, &value.raw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		records[value.sequence] = value.raw
	}
	keys := make([]int64, 0, len(records))
	for sequence := range records {
		keys = append(keys, sequence)
	}
	slices.Sort(keys)
	reviews := make([]tracker.ChangeReview, 0, len(keys))
	for _, sequence := range keys {
		var review tracker.ChangeReview
		if err := decodeStorageJSON(ctx, []byte(records[sequence]), &review); err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}
