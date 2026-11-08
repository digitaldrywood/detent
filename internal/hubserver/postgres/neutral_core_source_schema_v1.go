package postgres

import (
	"encoding/json"
	"strings"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

type neutralCoreSourceSchema struct {
	columns  map[string]string
	seekKeys []string
}

func neutralCoreSourceSchemasV1() map[string]neutralCoreSourceSchema {
	return map[string]neutralCoreSourceSchema{
		"projects": {seekKeys: []string{"id"}, columns: map[string]string{
			"aging_enabled":                "[30,\"aging_enabled\",\"INTEGER\",1,\"0\",0,0]",
			"archive_cancelled_after_days": "[20,\"archive_cancelled_after_days\",\"INTEGER\",0,\"7\",0,0]",
			"archive_completed_after_days": "[19,\"archive_completed_after_days\",\"INTEGER\",0,\"30\",0,0]",
			"checkout_repository":          "[12,\"checkout_repository\",\"TEXT\",1,\"''\",0,0]",
			"content_settings_revision":    "[32,\"content_settings_revision\",\"INTEGER\",1,\"0\",0,0]",
			"created_at":                   "[7,\"created_at\",\"TEXT\",1,null,0,0]",
			"deleted_at":                   "[22,\"deleted_at\",\"TEXT\",0,null,0,0]",
			"duplicate_challenge":          "[23,\"duplicate_challenge\",\"INTEGER\",1,\"1\",0,0]",
			"github_intake":                "[9,\"github_intake\",\"TEXT\",1,\"'disabled'\",0,0]",
			"github_projection":            "[10,\"github_projection\",\"TEXT\",1,\"'disabled'\",0,0]",
			"github_repository_enabled":    "[11,\"github_repository_enabled\",\"INTEGER\",1,\"1\",0,0]",
			"icon_color":                   "[26,\"icon_color\",\"TEXT\",0,null,0,0]",
			"icon_emoji":                   "[27,\"icon_emoji\",\"TEXT\",0,null,0,0]",
			"icon_image_key":               "[28,\"icon_image_key\",\"TEXT\",0,null,0,0]",
			"icon_image_sha256":            "[29,\"icon_image_sha256\",\"TEXT\",0,null,0,0]",
			"icon_kind":                    "[24,\"icon_kind\",\"TEXT\",0,null,0,0]",
			"icon_name":                    "[25,\"icon_name\",\"TEXT\",0,null,0,0]",
			"id":                           "[0,\"id\",\"TEXT\",0,\"'prj_' || lower(hex(randomblob(16)))\",1,0]",
			"integration_revision":         "[8,\"integration_revision\",\"INTEGER\",1,\"1\",0,0]",
			"model_selection_json":         "[16,\"model_selection_json\",\"TEXT\",0,null,0,0]",
			"model_selection_revision":     "[17,\"model_selection_revision\",\"INTEGER\",1,\"1\",0,0]",
			"name":                         "[3,\"name\",\"TEXT\",1,null,0,0]",
			"organization_id":              "[1,\"organization_id\",\"TEXT\",1,null,0,0]",
			"profile":                      "[4,\"profile\",\"TEXT\",1,null,0,0]",
			"repository_id":                "[2,\"repository_id\",\"INTEGER\",0,null,0,0]",
			"require_dependencies":         "[6,\"require_dependencies\",\"INTEGER\",1,\"1\",0,0]",
			"require_visual_attachments":   "[31,\"require_visual_attachments\",\"INTEGER\",1,\"0\",0,0]",
			"scheduling_rank":              "[18,\"scheduling_rank\",\"INTEGER\",1,\"2147483647\",0,0]",
			"slug":                         "[21,\"slug\",\"TEXT\",1,\"''\",0,0]",
			"states_json":                  "[5,\"states_json\",\"TEXT\",1,\"'[]'\",0,0]",
			"workflow_markdown":            "[15,\"workflow_markdown\",\"TEXT\",1,\"''\",0,0]",
			"workflow_source":              "[13,\"workflow_source\",\"TEXT\",1,\"''\",0,0]",
			"workflow_source_revision":     "[14,\"workflow_source_revision\",\"TEXT\",1,\"''\",0,0]",
		}},
		"repositories": {seekKeys: []string{"id"}, columns: map[string]string{
			"config_json":            "[6,\"config_json\",\"TEXT\",1,\"'{}'\",0,0]",
			"created_at":             "[11,\"created_at\",\"TEXT\",1,null,0,0]",
			"github_database_id":     "[2,\"github_database_id\",\"INTEGER\",0,null,0,0]",
			"github_installation_id": "[5,\"github_installation_id\",\"INTEGER\",0,null,0,0]",
			"github_name":            "[4,\"github_name\",\"TEXT\",1,null,0,0]",
			"github_node_id":         "[1,\"github_node_id\",\"TEXT\",1,null,0,0]",
			"github_owner":           "[3,\"github_owner\",\"TEXT\",1,null,0,0]",
			"id":                     "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"last_reconciled_at":     "[10,\"last_reconciled_at\",\"TEXT\",0,null,0,0]",
			"last_webhook_at":        "[9,\"last_webhook_at\",\"TEXT\",0,null,0,0]",
			"reconcile_cursor":       "[8,\"reconcile_cursor\",\"TEXT\",0,null,0,0]",
			"source_updated_at":      "[14,\"source_updated_at\",\"TEXT\",0,null,0,0]",
			"source_version":         "[13,\"source_version\",\"TEXT\",1,\"''\",0,0]",
			"synchronized_at":        "[15,\"synchronized_at\",\"TEXT\",0,null,0,0]",
			"updated_at":             "[12,\"updated_at\",\"TEXT\",1,null,0,0]",
			"webhook_cursor":         "[7,\"webhook_cursor\",\"TEXT\",0,null,0,0]",
		}},
		"policy_revisions": {seekKeys: []string{"scope", "policy_id"}, columns: map[string]string{
			"approved_at":              "[4,\"approved_at\",\"TEXT\",1,null,0,0]",
			"approved_by":              "[3,\"approved_by\",\"TEXT\",1,null,0,0]",
			"gate_github_pull_request": "[7,\"gate_github_pull_request\",\"INTEGER\",0,null,0,0]",
			"metadata_json":            "[2,\"metadata_json\",\"TEXT\",1,null,0,0]",
			"policy_id":                "[1,\"policy_id\",\"TEXT\",1,null,2,0]",
			"scope":                    "[0,\"scope\",\"TEXT\",1,null,1,0]",
			"source_digest":            "[5,\"source_digest\",\"TEXT\",1,\"''\",0,0]",
			"workflow_revision":        "[6,\"workflow_revision\",\"TEXT\",1,\"''\",0,0]",
		}},
		"project_policies": {seekKeys: []string{"scope"}, columns: map[string]string{
			"policy_id": "[1,\"policy_id\",\"TEXT\",1,null,0,0]",
			"scope":     "[0,\"scope\",\"TEXT\",0,null,1,0]",
		}},
		"change_review_policies": {seekKeys: []string{"organization_id", "project_id"}, columns: map[string]string{
			"organization_id": "[0,\"organization_id\",\"TEXT\",1,null,1,0]",
			"policy_json":     "[2,\"policy_json\",\"TEXT\",1,null,0,0]",
			"project_id":      "[1,\"project_id\",\"TEXT\",1,null,2,0]",
		}},
		"monthly_budget_policies": {seekKeys: []string{"organization_id", "project_id", "revision"}, columns: map[string]string{
			"actor_id":        "[4,\"actor_id\",\"TEXT\",1,null,0,0]",
			"organization_id": "[0,\"organization_id\",\"TEXT\",1,null,1,0]",
			"policy_json":     "[3,\"policy_json\",\"TEXT\",1,null,0,0]",
			"project_id":      "[1,\"project_id\",\"TEXT\",1,\"''\",2,0]",
			"revision":        "[2,\"revision\",\"INTEGER\",1,null,3,0]",
			"updated_at":      "[5,\"updated_at\",\"TEXT\",1,null,0,0]",
		}},
		"monthly_budget_admissions": {seekKeys: []string{"issue_id"}, columns: map[string]string{
			"admitted_at": "[1,\"admitted_at\",\"TEXT\",1,null,0,0]",
			"issue_id":    "[0,\"issue_id\",\"INTEGER\",0,null,1,0]",
		}},
		"monthly_budget_leases": {seekKeys: []string{"lease_id"}, columns: map[string]string{
			"exposure_json": "[1,\"exposure_json\",\"TEXT\",1,null,0,0]",
			"lease_id":      "[0,\"lease_id\",\"TEXT\",0,null,1,0]",
		}},
		"issues": {seekKeys: []string{"id"}, columns: map[string]string{
			"activity_actor_principal_id": "[51,\"activity_actor_principal_id\",\"TEXT\",0,null,0,0]",
			"actor_json":                  "[12,\"actor_json\",\"TEXT\",1,\"'{\\\"kind\\\":\\\"integration\\\",\\\"principal_id\\\":\\\"github-import\\\"}'\",0,0]",
			"actor_kind":                  "[49,\"actor_kind\",\"TEXT\",0,null,0,0]",
			"actor_principal_id":          "[50,\"actor_principal_id\",\"TEXT\",0,null,0,0]",
			"archived":                    "[31,\"archived\",\"INTEGER\",1,\"0\",0,0]",
			"assignees_json":              "[23,\"assignees_json\",\"TEXT\",1,\"'[]'\",0,0]",
			"author_login":                "[17,\"author_login\",\"TEXT\",1,\"''\",0,0]",
			"blocked_reason":              "[41,\"blocked_reason\",\"TEXT\",1,\"''\",0,0]",
			"blocked_reason_basis":        "[42,\"blocked_reason_basis\",\"TEXT\",1,\"''\",0,0]",
			"blocked_reason_revision":     "[43,\"blocked_reason_revision\",\"INTEGER\",1,\"0\",0,0]",
			"body":                        "[19,\"body\",\"TEXT\",1,\"''\",0,0]",
			"created_at":                  "[27,\"created_at\",\"TEXT\",1,null,0,0]",
			"dispatch_generation":         "[29,\"dispatch_generation\",\"INTEGER\",1,\"0\",0,0]",
			"dispatch_requested_at":       "[30,\"dispatch_requested_at\",\"TEXT\",1,\"''\",0,0]",
			"dispatch_wait_since":         "[48,\"dispatch_wait_since\",\"TEXT\",0,null,0,0]",
			"event_sequence":              "[11,\"event_sequence\",\"INTEGER\",1,\"0\",0,0]",
			"github_database_id":          "[4,\"github_database_id\",\"INTEGER\",0,null,0,0]",
			"github_node_id":              "[3,\"github_node_id\",\"TEXT\",0,null,0,0]",
			"github_number":               "[5,\"github_number\",\"INTEGER\",0,null,0,0]",
			"github_state":                "[21,\"github_state\",\"TEXT\",1,null,0,0]",
			"id":                          "[0,\"id\",\"INTEGER\",0,null,1,0]",
			"issue_contract_json":         "[33,\"issue_contract_json\",\"TEXT\",1,\"'{}'\",0,0]",
			"issue_type":                  "[36,\"issue_type\",\"TEXT\",0,null,0,0]",
			"labels_json":                 "[22,\"labels_json\",\"TEXT\",1,\"'[]'\",0,0]",
			"last_activity_at":            "[32,\"last_activity_at\",\"TEXT\",1,\"''\",0,0]",
			"native_created_at":           "[15,\"native_created_at\",\"TEXT\",1,\"strftime('%Y-%m-%dT%H:%M:%SZ', 'now')\",0,0]",
			"native_id":                   "[6,\"native_id\",\"TEXT\",1,\"'wi_' || lower(hex(randomblob(16)))\",0,0]",
			"native_source_key":           "[14,\"native_source_key\",\"TEXT\",0,null,0,0]",
			"native_updated_at":           "[16,\"native_updated_at\",\"TEXT\",1,\"strftime('%Y-%m-%dT%H:%M:%SZ', 'now')\",0,0]",
			"number":                      "[9,\"number\",\"INTEGER\",0,null,0,0]",
			"organization_id":             "[7,\"organization_id\",\"TEXT\",0,null,0,0]",
			"page_filters_ready":          "[54,\"page_filters_ready\",\"INTEGER\",0,null,0,0]",
			"progress_comment_sequence":   "[57,\"progress_comment_sequence\",\"INTEGER\",1,\"0\",0,0]",
			"progress_evidence_revision":  "[55,\"progress_evidence_revision\",\"INTEGER\",1,\"0\",0,0]",
			"progress_human_comment_id":   "[56,\"progress_human_comment_id\",\"TEXT\",1,\"''\",0,0]",
			"project_id":                  "[8,\"project_id\",\"TEXT\",0,null,0,0]",
			"provenance_external_id":      "[53,\"provenance_external_id\",\"TEXT\",0,null,0,0]",
			"provenance_json":             "[13,\"provenance_json\",\"TEXT\",0,null,0,0]",
			"provenance_provider":         "[52,\"provenance_provider\",\"TEXT\",0,null,0,0]",
			"recovery_runner_id":          "[34,\"recovery_runner_id\",\"TEXT\",1,\"''\",0,0]",
			"recovery_version_id":         "[35,\"recovery_version_id\",\"TEXT\",1,\"''\",0,0]",
			"report_context_json":         "[37,\"report_context_json\",\"TEXT\",0,null,0,0]",
			"repository_id":               "[1,\"repository_id\",\"INTEGER\",0,null,0,0]",
			"revision":                    "[10,\"revision\",\"INTEGER\",1,\"1\",0,0]",
			"source_updated_at":           "[25,\"source_updated_at\",\"TEXT\",1,null,0,0]",
			"source_version":              "[24,\"source_version\",\"TEXT\",1,null,0,0]",
			"synchronized_at":             "[26,\"synchronized_at\",\"TEXT\",1,null,0,0]",
			"terminal_entered_at":         "[47,\"terminal_entered_at\",\"TEXT\",0,null,0,0]",
			"title":                       "[18,\"title\",\"TEXT\",1,null,0,0]",
			"unblocks_reason":             "[44,\"unblocks_reason\",\"TEXT\",1,\"''\",0,0]",
			"unblocks_reason_basis":       "[45,\"unblocks_reason_basis\",\"TEXT\",1,\"''\",0,0]",
			"unblocks_reason_revision":    "[46,\"unblocks_reason_revision\",\"INTEGER\",1,\"0\",0,0]",
			"updated_at":                  "[28,\"updated_at\",\"TEXT\",1,null,0,0]",
			"url":                         "[20,\"url\",\"TEXT\",1,null,0,0]",
			"verification_state":          "[38,\"verification_state\",\"TEXT\",0,null,0,0]",
			"verified_at":                 "[40,\"verified_at\",\"TEXT\",0,null,0,0]",
			"verified_by_json":            "[39,\"verified_by_json\",\"TEXT\",0,null,0,0]",
			"workflow_state_id":           "[2,\"workflow_state_id\",\"INTEGER\",0,null,0,0]",
		}},
		"leases": {seekKeys: []string{"lease_id"}, columns: map[string]string{
			"acquired_at":        "[6,\"acquired_at\",\"TEXT\",1,null,0,0]",
			"created_at":         "[9,\"created_at\",\"TEXT\",1,null,0,0]",
			"expires_at":         "[5,\"expires_at\",\"TEXT\",1,null,0,0]",
			"fencing_token":      "[0,\"fencing_token\",\"INTEGER\",0,null,1,0]",
			"issue_id":           "[2,\"issue_id\",\"INTEGER\",1,null,0,0]",
			"landing_capacity":   "[12,\"landing_capacity\",\"INTEGER\",1,\"0\",0,0]",
			"lease_id":           "[1,\"lease_id\",\"TEXT\",1,null,0,0]",
			"machine_id":         "[3,\"machine_id\",\"TEXT\",1,null,0,0]",
			"released_at":        "[8,\"released_at\",\"TEXT\",0,null,0,0]",
			"renewed_at":         "[7,\"renewed_at\",\"TEXT\",1,null,0,0]",
			"session_id":         "[4,\"session_id\",\"TEXT\",1,null,0,0]",
			"updated_at":         "[10,\"updated_at\",\"TEXT\",1,null,0,0]",
			"work_item_revision": "[11,\"work_item_revision\",\"INTEGER\",1,\"0\",0,0]",
		}},
	}
}

func validateNeutralCoreSourceSchema(source *VerifiedNeutralImport, names ...string) error {
	if source == nil || source.Identity().SchemaVersion != 1 {
		return storagecatalog.ErrUnmapped
	}
	definitions := neutralCoreSourceSchemasV1()
	for _, name := range names {
		definition, ok := definitions[name]
		if !ok {
			return storagecatalog.ErrUnmapped
		}
		table, err := source.table(name)
		if err != nil || len(table.Columns) != len(definition.columns) || len(table.Key) != len(definition.seekKeys) {
			return storagecatalog.ErrUnmapped
		}
		for column, signature := range definition.columns {
			if !sameSourceSignature([]byte(table.Columns[column]), signature) {
				return storagecatalog.ErrUnmapped
			}
		}
		for index, key := range definition.seekKeys {
			var fields []json.RawMessage
			if json.Unmarshal([]byte(definition.columns[key]), &fields) != nil || len(fields) != 7 {
				return storagecatalog.ErrUnmapped
			}
			var sourceType string
			if json.Unmarshal(fields[2], &sourceType) != nil {
				return storagecatalog.ErrUnmapped
			}
			kind := "text"
			if strings.Contains(strings.ToUpper(sourceType), "INT") {
				kind = "integer"
			}
			if table.Key[index] != (NeutralImportKey{Column: key, Kind: kind}) {
				return storagecatalog.ErrUnmapped
			}
		}
	}
	return nil
}
