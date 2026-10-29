package hubserver

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/digitaldrywood/detent/internal/progresscheck"
)

func (s *Service) progressHealthReceipts(ctx context.Context, scope nativeScope, item string) ([]progresscheck.Receipt, error) {
	rows, err := s.database.reader.QueryContext(ctx, `SELECT f.id,f.signal,f.class,f.subject_json,f.summary,f.evidence_json
 FROM health_finding_projects p JOIN health_findings f ON f.id=p.finding_id
 WHERE p.project_id=? AND f.organization_id=? AND f.resolved_at IS NULL
 ORDER BY f.rowid DESC LIMIT ?`, scope.project, scope.organization, healthReadLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var direct, shared []progresscheck.Receipt
	for rows.Next() {
		var id, signal, class, subjectRaw, summary, evidence string
		if err := rows.Scan(&id, &signal, &class, &subjectRaw, &summary, &evidence); err != nil {
			return nil, err
		}
		var subject healthSubject
		if err := json.Unmarshal([]byte(subjectRaw), &subject); err != nil {
			return nil, err
		}
		if subject.Kind == "work_item" && subject.ID != item {
			continue
		}
		var scoped healthEvidence
		if err := json.Unmarshal([]byte(evidence), &scoped); err != nil {
			return nil, err
		}
		raw, err := json.Marshal(scopeHealthEvidence(scoped, scope.project))
		if err != nil {
			return nil, err
		}
		receipt := progresscheck.Receipt{ID: id, Step: "health", Outcome: signal, Detail: boundedFailureText(class+"\n"+subject.Kind+" "+subject.ID+"\n"+summary+"\n"+string(raw), 2048)}
		if subject.Kind == "work_item" {
			if len(direct) < 6 {
				direct = append(direct, receipt)
			}
		} else if len(shared) < 6 {
			shared = append(shared, receipt)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	return append(direct, shared...)[:min(6, len(direct)+len(shared))], nil
}
