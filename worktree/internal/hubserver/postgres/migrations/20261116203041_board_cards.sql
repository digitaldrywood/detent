-- +goose Up
CREATE TABLE native_board_cards (
 organization_id text NOT NULL,
 project_id text NOT NULL,
 work_item_id text NOT NULL,
 number bigint NOT NULL,
 revision bigint NOT NULL,
 state text NOT NULL,
 archived boolean NOT NULL,
 terminal boolean NOT NULL,
 ready boolean NOT NULL,
 workspace boolean NOT NULL,
 running boolean NOT NULL,
 dispatchable boolean NOT NULL,
 priority integer,
 priority_rank integer NOT NULL,
 activity_at timestamptz,
 identifier text NOT NULL,
 closed_at timestamptz,
 created_at timestamptz NOT NULL,
 updated_at timestamptz NOT NULL,
 last_activity_at timestamptz NOT NULL,
 actor_principal_id text NOT NULL,
 issue_type text NOT NULL,
 title text NOT NULL,
 labels text[] NOT NULL,
 assignees text[] NOT NULL,
 has_links boolean NOT NULL,
 provenance_provider text NOT NULL,
 provenance_external_id text NOT NULL,
 provenance_created_at timestamptz NOT NULL,
 provenance_updated_at timestamptz NOT NULL,
 provenance_observed_at timestamptz NOT NULL,
 pull_request_number bigint,
 pull_request_url text NOT NULL,
 list_change_branch text,
 list_change_head_sha text NOT NULL,
 card_ready boolean NOT NULL,
 status_line text NOT NULL,
 attempt_id text NOT NULL,
 attempt_count bigint NOT NULL,
 attempt_status text NOT NULL,
 attempt_runner_id text NOT NULL,
 attempt_machine_id text NOT NULL,
 attempt_started_at timestamptz,
 attempt_backend text NOT NULL,
 attempt_model text NOT NULL,
 attempt_effort text NOT NULL,
 execution_id text NOT NULL,
 execution_running boolean NOT NULL,
 execution_stage text NOT NULL,
 execution_runner_id text NOT NULL,
 execution_started_at timestamptz,
 execution_backend text NOT NULL,
 execution_model text NOT NULL,
 execution_effort text NOT NULL,
 change_id text NOT NULL,
 change_title text NOT NULL,
 change_status text NOT NULL,
 change_external_provider text NOT NULL,
 change_external_id text NOT NULL,
 change_external_url text NOT NULL,
 change_principal_id text NOT NULL,
 change_version_id text NOT NULL,
 change_policy_id text NOT NULL,
 change_review_policy_id text NOT NULL,
 change_base_status text NOT NULL,
 PRIMARY KEY (organization_id, project_id, work_item_id)
);
CREATE UNIQUE INDEX native_board_cards_item ON native_board_cards (organization_id, work_item_id);
CREATE INDEX native_board_cards_open ON native_board_cards (organization_id, project_id, archived) WHERE NOT terminal;
CREATE INDEX native_board_cards_closed ON native_board_cards (organization_id, project_id, state, (coalesce(closed_at, '-infinity'::timestamptz)) DESC, identifier DESC) WHERE terminal;
CREATE POLICY native_board_cards_backfill ON native_board_items FOR SELECT TO detent_migration USING (true);
INSERT INTO native_board_cards (organization_id,project_id,work_item_id,number,revision,state,archived,terminal,ready,workspace,running,dispatchable,priority,priority_rank,activity_at,identifier,closed_at,created_at,updated_at,last_activity_at,actor_principal_id,issue_type,title,labels,assignees,has_links,provenance_provider,provenance_external_id,provenance_created_at,provenance_updated_at,provenance_observed_at,pull_request_number,pull_request_url,list_change_branch,list_change_head_sha,card_ready,status_line,attempt_id,attempt_count,attempt_status,attempt_runner_id,attempt_machine_id,attempt_started_at,attempt_backend,attempt_model,attempt_effort,execution_id,execution_running,execution_stage,execution_runner_id,execution_started_at,execution_backend,execution_model,execution_effort,change_id,change_title,change_status,change_external_provider,change_external_id,change_external_url,change_principal_id,change_version_id,change_policy_id,change_review_policy_id,change_base_status)
SELECT b.organization_id,b.project_id,b.work_item_id,coalesce(b.issue_number,b.page_number),b.page_revision,b.state,b.archived,b.terminal,b.page_ready,'detent:workspace'=ANY(coalesce(b.page_labels,'{}')),b.running,b.dispatchable,b.priority,b.priority_rank,b.activity_at,b.identifier,b.page_closed_at,b.created_at,b.updated_at,CASE WHEN b.page_last_activity_at>'0001-01-02'::timestamptz THEN b.page_last_activity_at ELSE coalesce(b.activity_at,b.updated_at) END,b.actor_principal_id,b.page_type,b.page_title,coalesce(b.page_labels,'{}'),coalesce(b.page_assignees,'{}'),coalesce(cardinality(b.page_dependencies),0)+coalesce(cardinality(b.page_blockers_id),0)+coalesce(cardinality(b.page_dependents_id),0)+coalesce(cardinality(b.page_related_id),0)+coalesce(cardinality(b.page_reporter_dependencies),0)+coalesce(cardinality(b.page_reporter_blockers_id),0)+coalesce(cardinality(b.page_reporter_dependents_id),0)+coalesce(cardinality(b.page_reporter_related_id),0)>0 OR b.page_blocked_reason_text<>'' OR b.page_unblocks_reason_text<>'' OR b.page_reporter_blocked_reason_text<>'' OR b.page_reporter_unblocks_reason_text<>'',CASE WHEN b.page_provenance_present THEN b.page_provenance_provider ELSE '' END,b.page_provenance_external_id,b.page_provenance_created_at,b.page_provenance_updated_at,b.page_provenance_observed_at,CASE WHEN b.page_pull_request_present THEN b.page_pull_request_number END,b.page_pull_request_url,CASE WHEN b.page_list_change_present THEN b.page_list_change_branch END,b.page_list_change_head_sha,b.card_display_ready,coalesce(b.card_status_line,''),coalesce(b.card_attempt_id,''),coalesce(b.card_attempt_count,0),coalesce(b.card_attempt_status,''),coalesce(b.card_attempt_runner_id,''),coalesce(b.card_attempt_machine_id,''),b.card_attempt_started_at,coalesce(b.card_attempt_identity_backend,''),coalesce(b.card_attempt_identity_model,''),coalesce(b.card_attempt_identity_effort,''),coalesce(b.card_execution_id,''),coalesce(b.card_execution_running,false),coalesce(b.card_execution_stage,''),coalesce(b.card_execution_runner_id,''),b.card_execution_started_at,coalesce(b.card_execution_identity_backend,''),coalesce(b.card_execution_identity_model,''),coalesce(b.card_execution_identity_effort,''),coalesce(b.card_change_id,''),coalesce(b.card_change_title,''),coalesce(b.card_change_status,''),coalesce(b.card_change_external_provider,''),coalesce(b.card_change_external_id,''),coalesce(b.card_change_external_url,''),coalesce(b.card_change_principal_id,''),coalesce(b.card_change_version_id,''),coalesce(b.card_change_policy_id,''),coalesce(b.card_change_review_policy_id,''),coalesce(b.card_change_base_status,'') FROM native_board_items b;
DROP POLICY native_board_cards_backfill ON native_board_items;
ALTER TABLE native_board_cards ENABLE ROW LEVEL SECURITY;
ALTER TABLE native_board_cards FORCE ROW LEVEL SECURITY;
REVOKE ALL ON native_board_cards FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON native_board_cards TO detent_runtime;
CREATE POLICY tenant_isolation ON native_board_cards TO detent_runtime, detent_migration
 USING (organization_id = NULLIF(current_setting('detent.organization_id', true), ''))
 WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE native_board_cards;
