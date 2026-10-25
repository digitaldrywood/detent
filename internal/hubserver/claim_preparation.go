package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type claimPreparationKey struct{}
type claimWriterKey struct{}

type claimPreparation struct {
	versions    map[string]preparedClaimVersion
	checkpoints map[string]preparedClaimCheckpoint
	details     map[string]preparedClaimDetail
	decisions   bool
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
	summary                tracker.ChangeSummary
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
	for range 2 {
		prepared := &claimPreparation{versions: make(map[string]preparedClaimVersion), checkpoints: make(map[string]preparedClaimCheckpoint), details: make(map[string]preparedClaimDetail)}
		pass := context.WithValue(ctx, claimPreparationKey{}, prepared)
		lease, err := d.claimPreflight(pass, request, query, interval)
		if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil || d.claimReader == nil {
			lease, err = d.claimNextOn(pass, d.reader, &sql.TxOptions{ReadOnly: true}, claimMode{preflight: true}, request, query, interval)
		}
		var selected *claimSelected
		mode := claimMode{}
		switch {
		case errors.As(err, &selected):
			if !prepared.decisions {
				mode.hint = selected
			}
		case err == nil:
			return lease, nil
		case !prepared.decisions && preflightAnswered(err):
			return tracker.Lease{}, err
		case !preflightAnswered(err) && !preflightReadOnly(err):
			return tracker.Lease{}, err
		}
		lease, err = d.claimNextOn(pass, d.db, nil, mode, request, query, interval)
		if !errors.Is(err, errClaimHintStale) {
			return lease, err
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
		return tracker.ChangeVersion{}, errClaimHintStale
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
		return false, errClaimHintStale
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
			return nil, errClaimHintStale
		}
	}
	checkpoint, err := decodeRunCheckpoint(ctx, raw.String)
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
			detail.Summary = value.summary
			return detail, nil
		}
		if claimWriting(ctx) {
			return detail, errClaimHintStale
		}
	}
	detail.Versions = []tracker.ChangeVersion{version}
	detail.Reviews, err = changeRows[tracker.ChangeReview](ctx, q, `SELECT record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='review' ORDER BY sequence`, change.ID, version.ID)
	if err != nil {
		return detail, err
	}
	detail.Checks, err = changeRows[tracker.ChangeCheck](ctx, q, `SELECT record_json FROM change_evidence WHERE change_id=? AND version_id=? AND kind='check' ORDER BY sequence`, change.ID, version.ID)
	if err != nil {
		return detail, err
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
		prepared.details[version.ID] = preparedClaimDetail{sequence: sequence, policyID: current.approval.Policy.ID, reviewID: current.rules.ID, evaluatedAt: now, recheckAt: recheckAt, summary: detail.Summary}
	}
	detail.Reviews, detail.Checks, detail.Versions = nil, nil, nil
	return detail, nil
}
