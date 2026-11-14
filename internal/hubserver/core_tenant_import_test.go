package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/neutralimport/neutralimporttest"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/storagecatalog"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestImportTenantCoreCopyCompletesAllStagesAndScopesTenants(t *testing.T) {
	t.Parallel()
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
		fixture := neutralimporttest.Read(t, coreTenantAggregateSourceCopy(t, organization, false), organization)
		var issue tracker.NativeIssue
		var raw string
		if err := fixture.Records["collaboration_versions"][0].Decode(map[string]any{"record_json": &raw}); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &issue); err != nil {
			t.Fatal(err)
		}
		issue.Provenance = &tracker.Provenance{Provider: "github", ExternalID: "I_unresolved_" + string(organization), AuthorID: "source-author", CreatedAt: issue.CreatedAt, UpdatedAt: issue.UpdatedAt, ObservedAt: issue.UpdatedAt}
		fixture.Set("issues", map[string]string{"native_id": string(issue.WorkItemID)}, map[string]any{"provenance_json": string(historyTestJSON(t, issue.Provenance))})
		fixture.Set("collaboration_versions", map[string]string{"record_id": string(issue.WorkItemID), "revision": "1"}, map[string]any{"record_json": string(historyTestJSON(t, issue))})
		copyPath := fixture.Write()
		var logs bytes.Buffer
		source := TenantCoreImportSource{BundlePath: copyPath, OrganizationID: organization, Catalog: catalog, Objects: objects, Logger: slog.New(slog.NewTextHandler(&logs, nil))}
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
		if !strings.Contains(logs.String(), "GitHub issue reference remains unresolved") || !strings.Contains(logs.String(), string(issue.WorkItemID)) {
			t.Fatalf("top-level import lost unresolved-reference diagnostic: %s", logs.String())
		}
		if strings.Contains(logs.String(), issue.Body) || strings.Contains(logs.String(), issue.Title) {
			t.Fatalf("import diagnostic exposed source content: %s", logs.String())
		}
	}

	var imported, local int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*),count(*) FILTER (WHERE local) FROM organizations WHERE organization_id IN ('org_aggregate_a','org_aggregate_b')`).Scan(&imported, &local); err != nil || imported != 2 || local != 0 {
		t.Fatalf("hosted tenant imports organizations=%d local=%d error=%v, want 2 organizations and none local", imported, local, err)
	}
	t.Run("historical recovery requires authoritative mappings", func(t *testing.T) {
		store, err := openPostgresDatabase(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.Close(); err != nil {
				t.Error(err)
			}
		})
		for _, organization := range []tracker.OrganizationID{"org_historical_a", "org_historical_b"} {
			project := tracker.ProjectID("prj_historical")
			seedHistoricalImportTarget(t, store.postgres, organization, project, "github_compatible")
			fixture := neutralimporttest.HistoricalV7(t, organization)
			path := fixture.Write()
			source := TenantCoreImportSource{BundlePath: path, OrganizationID: organization, Catalog: catalog, Objects: objects}
			result, err := ImportTenantCoreCopy(t.Context(), config, source)
			if err == nil || !strings.Contains(err.Error(), "verified post-native mapping bundle") || result.CoreImportComplete {
				t.Fatalf("missing mapping result=%+v error=%v", result, err)
			}
			assertUnwritten := func() {
				t.Helper()
				if err := store.postgres.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
					rows, err := reader.WorkItems(t.Context(), project, 0, 10)
					if err != nil {
						return err
					}
					if len(rows) != 0 {
						return errors.New("identity prerequisite failure wrote work items")
					}
					if _, err := reader.Receipt(t.Context(), "production-import", "core-project-source", "source"); !errors.Is(err, corestore.ErrNotFound) {
						return fmt.Errorf("identity prerequisite failure pinned source: %v", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			assertUnwritten()
			mapping := coreWorkItemNeutralFixture(t, organization, project, false)
			mapping.AddTables(t, os.DirFS("../.."), "projects")
			mapping.Add("projects", map[string]any{"id": project, "organization_id": organization, "repository_id": 1, "name": "Historical mapping", "profile": "github_compatible", "created_at": "2026-09-01T12:00:00Z"})
			mapping.Set("repositories", map[string]string{"id": "1"}, map[string]any{"github_node_id": "R_original"})
			for index, id := range []int64{41, 43} {
				suffix := ""
				if index == 1 {
					suffix = "_second"
				}
				mapping.Set("issues", map[string]string{"native_id": "wi_core_import" + suffix}, map[string]any{"github_node_id": "I_original_" + fmt.Sprint(id), "number": index + 1})
			}
			bad := mapping.Clone(t)
			bad.Set("issues", map[string]string{"native_id": "wi_core_import"}, map[string]any{"github_node_id": "I_foreign"})
			source.MappingBundlePath = bad.Write()
			result, err = ImportTenantCoreCopy(t.Context(), config, source)
			if err == nil || result.CoreImportComplete {
				t.Fatalf("foreign identity mapping result=%+v error=%v", result, err)
			}
			assertUnwritten()
			source.MappingBundlePath = mapping.Write()
			for range 2 {
				result, err = ImportTenantCoreCopy(t.Context(), config, source)
				if err != nil || result.CoreImportComplete || result.WorkItems != 2 {
					t.Fatalf("verified historical recovery result=%+v error=%v", result, err)
				}
			}
			if err := store.postgres.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.WorkItem(t.Context(), project, "wi_core_import")
				if err != nil {
					return err
				}
				if row.ID != 41 || row.ProvenanceExternalID == nil || *row.ProvenanceExternalID != "I_original_41" {
					return errors.New("verified identity was not preserved")
				}
				row.Title = "Newer local state"
				_, err = w.UpdateWorkItem(t.Context(), row)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			source.MappingBundlePath = ""
			result, err = ImportTenantCoreCopy(t.Context(), config, source)
			if err != nil || result.CoreImportComplete || result.WorkItems != 2 {
				t.Fatalf("existing authoritative replay result=%+v error=%v", result, err)
			}
			if err := store.postgres.ReadTenant(t.Context(), organization, corestore.Serving, func(reader tenantstore.Reader) error {
				row, err := reader.WorkItem(t.Context(), project, "wi_core_import")
				if err != nil {
					return err
				}
				if row.Title != "Newer local state" || row.Revision != 2 {
					return errors.New("historical replay overwrote newer state")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("historical source was not preserved: %v", err)
			}
		}
	})

	t.Run("missing issues table keeps current import route", func(t *testing.T) {
		organization := tracker.OrganizationID("org_aggregate_c")
		fixture := neutralimporttest.Read(t, coreTenantAggregateSourceCopy(t, organization, false), organization)
		delete(fixture.Tables, "issues")
		delete(fixture.Records, "issues")
		result, err := ImportTenantCoreCopy(t.Context(), config, TenantCoreImportSource{BundlePath: fixture.Write(), OrganizationID: organization, Catalog: catalog, Objects: objects})
		if err == nil || result.CoreImportComplete || result.HubIdentityRows != 1 || result.Projects != 1 {
			t.Fatalf("missing issues changed current import route: result=%+v error=%v", result, err)
		}
	})

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
	fixture := newCoreNeutralFixture(t, organization, "ai_credit_accounts", "ai_credit_packs", "ai_credit_periods", "ai_credit_purchases", "ai_credit_transactions", "api_tokens", "artifact_grants", "artifact_references", "artifact_services", "attachments", "attempt_diff_file_archives", "attempt_diff_files", "attempt_diffs", "attempt_usage", "change_command_outputs", "change_content_objects", "change_evidence", "change_finding_attributions", "change_issue_links", "change_landing_receipts", "change_requests", "change_review_policies", "change_sources", "change_versions", "change_viewed_files", "collaboration_events", "collaboration_versions", "conversation_attachment_blobs", "conversation_attachments", "conversation_audience_events", "conversation_commands", "conversation_events", "conversation_messages", "conversation_prices", "conversation_questions", "conversation_starts", "conversation_tool_revisions", "conversation_turn_batches", "conversation_usage", "conversations", "daily_summary_preferences", "daily_summary_receipts", "hosted_audit", "hosted_audit_times", "hosted_binding_migrations", "hosted_invitations", "hosted_member_reservations", "hosted_members", "hosted_project_grants", "hosted_release_preferences", "hosted_sessions", "hosted_tenant", "hosted_transactions", "hub_identity", "issue_dependencies", "issues", "lease_policies", "lease_runners", "leases", "machines", "message_references", "monthly_budget_admissions", "monthly_budget_leases", "monthly_budget_policies", "native_attempt_events", "native_attempts", "native_comments", "native_pr_publications", "organization_secret_audit", "organization_secrets", "organizations", "policy_revisions", "project_policies", "project_secret_audit", "project_secrets", "projects", "quality_landings", "queue_entries", "release_deliveries", "repositories", "runner_enrollment_projects", "runner_enrollments", "runner_identities", "runner_identity_events", "runner_project_capacity_limits", "runner_project_ranks", "token_grants", "usage_cost_observations", "work_events", "workflow_states")
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
	fixture.Add("organizations", map[string]any{"id": organization, "name": "Example organization " + suffix, "slug": "example-org-" + suffix, "local": 1, "created_at": timestamp})
	fixture.Add("hub_identity", map[string]any{"id": "hub_" + strings.Repeat(suffix[:1], 32), "cursor_key": bytes.Repeat([]byte{suffix[0]}, 32)})
	fixture.Add("hosted_tenant", map[string]any{"singleton": 1, "organization_id": organization, "provider_id": "provider_" + string(organization), "bootstrap_subject": "", "public_url": "https://hub.example.test"})
	tokenID := "token_aggregate_" + string(organization[len("org_aggregate_"):])
	fixture.Add("api_tokens", map[string]any{"id": tokenID, "name": tokenID, "token_hash": apikey.HashToken("example-token-" + string(organization)), "token_fingerprint": "example-fingerprint-" + string(organization), "scope": "admin", "created_at": timestamp, "updated_at": timestamp, "native_only": 1})
	projectID := tracker.ProjectID("prj_aggregate_" + suffix)
	projectName := "Example project " + suffix
	projectSlug := "example-project-" + suffix
	fixture.Add("projects", map[string]any{"id": projectID, "organization_id": organization, "name": projectName, "profile": "native", "states_json": string(states), "created_at": timestamp, "slug": projectSlug, "workflow_markdown": "Example workflow"})
	workflowStateID := 100
	fixture.Add("workflow_states", map[string]any{"id": workflowStateID, "project_id": projectID, "source_name": "Todo", "detent_state": "Todo", "terminal": 0, "dispatchable": 1, "created_at": timestamp, "updated_at": timestamp})
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
	fixture.Add("issues", map[string]any{"id": issueID, "native_id": workItemID, "organization_id": organization, "project_id": projectID, "number": 1, "revision": 1, "event_sequence": 1, "title": issue.Title, "body": issue.Body, "issue_contract_json": "{}", "url": "https://hub.example.test/issues/" + string(workItemID), "github_state": "open", "source_version": "source-v1", "source_updated_at": timestamp, "synchronized_at": timestamp, "created_at": timestamp, "updated_at": timestamp, "native_created_at": timestamp, "native_updated_at": timestamp, "last_activity_at": timestamp, "workflow_state_id": workflowStateID, "actor_json": string(actorJSON), "labels_json": "[]", "assignees_json": "[]", "author_login": "example-owner"})
	fixture.Add("collaboration_versions", map[string]any{"organization_id": organization, "project_id": projectID, "work_item_id": workItemID, "record_id": workItemID, "revision": 1, "encoding": "snapshot", "record_json": string(issueJSON)})
	fixture.Add("collaboration_events", map[string]any{"id": "event_aggregate_" + suffix, "organization_id": organization, "project_id": projectID, "work_item_id": workItemID, "sequence": 1, "type": "issue.created", "schema_version": 1, "actor_json": string(actorJSON), "data_json": "{}", "recorded_at": timestamp, "event_revision": 1})
	machineID, leaseID, attemptID, runID := "machine_aggregate_"+suffix, "lease_aggregate_"+suffix, "attempt_aggregate_"+suffix, "run_aggregate_"+suffix
	fixture.Add("machines", map[string]any{"id": machineID, "hostname": "example-host", "display_name": "Example host", "capacity": 1, "version": "example-v1", "sprite_name": "", "last_heartbeat_at": timestamp, "registered_at": timestamp, "updated_at": timestamp})
	fixture.Add("leases", map[string]any{"fencing_token": 17, "lease_id": leaseID, "issue_id": issueID, "machine_id": machineID, "session_id": "session_aggregate_" + suffix, "work_item_revision": 1, "landing_capacity": 0, "acquired_at": timestamp, "renewed_at": timestamp, "expires_at": "2026-10-01T01:00:00Z", "released_at": "2026-10-02T00:00:00Z"})
	runJSON, err := json.Marshal(tracker.NativeRunData{AttemptID: attemptID, LeaseID: tracker.LeaseID(leaseID), FencingToken: 17, RunID: runID, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	fixture.Add("native_attempts", map[string]any{"id": attemptID, "project_id": projectID, "work_item_id": workItemID, "lease_id": leaseID, "fencing_token": 17, "run_id": runID, "sequence": 1, "status": "failed", "work_item_revision": 1, "dispatch_generation": 0, "data_json": string(runJSON), "checkpoint_json": nil, "artifact_ids_json": "[]", "started_at": timestamp, "updated_at": timestamp, "payload_object_key": "", "checkpoint_object_key": ""})
	fixture.Add("native_attempt_events", map[string]any{"attempt_id": attemptID, "sequence": 1, "request_hash": strings.Repeat("0", 64)})
	fixture.Add("attempt_usage", map[string]any{"attempt_id": attemptID, "project_id": projectID, "period": timestamp, "provider": "codex", "model": "example-model", "input": 100, "cached_input": 20, "output": 30, "cost_estimate": 0.25, "currency": "USD", "updated_at": timestamp})

	conversationID := "conversation_aggregate_" + suffix
	fixture.Add("conversations", map[string]any{"id": conversationID, "organization_id": organization, "project_id": projectID, "owner_principal_id": tokenID, "title": "Example conversation " + suffix, "visibility": "private", "status": "active", "execution_json": "{}", "created_at": timestamp, "updated_at": timestamp})
	fixture.Add("conversation_messages", map[string]any{"id": "message_aggregate_" + suffix, "conversation_id": conversationID, "seq": 1, "role": "user", "kind": "text", "text": "Example message " + suffix, "data_json": "{}", "delivery": "saved", "actor_json": string(actorJSON), "created_at": timestamp, "updated_at": timestamp})
	fixture.Add("ai_credit_accounts", map[string]any{"organization_id": organization, "mode": "test", "balance_micros": 2500})
	fixture.Add("ai_credit_transactions", map[string]any{"organization_id": organization, "mode": "test", "source": "aggregate-" + suffix, "amount_micros": 2500, "kind": "purchase", "recorded_at": 1, "id": 1})
	if unsupportedCreditPeriod {
		fixture.Add("ai_credit_periods", map[string]any{"organization_id": organization, "mode": "test", "period_start": 1, "period_end": 2, "allowance_micros": 100, "used_micros": 0})
	}
	return fixture.Write()
}

func mustParseTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestTenantCoreImportRefusesLegacyCopy(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "historical-source.db")
	original := []byte("preserved verified historical source")
	if err := os.WriteFile(path, original, 0400); err != nil {
		t.Fatal(err)
	}
	for _, bundle := range []string{"", "unused-neutral-bundle"} {
		_, err := ImportTenantCoreCopy(t.Context(), postgres.Config{}, TenantCoreImportSource{CopyPath: path, BundlePath: bundle})
		if !errors.Is(err, neutralimport.ErrInvalid) {
			t.Fatalf("tenant legacy selector error=%v", err)
		}
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatalf("historical source changed: %v", err)
	}
}
