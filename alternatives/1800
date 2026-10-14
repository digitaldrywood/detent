package hubserver

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeLandingCapacityProject = `CASE WHEN project.profile='native' AND lower(ws.detent_state)='merging'
 AND v.id IS NOT NULL AND COALESCE(json_extract(v.record_json,'$.policy.gates.github_pull_request'),0)=0 THEN i.project_id ELSE '' END`

const nativeLandingCapacityJoins = `LEFT JOIN projects project ON project.id=i.project_id LEFT JOIN workflow_states ws ON ws.id=i.workflow_state_id
 LEFT JOIN change_requests c ON c.organization_id=i.organization_id AND c.project_id=i.project_id AND c.work_item_id=i.native_id AND c.rowid=(SELECT max(latest.rowid) FROM change_requests latest WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id)
 LEFT JOIN change_versions v ON v.id=json_extract(c.record_json,'$.current_version_id')`

func landingCapacityKey(machine tracker.MachineID, project, session string) string {
	parts := strings.Split(session, "/")
	if len(parts) != 3 || parts[0] != "landing" || parts[1] == "" || parts[2] == "" || project == "" {
		return ""
	}
	return string(machine) + "/" + project + "/" + parts[1]
}

func nativeLandingCapacityKey(ctx context.Context, query nativeQueryer, machine tracker.MachineID, session string, id tracker.WorkItemID) (string, error) {
	if landingCapacityKey(machine, "project", session) == "" {
		return "", nil
	}
	var project string
	err := query.QueryRowContext(ctx, `SELECT `+nativeLandingCapacityProject+` FROM issues i `+nativeLandingCapacityJoins+` WHERE i.id=? ORDER BY c.rowid DESC LIMIT 1`, id).Scan(&project)
	if err != nil {
		return "", err
	}
	return landingCapacityKey(machine, project, session), nil
}
