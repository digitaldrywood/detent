package postgres

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

const boardDisplayColumns = `b.card_display_ready,b.card_display_exact,b.card_provenance_provider IS NOT NULL,coalesce(b.card_provenance_provider,''),coalesce(b.card_provenance_external_id,''),coalesce(b.card_provenance_author_id,''),coalesce(b.card_provenance_author_display_name,''),coalesce(b.card_provenance_created_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_updated_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_provenance_observed_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_principal_id,''),coalesce(b.card_title,(SELECT title FROM issues i WHERE i.organization_id=b.organization_id AND i.native_id=b.work_item_id)),coalesce(b.card_actor_kind,(SELECT coalesce(actor_kind,'') FROM issues i WHERE i.organization_id=b.organization_id AND i.native_id=b.work_item_id)),coalesce(b.card_actor_via_key,''),coalesce(b.card_actor_display_name,''),coalesce(b.card_actor_agent,''),coalesce(b.card_reason,''),coalesce(b.card_status_line,''),coalesce(b.card_attempt_id,''),coalesce(b.card_attempt_count,0),coalesce(b.card_attempt_status,''),coalesce(b.card_attempt_runner_id,''),coalesce(b.card_attempt_machine_id,''),coalesce(b.card_attempt_started_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_attempt_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_execution_id,''),coalesce(b.card_execution_running,false),coalesce(b.card_execution_stage,''),coalesce(b.card_execution_runner_id,''),b.card_execution_started_at,coalesce(b.card_execution_expires_at,'0001-01-01 00:00:00+00'::timestamptz),coalesce(b.card_change_id,''),coalesce(b.card_change_title,''),coalesce(b.card_change_status,''),coalesce(b.card_change_external_provider,''),coalesce(b.card_change_external_id,''),coalesce(b.card_change_external_url,''),coalesce(b.card_attempt_identity_role,''),coalesce(b.card_attempt_identity_backend,''),coalesce(b.card_attempt_identity_model,''),coalesce(b.card_attempt_identity_effort,''),coalesce(b.card_execution_identity_role,''),coalesce(b.card_execution_identity_backend,''),coalesce(b.card_execution_identity_model,''),coalesce(b.card_execution_identity_effort,''),coalesce(b.issue_number,(SELECT number FROM issues i WHERE i.organization_id=b.organization_id AND i.native_id=b.work_item_id))`

func boardDisplayTargets(d *viewstore.BoardDisplay) ([]any, func()) {
	var provenance tracker.Provenance
	var hasProvenance bool
	var a tracker.NativeBoardAttempt
	var e tracker.NativeBoardExecution
	var c tracker.NativeBoardChange
	var x tracker.ChangeExternalReference
	var ai, ei tracker.NativeExecutionIdentity
	return []any{&d.Ready, &d.Exact, &hasProvenance, &provenance.Provider, &provenance.ExternalID, &provenance.AuthorID, &provenance.AuthorDisplayName, &provenance.CreatedAt, &provenance.UpdatedAt, &provenance.ObservedAt, &d.ChangePrincipalID, &d.Title, &d.Actor.Kind, &d.Actor.ViaKey, &d.Actor.DisplayName, &d.Actor.Agent, &d.Reason, &d.StatusLine, &a.ID, &a.Count, &a.Status, &a.RunnerID, &a.MachineID, &a.StartedAt, &a.ExpiresAt, &e.ID, &e.Running, &e.Stage, &e.RunnerID, &e.StartedAt, &e.ExpiresAt, &c.ID, &c.Title, &c.Status, &x.Provider, &x.ID, &x.URL, &ai.Role, &ai.Backend, &ai.Model, &ai.ReasoningEffort, &ei.Role, &ei.Backend, &ei.Model, &ei.ReasoningEffort, &d.Number}, func() {
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
func (s *session) putBoardDisplay(ctx context.Context, b viewstore.BoardRecord) error {
	d := b.Display
	var provenance tracker.Provenance
	var provider *string
	if d.Provenance != nil {
		provenance = *d.Provenance
		provider = &provenance.Provider
	}
	var a tracker.NativeBoardAttempt
	var e tracker.NativeBoardExecution
	var c tracker.NativeBoardChange
	var x tracker.ChangeExternalReference
	var ai, ei tracker.NativeExecutionIdentity
	if d.Attempt != nil {
		a = *d.Attempt
		if a.Identity != nil {
			ai = *a.Identity
		}
	}
	if d.Execution != nil {
		e = *d.Execution
		if e.Identity != nil {
			ei = *e.Identity
		}
	}
	if d.Change != nil {
		c = *d.Change
		if c.External != nil {
			x = *c.External
		}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE native_board_items SET card_display_ready=$39,card_display_exact=$40,card_provenance_provider=$41,card_provenance_external_id=$42,card_provenance_author_id=$43,card_provenance_author_display_name=$44,card_provenance_created_at=$45,card_provenance_updated_at=$46,card_provenance_observed_at=$47,card_change_principal_id=$38,card_title=$4,card_actor_kind=$5,card_actor_via_key=$6,card_actor_display_name=$7,card_actor_agent=$8,card_reason=$9,card_status_line=$10,card_attempt_id=$11,card_attempt_count=$12,card_attempt_status=$13,card_attempt_runner_id=$14,card_attempt_machine_id=$15,card_attempt_started_at=$16,card_attempt_expires_at=$17,card_execution_id=$18,card_execution_running=$19,card_execution_stage=$20,card_execution_runner_id=$21,card_execution_started_at=$22,card_execution_expires_at=$23,card_change_id=$24,card_change_title=$25,card_change_status=$26,card_change_external_provider=$27,card_change_external_id=$28,card_change_external_url=$29,card_attempt_identity_role=$30,card_attempt_identity_backend=$31,card_attempt_identity_model=$32,card_attempt_identity_effort=$33,card_execution_identity_role=$34,card_execution_identity_backend=$35,card_execution_identity_model=$36,card_execution_identity_effort=$37 WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3`, s.organization, b.ProjectID, b.WorkItemID, d.Title, d.Actor.Kind, d.Actor.ViaKey, d.Actor.DisplayName, d.Actor.Agent, d.Reason, d.StatusLine, a.ID, a.Count, a.Status, a.RunnerID, a.MachineID, a.StartedAt, a.ExpiresAt, e.ID, e.Running, e.Stage, e.RunnerID, e.StartedAt, e.ExpiresAt, c.ID, c.Title, c.Status, x.Provider, x.ID, x.URL, ai.Role, ai.Backend, ai.Model, ai.ReasoningEffort, ei.Role, ei.Backend, ei.Model, ei.ReasoningEffort, d.ChangePrincipalID, d.Ready, d.Exact, provider, provenance.ExternalID, provenance.AuthorID, provenance.AuthorDisplayName, provenance.CreatedAt, provenance.UpdatedAt, provenance.ObservedAt)
	return mapError(ctx, err)
}
