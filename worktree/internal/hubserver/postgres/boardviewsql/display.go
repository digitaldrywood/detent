package boardviewsql

import (
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const DisplayColumns = `b.card_display_ready,b.card_display_exact,b.card_provenance_provider IS NOT NULL,coalesce(b.card_provenance_provider,''),coalesce(b.card_provenance_external_id,''),coalesce(b.card_provenance_author_id,''),coalesce(b.card_provenance_author_display_name,''),coalesce(b.card_provenance_created_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_updated_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_observed_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_principal_id,''),coalesce(b.card_title,(SELECT title FROM issues i WHERE i.organization_id=b.organization_id AND i.native_id=b.work_item_id)),coalesce(b.card_actor_kind,(SELECT coalesce(actor_kind,'') FROM issues i WHERE i.organization_id=b.organization_id AND i.native_id=b.work_item_id)),coalesce(b.card_actor_via_key,''),coalesce(b.card_actor_display_name,''),coalesce(b.card_actor_agent,''),coalesce(b.card_reason,''),coalesce(b.card_status_line,''),coalesce(b.card_attempt_id,''),coalesce(b.card_attempt_count,0),coalesce(b.card_attempt_status,''),coalesce(b.card_attempt_runner_id,''),coalesce(b.card_attempt_machine_id,''),coalesce(b.card_attempt_started_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_attempt_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_execution_id,''),coalesce(b.card_execution_running,false),coalesce(b.card_execution_stage,''),coalesce(b.card_execution_runner_id,''),b.card_execution_started_at,coalesce(b.card_execution_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_id,''),coalesce(b.card_change_title,''),coalesce(b.card_change_status,''),coalesce(b.card_change_external_provider,''),coalesce(b.card_change_external_id,''),coalesce(b.card_change_external_url,''),coalesce(b.card_attempt_identity_role,''),coalesce(b.card_attempt_identity_backend,''),coalesce(b.card_attempt_identity_model,''),coalesce(b.card_attempt_identity_effort,''),coalesce(b.card_execution_identity_role,''),coalesce(b.card_execution_identity_backend,''),coalesce(b.card_execution_identity_model,''),coalesce(b.card_execution_identity_effort,''),coalesce(b.issue_number,(SELECT number FROM issues i WHERE i.organization_id=b.organization_id AND i.native_id=b.work_item_id)),b.card_change_base_status IS NOT NULL,coalesce(b.card_change_version_id,''),coalesce(b.card_change_policy_id,''),coalesce(b.card_change_review_policy_id,''),coalesce(b.card_change_base_status,'')`

func DisplayTargets(d *viewstore.BoardDisplay) ([]any, func()) {
	var provenance tracker.Provenance
	var hasProvenance bool
	var a tracker.NativeBoardAttempt
	var e tracker.NativeBoardExecution
	var c tracker.NativeBoardChange
	var x tracker.ChangeExternalReference
	var ai, ei tracker.NativeExecutionIdentity
	var changeProvenance viewstore.BoardChangeProvenance
	var hasChangeProvenance bool
	return []any{&d.Ready, &d.Exact, &hasProvenance, &provenance.Provider, &provenance.ExternalID, &provenance.AuthorID, &provenance.AuthorDisplayName, &provenance.CreatedAt, &provenance.UpdatedAt, &provenance.ObservedAt, &d.ChangePrincipalID, &d.Title, &d.Actor.Kind, &d.Actor.ViaKey, &d.Actor.DisplayName, &d.Actor.Agent, &d.Reason, &d.StatusLine, &a.ID, &a.Count, &a.Status, &a.RunnerID, &a.MachineID, &a.StartedAt, &a.ExpiresAt, &e.ID, &e.Running, &e.Stage, &e.RunnerID, &e.StartedAt, &e.ExpiresAt, &c.ID, &c.Title, &c.Status, &x.Provider, &x.ID, &x.URL, &ai.Role, &ai.Backend, &ai.Model, &ai.ReasoningEffort, &ei.Role, &ei.Backend, &ei.Model, &ei.ReasoningEffort, &d.Number, &hasChangeProvenance, &changeProvenance.VersionID, &changeProvenance.PolicyID, &changeProvenance.ReviewPolicyID, &changeProvenance.BaseStatus}, func() {
		if hasChangeProvenance {
			d.ChangeProvenance = &changeProvenance
		}
		if hasProvenance {
			d.Provenance = &provenance
		}
		if ai != (tracker.NativeExecutionIdentity{}) {
			a.Identity = &ai
		}
		if ei != (tracker.NativeExecutionIdentity{}) {
			e.Identity = &ei
		}
		if x.Provider != "" {
			c.External = &x
		}
		if a.ID != "" {
			d.Attempt = &a
		}
		if e.ID != "" {
			d.Execution = &e
		}
		if c.ID != "" {
			d.Change = &c
		}
	}
}
