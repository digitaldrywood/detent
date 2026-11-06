package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCommandFacts(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	db := f.service.database
	ctx := f.service.contentContext(t.Context())
	store := func(raw string, referenced bool) (string, string) {
		t.Helper()
		if !referenced {
			return raw, ""
		}
		ref, err := prepareMeteredContent(ctx, string(f.project.OrganizationID), []byte(raw), true)
		if err != nil {
			t.Fatal(err)
		}
		return "{}", ref.Key
	}
	for _, format := range []struct{ typed, referenced bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		typed := format.typed
		for i := range 2 {
			key := fmt.Sprintf("command-%t-%t-%d", typed, format.referenced, i)
			request := "request-" + key
			raw := fmt.Sprintf(`{"request":{"id":%q},"pending":true}`, request)
			var pending any
			var requestID any = "contradictory"
			if typed {
				pending, requestID = true, request
				raw = `{"request":{"id":"contradictory"}}`
			}
			inline, object := store(raw, format.referenced)
			if _, err := db.db.ExecContext(ctx, `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,response_object_key,created_at,response_pending,response_request_id) VALUES(?,?,'urgent_runner_update',?,'hash',?,?,?,?,?)`, f.project.OrganizationID, bootstrapTokenID, key, inline, object, formatHubTime(f.service.config.now()), pending, requestID); err != nil {
				t.Fatal(err)
			}
			got, err := readUrgentCommandReceipt(ctx, db.reader, f.project.OrganizationID, request)
			if err != nil || got != raw {
				t.Fatalf("typed=%t receipt=%s err=%v", typed, got, err)
			}
			operation := "drain_local_project " + key + " project"
			completed, running := `{"pending":false}`, `{"pending":true}`
			var donePending, runningPending any
			if typed {
				donePending, runningPending = false, true
				completed, running = `{"pending":true}`, `{"pending":false}`
			}
			for j, entry := range []struct {
				raw     string
				pending any
			}{{completed, donePending}, {running, runningPending}} {
				inline, object := store(entry.raw, format.referenced)
				if _, err := db.db.ExecContext(ctx, `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,response_object_key,created_at,response_pending) VALUES(?,?,?,?,'hash',?,?,?,?)`, f.project.OrganizationID, bootstrapTokenID, operation, fmt.Sprintf("%s-%d", key, j), inline, object, formatHubTime(f.service.config.now().Add(time.Duration(j)*time.Second)), entry.pending); err != nil {
					t.Fatal(err)
				}
			}
			got, gotOperation, err := readCompletedConfigurationReceipt(ctx, db.reader, f.project.OrganizationID, []string{operation, "unused-one", "unused-two", "unused-three"})
			if err != nil || got != completed || gotOperation != operation {
				t.Fatalf("typed=%t completed=%s operation=%s err=%v", typed, got, gotOperation, err)
			}
		}
	}
	if err := db.populateRunnerFacts(ctx); err != nil {
		t.Fatal(err)
	}
	var unknown int
	if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_commands WHERE response_pending IS NULL`).Scan(&unknown); err != nil || unknown != 0 {
		t.Fatalf("unknown commands=%d err=%v", unknown, err)
	}
	_, err := readUrgentCommandReceipt(ctx, db.reader, f.project.OrganizationID, "absent")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("absent request err=%v", err)
	}
}

func TestConfigurationReceiptsExcludeExpiredHistory(t *testing.T) {
	for _, history := range []int{100, 10000} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			f := newDefaultNativeFixture(t, Config{})
			db := f.service.database
			ctx := f.service.contentContext(t.Context())
			operations := []string{"drain_local_project example-runner example-project", "resume_local_project example-runner example-project"}
			for _, operation := range operations {
				if _, err := db.db.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending,response_expired)
SELECT ?,?,?,i,'hash','{}','2026-10-31T12:00:00Z',0,1 FROM n`, history, f.project.OrganizationID, bootstrapTokenID, operation); err != nil {
					t.Fatal(err)
				}
			}
			for _, live := range []bool{false, true} {
				t.Run(fmt.Sprintf("live=%t", live), func(t *testing.T) {
					if live {
						for i, pending := range []any{nil, false, true} {
							if _, err := db.db.ExecContext(ctx, `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending) VALUES(?,?,?,?,'hash',?,'2026-10-30T12:00:00Z',?)`, f.project.OrganizationID, bootstrapTokenID, operations[0], fmt.Sprintf("live-%d", i), fmt.Sprintf(`{"pending":%t,"order":%d}`, i == 2, i), pending); err != nil {
								t.Fatal(err)
							}
						}
					}
					reader := &backgroundCPUReader{nativeQueryer: db.reader}
					raw, operation, err := readCompletedConfigurationReceipt(ctx, reader, f.project.OrganizationID, operations)
					if live {
						if err != nil || raw != `{"pending":false,"order":1}` || operation != operations[0] {
							t.Fatalf("latest live receipt: raw=%s operation=%s error=%v", raw, operation, err)
						}
					} else if !errors.Is(err, sql.ErrNoRows) {
						t.Fatalf("expired history returned a receipt: raw=%s error=%v", raw, err)
					}
					plans := reader.plans(t)
					if strings.Count(plans, "response_expired=?") != len(operations) {
						t.Errorf("configuration lookup scans expired history: %s", plans)
					}
					if strings.Contains(plans, "RIGHT PART OF ORDER BY") || strings.Contains(plans, "LAST TERM OF ORDER BY") {
						t.Errorf("configuration lookup sorts tied history: %s", plans)
					}
				})
			}
		})
	}
}

func setSpriteFactsFixtureMode(t *testing.T, f nativeFixture, typed bool) {
	t.Helper()
	rows, err := f.service.database.reader.QueryContext(t.Context(), `SELECT id,hostname,capabilities_json,sprite_name,sprite_woken_at,sprite_wake_failed_at,machine_facts_ready FROM machines WHERE organization_id=?`, f.project.OrganizationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type machine struct {
		id            tracker.MachineID
		hostname, raw string
		facts         machineSpriteFacts
		ready         sql.NullBool
	}
	var machines []machine
	for rows.Next() {
		var m machine
		if err := rows.Scan(&m.id, &m.hostname, &m.raw, &m.facts.Name, &m.facts.WokenAt, &m.facts.WakeFailedAt, &m.ready); err != nil {
			t.Fatal(err)
		}
		machines = append(machines, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	for _, m := range machines {
		facts := m.facts
		if !m.ready.Valid {
			facts = machineSpriteFacts{}
			if err := json.Unmarshal([]byte(m.raw), &facts); err != nil {
				t.Fatal(err)
			}
		}
		contradictory := machineSpriteFacts{Name: new(m.hostname), WokenAt: new("9999-01-01T00:00:00Z"), WakeFailedAt: new("9999-01-01T00:00:00Z")}
		if facts.Name != nil && *facts.Name == m.hostname {
			contradictory.Name = new("other-sprite")
		}
		raw := m.raw
		var ready any
		if typed {
			ready = 1
			encoded, err := json.Marshal(contradictory)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				t.Fatal(err)
			}
			if payload == nil {
				payload = make(map[string]json.RawMessage)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			for key, value := range fields {
				payload[key] = value
			}
			encoded, err = json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			raw = string(encoded)
		} else {
			facts = contradictory
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE machines SET sprite_name=?,sprite_woken_at=?,sprite_wake_failed_at=?,machine_facts_ready=?,capabilities_json=? WHERE id=?`, facts.Name, facts.WokenAt, facts.WakeFailedAt, ready, raw, m.id); err != nil {
			t.Fatal(err)
		}
	}
}
