package hubserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

type healthDetector struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func (s *Service) startHealthDetector() {
	ctx, cancel := context.WithCancel(context.Background())
	s.healthDetector = &healthDetector{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(s.healthDetector.done)
		ticker := time.NewTicker(healthInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := s.evaluateTenantHealth(ctx); err != nil && ctx.Err() == nil {
					s.config.Logger.Warn("health detector evaluation failed", "error", err)
				}
			}
		}
	}()
}

func (s *Service) stopHealthDetector() {
	if s.healthDetector != nil {
		s.healthDetector.cancel()
		<-s.healthDetector.done
	}
}

func (s *Service) evaluateTenantHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if s.requestMetrics != nil {
		if err := s.requestMetrics.flush(ctx, s.config.now()); err != nil {
			s.config.Logger.Warn("flush tenant request metrics", "error", err)
		}
	}
	rows, err := s.database.reader.QueryContext(ctx, "SELECT id FROM organizations ORDER BY id LIMIT ?", healthReadLimit+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	organizations := []tracker.OrganizationID{}
	for rows.Next() {
		var id tracker.OrganizationID
		if err := rows.Scan(&id); err != nil {
			return err
		}
		organizations = append(organizations, id)
		if len(organizations) > healthReadLimit {
			return errHealthReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var failures error
	for _, organization := range organizations {
		failures = errors.Join(failures, s.evaluateOrganizationHealth(ctx, organization, time.Now().UTC()))
	}
	return failures
}

func (s *Service) evaluateOrganizationHealth(ctx context.Context, organization tracker.OrganizationID, now time.Time) error {
	snapshot, err := s.readHealthSnapshot(ctx, organization, now)
	if err != nil {
		return err
	}
	unavailable := snapshot.BaselineUnavailable
	if unavailable == nil {
		unavailable = []healthBaselineUnavailable{}
	}
	raw, err := json.Marshal(unavailable)
	if err != nil {
		return err
	}
	tx, err := s.database.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO health_detector_ticks(organization_id,last_tick_at,baseline_unavailable_json) VALUES(?,?,?)
 ON CONFLICT(organization_id) DO UPDATE SET baseline_unavailable_json=excluded.baseline_unavailable_json`, organization, formatHubTime(now), string(raw)); err != nil {
		return err
	}
	findings := evaluateHealth(now, snapshot)
	if s.requestMetrics != nil && organization == s.requestMetrics.organization {
		findings = append(findings, s.requestMetrics.findings(now)...)
	}
	return s.commitHealthEvaluation(ctx, tx, organization, now, findings)
}

func (s *Service) readHealthSnapshot(ctx context.Context, organization tracker.OrganizationID, now time.Time) (healthSnapshot, error) {
	tx, err := s.database.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return healthSnapshot{}, err
	}
	defer tx.Rollback()
	return readHealthSnapshot(ctx, tx, organization, now)
}

func writeHealthEvaluation(ctx context.Context, tx *sql.Tx, organization tracker.OrganizationID, now time.Time, findings []healthFinding) ([]string, error) {
	fingerprints := make([]string, 0, len(findings))
	transitions := []string{}
	for _, f := range findings {
		fingerprints = append(fingerprints, f.Fingerprint)
		var id string
		err := tx.QueryRowContext(ctx, "SELECT id FROM health_findings WHERE organization_id=? AND fingerprint=? AND resolved_at IS NULL", organization, f.Fingerprint).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			cutoff := now.Add(-healthReopenWindow)
			err = tx.QueryRowContext(ctx, `SELECT id FROM health_findings WHERE organization_id=? AND fingerprint=? AND resolved_at>=? AND julianday(resolved_at)>=julianday(?) ORDER BY resolved_at DESC LIMIT 1`, organization, f.Fingerprint, cutoff.UTC().Format("2006-01-02T15:04:05"), formatHubTime(cutoff)).Scan(&id)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}

		subject, err := json.Marshal(f.Subject)
		if err != nil {
			return nil, err
		}
		projects, err := json.Marshal(f.Projects)
		if err != nil {
			return nil, err
		}
		evidence, err := json.Marshal(f.Evidence)
		if err != nil {
			return nil, err
		}
		if id == "" {
			id = newNativeID("finding")
			transitions = append(transitions, id)
			_, err = tx.ExecContext(ctx, `INSERT INTO health_findings(id,organization_id,fingerprint,signal,class,subject_json,projects_json,opened_at,last_seen_at,severity,summary,next_action,evidence_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, id, organization, f.Fingerprint, f.Signal, f.Class, string(subject), string(projects), formatHubTime(now), formatHubTime(now), f.Severity, f.Summary, f.NextAction, string(evidence))
			if err == nil && f.Severity == "attention" {
				err = enqueueSlackOpened(ctx, tx, organization, id, now)
			}
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE health_findings SET last_seen_at=?,resolved_at=NULL,projects_json=?,summary=?,next_action=?,evidence_json=? WHERE id=? AND organization_id=?`, formatHubTime(now), string(projects), f.Summary, f.NextAction, string(evidence), id, organization)
		}
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(fingerprints)
	if err != nil {
		return nil, err
	}
	if err := enqueueSlackResolved(ctx, tx, organization, raw, now); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `UPDATE health_findings SET resolved_at=? WHERE organization_id=? AND resolved_at IS NULL AND fingerprint NOT IN (SELECT value FROM json_each(?)) RETURNING id`, formatHubTime(now), organization, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		transitions = append(transitions, id)
		if len(transitions) > healthReadLimit {
			return nil, errHealthReadLimit
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO health_detector_ticks(organization_id,last_tick_at) VALUES(?,?) ON CONFLICT(organization_id) DO UPDATE SET last_tick_at=excluded.last_tick_at`, organization, formatHubTime(now))
	return transitions, err
}
