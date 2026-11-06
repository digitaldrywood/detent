package hubserver

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

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
		source := TenantCoreImportSource{CopyPath: copyPath, OrganizationID: organization, Catalog: catalog, Objects: objects}
		var first TenantCoreImportResult
		for replay := range 2 {
			result, err := ImportTenantCoreCopy(t.Context(), config, source)
			if err != nil {
				t.Fatalf("organization %s import replay %d: %v", organization, replay, err)
			}
			if !result.CoreImportComplete || result.HubIdentityRows != 1 || result.Projects != 1 || result.WorkItems != 1 || result.ConversationRows < 2 || result.CreditAccounts != 1 || result.CreditTransactions != 1 {
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
	failedSource := TenantCoreImportSource{CopyPath: failedCopy, OrganizationID: "org_aggregate_failure", Catalog: catalog, Objects: objects}
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
	path := filepath.Join(t.TempDir(), "tenant-core-source.db")
	seedHubDatabaseTemplate(t, path)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
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
		_ = db.Close()
		t.Fatal("organization is required")
	}
	suffix := string(organization[len("org_aggregate_"):])
	if _, err := db.ExecContext(t.Context(), `UPDATE organizations SET id=?,name=?,slug=? WHERE local=1`, organization, "Example organization "+suffix, "example-org-"+suffix); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE hub_identity SET id=?,cursor_key=?`, "hub_"+strings.Repeat(suffix[:1], 32), bytes.Repeat([]byte{suffix[0]}, 32)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO hosted_tenant(singleton,organization_id,provider_id,bootstrap_subject,public_url) VALUES(1,?,?, '', 'https://hub.example.test')`, organization, "provider_"+string(organization)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	tokenID := "token_aggregate_" + string(organization[len("org_aggregate_"):])
	if _, err := db.ExecContext(t.Context(), `INSERT INTO api_tokens(id,name,token_hash,token_fingerprint,scope,created_at,updated_at,native_only) VALUES(?,?,?,?,?,?,?,1)`, tokenID, tokenID, apikey.HashToken("example-token-"+string(organization)), "example-fingerprint-"+string(organization), "admin", timestamp, timestamp); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	projectID := tracker.ProjectID("prj_aggregate_" + suffix)
	projectName := "Example project " + suffix
	projectSlug := "example-project-" + suffix
	if _, err := db.ExecContext(t.Context(), `INSERT INTO projects(id,organization_id,name,profile,states_json,created_at,slug,workflow_markdown) VALUES(?,?,?,'native',?,?,?,?)`, projectID, organization, projectName, string(states), timestamp, projectSlug, "Example workflow"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	workflowStateID := 100
	if _, err := db.ExecContext(t.Context(), `INSERT INTO workflow_states(id,project_id,source_name,detent_state,terminal,dispatchable,created_at,updated_at) VALUES(?,?, 'Todo','Todo',0,1,?,?)`, workflowStateID, projectID, timestamp, timestamp); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
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
		_ = db.Close()
		t.Fatal(err)
	}
	issueID := 41
	if _, err := db.ExecContext(t.Context(), `INSERT INTO issues(id,native_id,organization_id,project_id,number,revision,event_sequence,title,body,issue_contract_json,url,github_state,source_version,source_updated_at,synchronized_at,created_at,updated_at,native_created_at,native_updated_at,last_activity_at,workflow_state_id,actor_json,labels_json,assignees_json,author_login)
VALUES(?,?,?,?,?,1,1,?,?,'{}',?,'open','source-v1',?,?,?,?,?,?,?,?,?,'[]','[]','example-owner')`, issueID, workItemID, organization, projectID, 1, issue.Title, issue.Body, "https://hub.example.test/issues/"+string(workItemID), timestamp, timestamp, timestamp, timestamp, timestamp, timestamp, timestamp, workflowStateID, string(actorJSON)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO collaboration_versions(organization_id,project_id,work_item_id,record_id,revision,encoding,record_json) VALUES(?,?,?,?,1,'snapshot',?)`, organization, projectID, workItemID, workItemID, string(issueJSON)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at,event_revision) VALUES(?,?,?, ?,1,'issue.created',1,?,'{}',?,1)`, "event_aggregate_"+suffix, organization, projectID, workItemID, string(actorJSON), timestamp); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	conversationID := "conversation_aggregate_" + suffix
	if _, err := db.ExecContext(t.Context(), `INSERT INTO conversations(id,organization_id,project_id,owner_principal_id,title,visibility,status,execution_json,created_at,updated_at) VALUES(?,?,?,?,?,'private','active','{}',?,?)`, conversationID, organization, projectID, tokenID, "Example conversation "+suffix, timestamp, timestamp); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_messages(id,conversation_id,seq,role,kind,text,data_json,delivery,actor_json,created_at,updated_at) VALUES(?,?,1,'user','text',?,'{}','saved',?,?,?)`, "message_aggregate_"+suffix, conversationID, "Example message "+suffix, string(actorJSON), timestamp, timestamp); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO ai_credit_accounts(organization_id,mode,balance_micros) VALUES(?,'test',2500)`, organization); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO ai_credit_transactions(organization_id,mode,source,amount_micros,kind,recorded_at) VALUES(?,'test',?,2500,'purchase',1)`, organization, "aggregate-"+suffix); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if unsupportedCreditPeriod {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO ai_credit_periods(organization_id,mode,period_start,period_end,allowance_micros,used_micros) VALUES(?,'test',1,2,100,0)`, organization); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustParseTestTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
