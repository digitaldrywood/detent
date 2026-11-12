package hubserver

import (
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
)

const nativeLandingCapacityUnknown = -1

const nativePendingLandingClause = `c.work_item_id=i.native_id AND v.id IS NOT NULL
 AND (CASE WHEN EXISTS(SELECT 1 FROM policy_revisions frozen WHERE frozen.scope=i.organization_id||'/'||i.project_id AND frozen.policy_id=v.policy_revision_id) THEN (SELECT frozen.gate_github_pull_request FROM policy_revisions frozen WHERE frozen.scope=i.organization_id||'/'||i.project_id AND frozen.policy_id=v.policy_revision_id) ELSE v.gate_github_pull_request END)=1
 AND c.landed_version_id<>v.id
 AND EXISTS (SELECT 1 FROM native_attempts pending
 WHERE pending.organization_id=i.organization_id AND pending.project_id=i.project_id AND pending.work_item_id=i.native_id
 AND pending.work_item_revision>=i.revision AND pending.dispatch_generation>=i.dispatch_generation
 AND pending.fencing_token=(SELECT max(latest.fencing_token) FROM native_attempts latest
 WHERE latest.organization_id=i.organization_id AND latest.project_id=i.project_id AND latest.work_item_id=i.native_id)
 AND pending.landing_waiting=1
 AND pending.landing_change_id=c.id
 AND pending.landing_version_id=v.id
 AND pending.landing_head_sha=v.claim_head_sha)`

func landingCapacityKey(machine tracker.MachineID, project, session string) string {
	parts := strings.Split(session, "/")
	if len(parts) != 3 || parts[0] != "landing" || parts[1] == "" || parts[2] == "" || project == "" {
		return ""
	}
	return string(machine) + "/" + project + "/" + parts[1]
}
