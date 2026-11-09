package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestImportTenantCoreCopyCompletesAllStagesAndScopesTenants(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	connection := startupConnection(t, admin, "detent_runtime_v2", "")
	config := postgres.Config{
		Connection: connection,
		Pool:       postgres.PoolConfig{MaxConnections: 3, ServingConnections: 1, BackgroundConnections: 1},
	}
	catalog, err := storagecatalog.Read(os.DirFS("../.."), "tools/storageinventory/mappings/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	objects := &memoryRunDetails{}

	for _, organization := range []tracker.OrganizationID{"org_aggregate_a", "org_aggregate_b"} {
		copyPath := coreTenantAggregateSourceCopy(t, organization, false)
		source := TenantCoreImportSource{BundlePath: copyPath, OrganizationID: organization, Catalog: catalog, Objects: objects}
		var first TenantCoreImportResult
		for replay := range 2 {
			result, err := ImportTenantCoreCopy(t.Context(), config, source)
			if err != nil {
				t.Fatalf("organization %s import replay %d: %v", organization, replay, err)
			}
			if !result.CoreImportComplete || result.HubIdentityRows != 1 || result.Projects != 1 || result.WorkItems != 1 || result.ExecutionRows != 3 || result.UsageRows != 1 || result.ConversationRows < 2 || result.CreditAccounts != 1 || result.CreditTransactions != 1 {
				t.Fatalf("organization %s import replay %d incomplete: %+v", organization, replay, result)
			}
			if replay == 0 {
				first = result
			} else if result != first {
				t.Fatalf("organization %s replay changed result: first=%+v replay=%+v", organization, first, result)
			}
		}
	}

	failedCopy := coreTenantAggregateSourceCopy(t, "org_aggregate_failure", true)
	failedSource := TenantCoreImportSource{BundlePath: failedCopy, OrganizationID: "org_aggregate_failure", Catalog: catalog, Objects: objects}
	failed, err := ImportTenantCoreCopy(t.Context(), config, failedSource)
	if err == nil || failed.CoreImportComplete || failed.HubIdentityRows != 1 || failed.Projects != 1 || failed.WorkItems != 1 || failed.ConversationRows < 2 || failed.CreditTransactions != 0 {
		t.Fatalf("late credit-stage refusal result=%+v error=%v", failed, err)
	}

	store, err := openPostgresDatabase(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, org := range []tracker.OrganizationID{"org_aggregate_a", "org_aggregate_b"} {
		var count int
		var cost float64
		if err := admin.QueryRowContext(t.Context(), "SELECT count(*),sum(cost_estimate) FROM attempt_usage WHERE organization_id=$1", org).Scan(&count, &cost); err != nil || count != 1 || cost != 0.25 {
			t.Fatalf("imported usage rows=%d cost=%v error=%v", count, cost, err)
		}
	}

	for _, organization := range []tracker.OrganizationID{"org_aggregate_a", "org_aggregate_b"} {
		suffix := string(organization[len("org_aggregate_"):])
		otherSuffix := "a"
		if suffix == "a" {
			otherSuffix = "b"
		}
		if err := store.postgres.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
			identity, err := reader.HubIdentity(t.Context())
			if err != nil {
				return fmt.Errorf("read imported Hub identity: %w", err)
			}
			if identity.OrganizationID != organization || identity.ID != "hub_"+strings.Repeat(suffix[:1], 32) || !bytes.Equal(identity.CursorKey, bytes.Repeat([]byte{suffix[0]}, 32)) {
				return fmt.Errorf("Hub identity not preserved: %+v", identity)
			}
			projectID := tracker.ProjectID("prj_aggregate_" + suffix)
			project, err := reader.ProjectRecord(t.Context(), projectID)
			if err != nil || project.Core.OrganizationID != organization {
				return fmt.Errorf("project scope not preserved: %+v: %w", project.Core, err)
			}
			if _, err := reader.ProjectRecord(t.Context(), tracker.ProjectID("prj_aggregate_"+otherSuffix)); !errors.Is(err, corestore.ErrNotFound) {
				return fmt.Errorf("cross-tenant project read error=%v", err)
			}
			item, err := reader.WorkItem(t.Context(), projectID, tracker.NativeWorkItemID("wi_aggregate_"+suffix))
			if err != nil || item.OrganizationID != organization || item.Title != "Example issue "+suffix {
				return fmt.Errorf("work item scope not preserved: %+v: %w", item, err)
			}
			credential, err := reader.CredentialByHash(t.Context(), apikey.HashToken("example-token-"+string(organization)))
			if err != nil || credential.OrganizationID != organization || credential.ID != "token_aggregate_"+suffix {
				return fmt.Errorf("access identity not preserved: %+v: %w", credential, err)
			}
			if _, err := reader.CredentialByHash(t.Context(), apikey.HashToken("example-token-org_aggregate_"+otherSuffix)); !errors.Is(err, corestore.ErrNotFound) {
				return fmt.Errorf("cross-tenant credential read error=%v", err)
			}
			conversation, err := reader.ConversationByID(t.Context(), "conversation_aggregate_"+suffix)
			if err != nil || conversation.OrganizationID != organization || conversation.ProjectID != projectID {
				return fmt.Errorf("conversation scope not preserved: %+v: %w", conversation, err)
			}
			if _, err := reader.ConversationByID(t.Context(), "conversation_aggregate_"+otherSuffix); !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("cross-tenant conversation read error=%v", err)
			}
			message, err := reader.Message(t.Context(), conversation.ID, "message_aggregate_"+suffix)
			if err != nil || message.Text != "Example message "+suffix {
				return fmt.Errorf("conversation message not preserved: %+v: %w", message, err)
			}
			account, err := reader.BillingCreditAccount(t.Context(), "test")
			if err != nil || account.OrganizationID != organization || account.BalanceMicros != 2500 {
				return fmt.Errorf("credit account not preserved: %+v: %w", account, err)
			}
			transactions, err := reader.BillingCreditTransactions(t.Context(), "test", 0, 10)
			if err != nil || len(transactions) != 1 || transactions[0].ID != 1 || transactions[0].Source != "aggregate-"+suffix || transactions[0].AmountMicros != 2500 {
				return fmt.Errorf("credit transaction not preserved: %+v: %w", transactions, err)
			}
			return nil
		}); err != nil {
			t.Fatalf("verify organization %s: %v", organization, err)
		}
	}
	if err := store.postgres.ReadTenant(t.Context(), "org_aggregate_failure", corestore.Serving, func(reader tenantstore.Reader) error {
		_, err := reader.ConversationByID(t.Context(), "conversation_aggregate_failure")
		return err
	}); err != nil {
		t.Fatalf("late failure lost successfully imported conversation rows: %v", err)
	}
}

func coreTenantAggregateSourceCopy(t *testing.T, organization tracker.OrganizationID, unsupportedCreditPeriod bool) string {
	t.Helper()
	fixture := newCoreNeutralFixture(t, organization, "ai_credit_accounts", "ai_credit_packs", "ai_credit_periods", "ai_credit_purchases", "ai_credit_transactions", "api_tokens", "artifact_grants", "artifact_references", "artifact_services", "attachments", "attempt_usage", "change_command_outputs", "change_content_objects", "change_evidence", "change_finding_attributions", "change_issue_links", "change_landing_receipts", "change_requests", "change_review_policies", "change_sources", "change_versions", "change_viewed_files", "collaboration_events", "collaboration_versions", "conversation_attachment_blobs", "conversation_attachments", "conversation_audience_events", "conversation_commands", "conversation_events", "conversation_messages", "conversation_prices", "conversation_questions", "conversation_starts", "conversation_tool_revisions", "conversation_turn_batches", "conversation_usage", "conversations", "daily_summary_preferences", "daily_summary_receipts", "hosted_audit", "hosted_audit_times", "hosted_binding_migrations", "hosted_invitations", "hosted_member_reservations", "hosted_members", "hosted_project_grants", "hosted_release_preferences", "hosted_sessions", "hosted_tenant", "hosted_transactions", "hub_identity", "issue_dependencies", "issues", "lease_policies", "lease_runners", "leases", "machines", "message_references", "monthly_budget_admissions", "monthly_budget_leases", "monthly_budget_policies", "native_attempt_events", "native_attempts", "native_comments", "native_pr_publications", "organization_secret_audit", "organization_secrets", "organizations", "policy_revisions", "project_policies", "project_secret_audit", "project_secrets", "projects", "queue_entries", "release_deliveries", "repositories", "runner_enrollment_projects", "runner_enrollments", "runner_identities", "runner_identity_events", "runner_project_capacity_limits", "runner_project_ranks", "token_grants", "usage_cost_observations", "work_events", "workflow_states")
	const timestamp = "2026-10-01T00:00:00.000000000Z"
	states, err := json.Marshal(nativeFixtureStates())
	if err != nil {
		t.Fatal(err)
	}
	actorJSON, err := json.Marshal(tracker.Actor{Kind: "human", PrincipalID: "example-owner"})
	if err != nil {
		t.Fatal(err)
	}
	if organization == "" {
		t.Fatal("organization is required")
	}
	suffix := string(organization[len("org_aggregate_"):])
	fixture.add("organizations", map[string]any{"id": organization, "name": "Example organization " + suffix, "slug": "example-org-" + suffix, "local": 1, "created_at": timestamp})
	fixture.add("hub_identity", map[string]any{"id": "hub_" + strings.Repeat(suffix[:1], 32), "cursor_key": bytes.Repeat([]byte{suffix[0]}, 32)})
	fixture.add("hosted_tenant", map[string]any{"singleton": 1, "organization_id": organization, "provider_id": "provider_" + string(organization), "bootstrap_subject": "", "public_url": "https://hub.example.test"})
	tokenID := "token_aggregate_" + string(organization[len("org_aggregate_"):])
	fixture.add("api_tokens", map[string]any{"id": tokenID, "name": tokenID, "token_hash": apikey.HashToken("example-token-" + string(organization)), "token_fingerprint": "example-fingerprint-" + string(organization), "scope": "admin", "created_at": timestamp, "updated_at": timestamp, "native_only": 1})
	projectID := tracker.ProjectID("prj_aggregate_" + suffix)
	projectName := "Example project " + suffix
	projectSlug := "example-project-" + suffix
	fixture.add("projects", map[string]any{"id": projectID, "organization_id": organization, "name": projectName, "profile": "native", "states_json": string(states), "created_at": timestamp, "slug": projectSlug, "workflow_markdown": "Example workflow"})
	workflowStateID := 100
	fixture.add("workflow_states", map[string]any{"id": workflowStateID, "project_id": projectID, "source_name": "Todo", "detent_state": "Todo", "terminal": 0, "dispatchable": 1, "created_at": timestamp, "updated_at": timestamp})
	workItemID := tracker.NativeWorkItemID("wi_aggregate_" + suffix)
	issue := tracker.NativeIssue{
		NativeReference: tracker.NativeReference{OrganizationID: organization, ProjectID: projectID, WorkItemID: workItemID, Number: 1, Revision: 1, Profile: "native"},
		IssueContract:   &issuecontract.State{},
		Title:           "Example issue " + suffix,
		Body:            "Example body " + suffix,
		State:           "Todo",
		Actor:           tracker.Actor{Kind: "human", PrincipalID: "example-owner"},
		CreatedAt:       mustParseTestTime(t, timestamp),
		UpdatedAt:       mustParseTestTime(t, timestamp),
		LastActivityAt:  mustParseTestTime(t, timestamp),
	}
	issueJSON, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	issueID := 41
	fixture.add("issues", map[string]any{"id": issueID, "native_id": workItemID, "organization_id": organization, "project_id": projectID, "number": 1, "revision": 1, "event_sequence": 1, "title": issue.Title, "body": issue.Body, "issue_contract_json": "{}", "url": "https://hub.example.test/issues/" + string(workItemID), "github_state": "open", "source_version": "source-v1", "source_updated_at": timestamp, "synchronized_at": timestamp, "created_at": timestamp, "updated_at": timestamp, "native_created_at": timestamp, "native_updated_at": timestamp, "last_activity_at": timestamp, "workflow_state_id": workflowStateID, "actor_json": string(actorJSON), "labels_json": "[]", "assignees_json": "[]", "author_login": "example-owner"})
	fixture.add("collaboration_versions", map[string]any{"organization_id": organization, "project_id": projectID, "work_item_id": workItemID, "record_id": workItemID, "revision": 1, "encoding": "snapshot", "record_json": string(issueJSON)})
	fixture.add("collaboration_events", map[string]any{"id": "event_aggregate_" + suffix, "organization_id": organization, "project_id": projectID, "work_item_id": workItemID, "sequence": 1, "type": "issue.created", "schema_version": 1, "actor_json": string(actorJSON), "data_json": "{}", "recorded_at": timestamp, "event_revision": 1})
	machineID, leaseID, attemptID, runID := "machine_aggregate_"+suffix, "lease_aggregate_"+suffix, "attempt_aggregate_"+suffix, "run_aggregate_"+suffix
	fixture.add("machines", map[string]any{"id": machineID, "hostname": "example-host", "display_name": "Example host", "capacity": 1, "version": "example-v1", "sprite_name": "", "last_heartbeat_at": timestamp, "registered_at": timestamp, "updated_at": timestamp})
	fixture.add("leases", map[string]any{"fencing_token": 17, "lease_id": leaseID, "issue_id": issueID, "machine_id": machineID, "session_id": "session_aggregate_" + suffix, "work_item_revision": 1, "landing_capacity": 0, "acquired_at": timestamp, "renewed_at": timestamp, "expires_at": "2026-10-01T01:00:00Z", "released_at": "2026-10-02T00:00:00Z"})
	runJSON, err := json.Marshal(tracker.NativeRunData{AttemptID: attemptID, LeaseID: tracker.LeaseID(leaseID), FencingToken: 17, RunID: runID, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	fixture.add("native_attempts", map[string]any{"id": attemptID, "project_id": projectID, "work_item_id": workItemID, "lease_id": leaseID, "fencing_token": 17, "run_id": runID, "sequence": 1, "status": "failed", "work_item_revision": 1, "dispatch_generation": 0, "data_json": string(runJSON), "checkpoint_json": nil, "artifact_ids_json": "[]", "started_at": timestamp, "updated_at": timestamp, "payload_object_key": "", "checkpoint_object_key": ""})
	fixture.add("native_attempt_events", map[string]any{"attempt_id": attemptID, "sequence": 1, "request_hash": strings.Repeat("0", 64)})
	fixture.add("attempt_usage", map[string]any{"attempt_id": attemptID, "project_id": projectID, "period": timestamp, "provider": "codex", "model": "example-model", "input": 100, "cached_input": 20, "output": 30, "cost_estimate": 0.25, "currency": "USD", "updated_at": timestamp})

	conversationID := "conversation_aggregate_" + suffix
	fixture.add("conversations", map[string]any{"id": conversationID, "organization_id": organization, "project_id": projectID, "owner_principal_id": tokenID, "title": "Example conversation " + suffix, "visibility": "private", "status": "active", "execution_json": "{}", "created_at": timestamp, "updated_at": timestamp})
	fixture.add("conversation_messages", map[string]any{"id": "message_aggregate_" + suffix, "conversation_id": conversationID, "seq": 1, "role": "user", "kind": "text", "text": "Example message " + suffix, "data_json": "{}", "delivery": "saved", "actor_json": string(actorJSON), "created_at": timestamp, "updated_at": timestamp})
	fixture.add("ai_credit_accounts", map[string]any{"organization_id": organization, "mode": "test", "balance_micros": 2500})
	fixture.add("ai_credit_transactions", map[string]any{"organization_id": organization, "mode": "test", "source": "aggregate-" + suffix, "amount_micros": 2500, "kind": "purchase", "recorded_at": 1, "id": 1})
	if unsupportedCreditPeriod {
		fixture.add("ai_credit_periods", map[string]any{"organization_id": organization, "mode": "test", "period_start": 1, "period_end": 2, "allowance_micros": 100, "used_micros": 0})
	}
	return fixture.write()
}

func mustParseTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
