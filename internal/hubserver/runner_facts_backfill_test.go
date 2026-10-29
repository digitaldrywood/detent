package hubserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerFactsBackfill(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	other := newNativeFixture(t, f.service, f.project.OrganizationID, "other-facts")
	fallback := newNativeFixture(t, f.service, f.project.OrganizationID, "fallback-facts")
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE projects SET scheduling_rank=7 WHERE id=?`, fallback.project.ID); err != nil {
		t.Fatal(err)
	}
	db := f.service.database
	runners := []runnerFixture{prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Heartbeat), prepareRunnerForScope(t, f, "organization", runnerauth.Read, runnerauth.Heartbeat)}
	checks := runnerauth.LocalChecks{Checkout: "passed", Doctor: "warning", Provider: "pending", ProviderKinds: []string{"opencode", "codex"}, ObservedAt: now, RunnerSetupDeclared: new(false)}
	projects := []tracker.ProjectID{f.project.ID, other.project.ID}
	for i := range runners {
		r := &runners[i]
		r.enroll(t)
		context := tracker.NativeAdmissionContext{ObservedAt: now, PolicyID: "policy-facts", LabelExclude: []string{"human-owned", "excluded"}, WorkflowStates: []string{"Todo", "Rework"}, RefreshDuration: time.Second}
		observation := tracker.NativeAdmissionObservation{Context: context, RunnerRevision: 1, ReceivedAt: now}
		command := runnerauth.ProjectConfigurationCommand{Request: runnerauth.ProjectConfigurationRequest{RequestID: "request-" + r.binding.RunnerID, ProjectID: string(f.project.ID), Operation: "drain_local_project", ExpectedConfigRevision: "revision-one", AllowLocalBinding: new(false)}, Issuer: json.RawMessage(`{}`), RunnerRevision: 1}
		settings := runnerSettings{ProjectRanks: map[tracker.ProjectID]int{f.project.ID: i + 1, other.project.ID: i + 5}, ProjectConfigurationCommand: &command}
		caps, err := json.Marshal(map[string]any{"sprite_name": fmt.Sprintf("sprite-facts-%d", i), "sprite_woken_at": formatHubTime(now.Add(-time.Minute)), "sprite_wake_failed_at": formatHubTime(now), "native_admission": map[string]any{r.binding.RunnerID: map[tracker.ProjectID]tracker.NativeAdmissionObservation{f.project.ID: observation, other.project.ID: observation}}})
		if err != nil {
			t.Fatal(err)
		}
		settingsRaw, err := json.Marshal(settings)
		if err != nil {
			t.Fatal(err)
		}
		checksRaw, err := json.Marshal(map[tracker.ProjectID]runnerauth.LocalChecks{f.project.ID: checks, other.project.ID: checks})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json=?,machine_facts_ready=NULL,admission_facts_ready=NULL WHERE id=?`, string(caps), r.binding.MachineID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.ExecContext(t.Context(), `UPDATE runner_identities SET routing_settings_json=?,local_checks_json=?,routing_facts_ready=NULL,local_check_facts_ready=NULL WHERE id=?`, string(settingsRaw), string(checksRaw), r.binding.RunnerID); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{f.project.ID, other.project.ID, fallback.project.ID} {
			if _, err := db.db.ExecContext(t.Context(), `INSERT INTO runner_project_ranks(runner_id,project_id,rank) VALUES(?,?,?)`, r.binding.RunnerID, project, 99); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertFacts := func(phase string) {
		for i, r := range runners {
			facts, err := readMachineSpriteFacts(t.Context(), db.reader, f.project.OrganizationID, r.binding.MachineID)
			if err != nil || spriteFactValue(facts.Name) != fmt.Sprintf("sprite-facts-%d", i) || spriteFactValue(facts.WokenAt) != formatHubTime(now.Add(-time.Minute)) || spriteFactValue(facts.WakeFailedAt) != formatHubTime(now) {
				t.Fatalf("%s Sprite facts=%+v err=%v", phase, facts, err)
			}
			ranks, err := readRunnerProjectRanks(t.Context(), db.reader, r.binding.RunnerID, f.project.OrganizationID)
			if err != nil || !reflect.DeepEqual(ranks, map[tracker.ProjectID]int{f.project.ID: i + 1, other.project.ID: i + 5, fallback.project.ID: 7}) {
				t.Fatalf("%s ranks=%v err=%v", phase, ranks, err)
			}
			command, err := readConfigurationCommandFacts(t.Context(), db.reader, f.project.OrganizationID, r.binding.RunnerID)
			if err != nil || command == nil || command.Request.RequestID != "request-"+r.binding.RunnerID || command.Request.ExpectedConfigRevision != "revision-one" || command.Request.AllowLocalBinding == nil || *command.Request.AllowLocalBinding {
				t.Fatalf("%s command=%+v err=%v", phase, command, err)
			}
			for _, project := range projects {
				observation, err := readAdmissionFacts(t.Context(), db.reader, r.binding.RunnerID, project, r.binding.MachineID, f.project.OrganizationID)
				if err != nil || observation.RunnerRevision != 1 || !observation.ReceivedAt.Equal(now) || !reflect.DeepEqual(observation.Context.LabelExclude, []string{"human-owned", "excluded"}) || !reflect.DeepEqual(observation.Context.WorkflowStates, []string{"Todo", "Rework"}) {
					t.Fatalf("%s observation=%+v err=%v", phase, observation, err)
				}
				raw, err := readLocalCheckFacts(t.Context(), db.reader, r.binding.RunnerID, project)
				if err != nil {
					t.Fatal(err)
				}
				var got runnerauth.LocalChecks
				if !raw.Valid {
					t.Fatal("typed checks unavailable")
				}
				if err := json.Unmarshal([]byte(raw.String), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, checks) {
					t.Fatalf("%s checks=%+v, want %+v", phase, got, checks)
				}
			}
		}
	}
	assertFacts("unknown")
	unknownAdmission, err := readRunnerAdmissionObservation(t.Context(), db.reader, nativeScope{organization: f.project.OrganizationID, project: tracker.ProjectID("prj_unknown")}, runnerauth.Runner{Binding: runners[0].binding, OrganizationID: f.project.OrganizationID, Revision: 1}, "policy-facts", now)
	if err != nil || unknownAdmission != nil {
		t.Fatalf("unknown admission=%+v err=%v", unknownAdmission, err)
	}
	for i := range runnerFactsRowsPerTick + 1 {
		id := fmt.Sprintf("machine-facts-%03d", i)
		if _, err := db.db.ExecContext(t.Context(), `INSERT INTO machines(id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES(?,?,0,'test',?,?,?)`, id, id, formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	var remaining int
	countUnknown := func() int {
		t.Helper()
		var count int
		if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM machines WHERE machine_facts_ready IS NULL OR admission_facts_ready IS NULL`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	remaining = countUnknown()
	more, err := db.populateRunnerFact(t.Context())
	if err != nil || !more || countUnknown() != remaining-1 {
		t.Fatalf("single transaction: more=%t err=%v remaining=%d", more, err, countUnknown())
	}
	remaining = countUnknown()
	if err := db.populateRunnerFacts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := countUnknown(); got != remaining-runnerFactsRowsPerTick {
		t.Fatalf("one tick left %d, want %d", got, remaining-runnerFactsRowsPerTick)
	}
	if err := db.populateRunnerFacts(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := countUnknown(); got != 0 {
		t.Fatalf("second tick left %d", got)
	}
	for _, phase := range []string{"backfilled", "contradictory legacy payload"} {
		if phase == "contradictory legacy payload" {
			for _, r := range runners {
				if _, err := db.db.ExecContext(t.Context(), `UPDATE machines SET capabilities_json='{}' WHERE id=?`, r.binding.MachineID); err != nil {
					t.Fatal(err)
				}
				if _, err := db.db.ExecContext(t.Context(), `UPDATE runner_identities SET local_checks_json='{}',routing_settings_json='{"project_ranks":{},"project_configuration_request":{"issuer":{},"request":{}}}' WHERE id=?`, r.binding.RunnerID); err != nil {
					t.Fatal(err)
				}
			}
		}
		assertFacts(phase)
	}
	more, err = db.populateRunnerFact(t.Context())
	if err != nil || more {
		t.Fatalf("completed backfill more=%t err=%v", more, err)
	}
}

func TestRunnerCommandFacts(t *testing.T) {
	f := newDefaultNativeFixture(t, Config{})
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	db := f.service.database
	for _, typed := range []bool{false, true} {
		for i := range 2 {
			key := fmt.Sprintf("command-%t-%d", typed, i)
			request := "request-" + key
			raw := fmt.Sprintf(`{"request":{"id":%q},"pending":true}`, request)
			var pending any
			var requestID any = "contradictory"
			if typed {
				pending, requestID = true, request
				raw = `{"request":{"id":"contradictory"}}`
			}
			if _, err := db.db.ExecContext(t.Context(), `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending,response_request_id) VALUES(?,?,'urgent_runner_update',?,'hash',?,?,?,?)`, f.project.OrganizationID, bootstrapTokenID, key, raw, formatHubTime(f.service.config.now()), pending, requestID); err != nil {
				t.Fatal(err)
			}
			got, err := readUrgentCommandReceipt(t.Context(), db.reader, f.project.OrganizationID, request)
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
				if _, err := db.db.ExecContext(t.Context(), `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending) VALUES(?,?,?,?,'hash',?,?,?)`, f.project.OrganizationID, bootstrapTokenID, operation, fmt.Sprintf("%s-%d", key, j), entry.raw, formatHubTime(f.service.config.now().Add(time.Duration(j)*time.Second)), entry.pending); err != nil {
					t.Fatal(err)
				}
			}
			got, gotOperation, err := readCompletedConfigurationReceipt(t.Context(), db.reader, f.project.OrganizationID, []string{operation, "unused-one", "unused-two", "unused-three"})
			if err != nil || got != completed || gotOperation != operation {
				t.Fatalf("typed=%t completed=%s operation=%s err=%v", typed, got, gotOperation, err)
			}
		}
	}
	if err := db.populateRunnerFacts(t.Context()); err != nil {
		t.Fatal(err)
	}
	var unknown int
	if err := db.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM native_commands WHERE response_pending IS NULL`).Scan(&unknown); err != nil || unknown != 0 {
		t.Fatalf("unknown commands=%d err=%v", unknown, err)
	}
	_, err := readUrgentCommandReceipt(t.Context(), db.reader, f.project.OrganizationID, "absent")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("absent request err=%v", err)
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
