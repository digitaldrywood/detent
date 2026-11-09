package postgres_test

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestConversationContinuationUpgradePreservesAcceptedCommands(t *testing.T) {
	const requiredVersion = 20261108232743
	db, _ := migrationDatabaseAtVersion(t, 20261108224925)
	seedCore(t, db)
	cases := []struct {
		name, delivery string
		statuses       []string
		want           bool
		referenced     bool
	}{
		{name: "referenced_interrupted", delivery: "delivered", statuses: []string{"running", "interrupted"}, referenced: true, want: true},
		{name: "referenced_completed", delivery: "delivered", statuses: []string{"completed", "interrupted"}, referenced: true},
		{name: "saved", delivery: "saved", want: true},
		{name: "queued", delivery: "queued", statuses: []string{"waiting_for_runner"}, want: true},
		{name: "consumed", delivery: "sent", statuses: []string{"running", "interrupted", "starting", "interrupted"}, want: true},
		{name: "acknowledged", delivery: "delivered", statuses: []string{"running", "interrupted", "starting", "interrupted"}, want: true},
		{name: "lost_acknowledgement", delivery: "unknown", statuses: []string{"interrupted", "starting", "interrupted"}, want: true},
		{name: "successful_unbind", delivery: "delivered", statuses: []string{"running", "completed", "starting", "interrupted"}},
		{name: "successful_execution_status", delivery: "sent", statuses: []string{"completed", "running", "interrupted"}},
		{name: "continued_again", delivery: "delivered", statuses: []string{"completed", "running"}, want: true},
		{name: "completed_provider_turn", delivery: "completed", statuses: []string{"running"}, want: true},
		{name: "paged_history", delivery: "interrupted"},
		{name: "tenant_isolation", delivery: "interrupted", statuses: []string{"interrupted"}, want: true},
	}
	for i := range cases {
		if cases[i].name == "paged_history" {
			for index := range 260 {
				status := "interrupted"
				if index == 129 {
					status = "completed"
				}
				cases[i].statuses = append(cases[i].statuses, status)
			}
		}
	}

	for _, organization := range []string{"org_first", "org_second"} {
		if _, err := db.ExecContext(t.Context(), `INSERT INTO api_tokens(organization_id,id,name,token_hash,token_fingerprint,scope,native_only,created_at,updated_at) VALUES($1,'continuation_owner','Continuation owner',$1,$1,'operator',true,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`, organization); err != nil {
			t.Fatal(err)
		}
		for _, tc := range cases {
			id := "continuation_" + tc.name
			if _, err := db.ExecContext(t.Context(), `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status) VALUES($1,$2,'prj_shared','continuation_owner','worker','shared','active')`, organization, id); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_commands(organization_id,conversation_id,key,kind,request_hash,receipt_json) VALUES($1,$2,'continue_original','continue','verified_request','{}')`, organization, id); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery,attempt_id,command_key) VALUES($1,$2,$3,1,'user','continue',$4,'original_attempt','continue_original')`, organization, "message_"+tc.name, id, tc.delivery); err != nil {
				t.Fatal(err)
			}
			sequence := int64(0)
			appendEvent := func(kind string, body any) {
				t.Helper()
				sequence++
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				if tc.referenced {
					var metadata struct {
						ID     string `json:"id"`
						Kind   string `json:"kind"`
						Status string `json:"status"`
					}
					if err := json.Unmarshal(raw, &metadata); err != nil {
						t.Fatal(err)
					}
					if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json,content_attachment_id,query_metadata_version,object_id,message_kind,execution_status) VALUES($1,$2,$3,$4,'{}',$5,1,$6,$7,$8)`, organization, id, sequence, kind, fmt.Sprintf("archive_%s_%s_%d", organization, id, sequence), metadata.ID, metadata.Kind, metadata.Status); err != nil {
						t.Fatal(err)
					}
					return
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json) VALUES($1,$2,$3,$4,$5)`, organization, id, sequence, kind, string(raw)); err != nil {
					t.Fatal(err)
				}
			}
			appendEvent("message.accepted", map[string]string{"id": "message_" + tc.name, "kind": "continue"})
			for _, status := range tc.statuses {
				appendEvent("execution.updated", map[string]string{"status": status})
			}
			if tc.name == "continued_again" {
				if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_commands(organization_id,conversation_id,key,kind,request_hash,receipt_json) VALUES($1,$2,'continue_again','continue','verified_again','{}')`, organization, id); err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery,command_key) VALUES($1,'message_again',$2,2,'user','continue','saved','continue_again')`, organization, id); err != nil {
					t.Fatal(err)
				}
				appendEvent("message.accepted", map[string]string{"id": "message_again", "kind": "continue"})
			}
			if tc.name == "completed_provider_turn" {
				appendEvent("message.updated", map[string]string{"id": "provider_response", "kind": "text", "delivery": "completed"})
			}
			if tc.name == "tenant_isolation" && organization == "org_second" {
				appendEvent("execution.updated", map[string]string{"status": "completed"})
			}
			if _, err := db.ExecContext(t.Context(), `UPDATE conversations SET event_seq=$1 WHERE organization_id=$2 AND id=$3`, sequence, organization, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status) VALUES($1,'no_continue','prj_shared','continuation_owner','worker','shared','active')`, organization); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status) SELECT $1,'ordinary_'||n,'prj_shared','continuation_owner','worker','shared','active' FROM generate_series(1,17) n`, organization); err != nil {
			t.Fatal(err)
		}

	}
	if _, err := db.ExecContext(t.Context(), `DELETE FROM conversation_events WHERE organization_id='org_second' AND conversation_id='continuation_consumed' AND seq=2`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	provider, err := migrationProvider(db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), requiredVersion); err == nil {
		t.Fatal("incomplete accepted Continue history was silently migrated")
	}
	if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	assertRolledBack := func() {
		t.Helper()
		var resolved int
		if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM conversations WHERE execution_continuation IS NOT NULL`).Scan(&resolved); err != nil || resolved != 0 {
			t.Fatalf("failed migration resolved %d conversations: %v", resolved, err)
		}
	}
	assertRolledBack()
	var unresolved bool
	if err := db.QueryRowContext(t.Context(), `SELECT execution_continuation IS NULL FROM conversations WHERE organization_id='org_second' AND id='continuation_consumed'`).Scan(&unresolved); err != nil || !unresolved {
		t.Fatalf("incomplete history ownership unresolved=%t error=%v", unresolved, err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json) VALUES('org_second','continuation_consumed',2,'execution.updated','{"status":"running"}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO conversations(organization_id,id,project_id,owner_principal_id,origin,visibility,status,event_seq) VALUES('org_first','unverified','prj_shared','continuation_owner','worker','shared','active',1);
        INSERT INTO conversation_messages(organization_id,id,conversation_id,seq,role,kind,delivery,command_key) VALUES('org_first','unverified_message','unverified',1,'user','continue','sent','missing_command');
        INSERT INTO conversation_events(organization_id,conversation_id,seq,type,body_json) VALUES('org_first','unverified',1,'message.accepted','{"id":"unverified_message","kind":"continue"}');`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), requiredVersion); err == nil {
		t.Fatal("retained Continue without typed command evidence was silently migrated")
	}
	if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	assertRolledBack()
	if err := db.QueryRowContext(t.Context(), `SELECT execution_continuation IS NULL FROM conversations WHERE organization_id='org_first' AND id='unverified'`).Scan(&unresolved); err != nil || !unresolved {
		t.Fatalf("unverified command unresolved=%t error=%v", unresolved, err)
	}
	if _, err := db.ExecContext(t.Context(), `INSERT INTO conversation_commands(organization_id,conversation_id,key,kind,request_hash,receipt_json) VALUES('org_first','unverified','missing_command','continue','restored_verified_request','{}')`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), requiredVersion); err != nil {
		t.Fatal(err)
	}
	var visible int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM conversations`).Scan(&visible); err != nil || visible != 0 {
		t.Fatalf("unscoped migration role sees %d conversations: %v", visible, err)
	}
	if _, err := db.ExecContext(t.Context(), `RESET ROLE`); err != nil {
		t.Fatal(err)
	}
	for _, organization := range []string{"org_first", "org_second"} {
		for _, tc := range cases {
			want := tc.want && (organization != "org_second" || tc.name != "tenant_isolation")
			var got bool
			if err := db.QueryRowContext(t.Context(), `SELECT execution_continuation FROM conversations WHERE organization_id=$1 AND id=$2`, organization, "continuation_"+tc.name).Scan(&got); err != nil || got != want {
				t.Errorf("%s/%s ownership=%t want=%t error=%v", organization, tc.name, got, want, err)
			}
			var delivery, attempt, command string
			if err := db.QueryRowContext(t.Context(), `SELECT delivery,attempt_id,command_key FROM conversation_messages WHERE organization_id=$1 AND id=$2`, organization, "message_"+tc.name).Scan(&delivery, &attempt, &command); err != nil || delivery != tc.delivery || attempt != "original_attempt" || command != "continue_original" {
				t.Errorf("%s/%s source changed: %s %s %s error=%v", organization, tc.name, delivery, attempt, command, err)
			}

		}
		var ordinary bool
		if err := db.QueryRowContext(t.Context(), `SELECT execution_continuation FROM conversations WHERE organization_id=$1 AND id='no_continue'`, organization).Scan(&ordinary); err != nil || ordinary {
			t.Errorf("%s ordinary ownership=%t error=%v", organization, ordinary, err)
		}
	}
	var unresolvedCount int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM conversations WHERE execution_continuation IS NULL`).Scan(&unresolvedCount); err != nil || unresolvedCount != 0 {
		t.Fatalf("unresolved conversations=%d error=%v", unresolvedCount, err)
	}
	var restored bool
	if err := db.QueryRowContext(t.Context(), `SELECT execution_continuation FROM conversations WHERE organization_id='org_first' AND id='unverified'`).Scan(&restored); err != nil || !restored {
		t.Fatalf("restored accepted command ownership=%t error=%v", restored, err)
	}

	var policies int
	if err := db.QueryRowContext(t.Context(), `SELECT count(*) FROM pg_policies WHERE policyname='continuation_upgrade'`).Scan(&policies); err != nil || policies != 0 {
		t.Fatalf("temporary upgrade policies=%d error=%v", policies, err)
	}
}
