package hubserver

import (
	"context"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/operatorfailure"
)

// reportsTrackingFingerprints maps the fingerprint a machine-filed issue would
// carry for a failure to its class: the normalized signature, the signature's
// digest, the class name and the class name's digest.
func reportsTrackingFingerprints(failures []nativeFailure) map[string]string {
	out := map[string]string{}
	for _, f := range failures {
		class := classifyFailure(f.example).Name
		signature := operatorfailure.Normalize(f.example)
		for _, fingerprint := range []string{signature, issueorigin.Fingerprint(signature), class, issueorigin.Fingerprint(class)} {
			if _, taken := out[fingerprint]; !taken && fingerprint != "" {
				out[fingerprint] = class
			}
		}
	}
	return out
}

// reportsTrackingIssues links each failure class to an open issue whose origin
// fingerprint matches one of its signatures. A class with no match keeps no link.
func (s *Service) reportsTrackingIssues(ctx context.Context, scope nativeScope, classes []reportClassTotal, failures []nativeFailure) error {
	fingerprints := reportsTrackingFingerprints(failures)
	if len(fingerprints) == 0 {
		return nil
	}
	rows, err := s.database.reader.QueryContext(ctx, `SELECT i.native_id, i.body FROM issues i JOIN workflow_states ws ON ws.id = i.workflow_state_id
WHERE i.organization_id = ? AND i.project_id = ? AND i.archived = 0 AND ws.terminal = 0
AND (instr(i.body, 'detent-origin') > 0 OR instr(i.body, 'detent-audit-fp') > 0)
ORDER BY i.number LIMIT ?`, scope.organization, scope.project, maxAnalyticsPopulation)
	if err != nil {
		return err
	}
	defer rows.Close()
	tracking := map[string]string{}
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return err
		}
		origin, ok := issueorigin.Parse(body)
		if !ok {
			continue
		}
		if class, match := fingerprints[origin.Fingerprint]; match {
			if _, taken := tracking[class]; !taken {
				tracking[class] = id
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range classes {
		classes[i].TrackingWorkItemID = tracking[classes[i].Class]
	}
	return nil
}
