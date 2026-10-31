package hubserver

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func storageTestArchiveObjects(t *testing.T, service *Service, objects map[string][]byte) map[string][]byte {
	t.Helper()
	archives := maps.Clone(objects)
	rows, err := service.database.reader.QueryContext(t.Context(), "SELECT response_object_key FROM native_commands WHERE response_object_key<>''")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		delete(archives, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return archives
}

func TestNativeCommandReceiptsExcludeEvidence(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Content: objects.call})
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := service.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	f := newChangeFixture(t, service)
	for _, size := range []int{32, 60 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			request := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("discussion-%d", size)}, Body: strings.Repeat("x", size)}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			var inline string
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT response_json FROM native_commands WHERE command_key=?", request.IdempotencyKey).Scan(&inline); err != nil {
				t.Fatal(err)
			}
			if inline != "{}" {
				t.Errorf("command receipt retained %d inline response bytes", len(inline))
			}
			replay := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request)
			requireNativeStatus(t, replay, http.StatusOK)
			if replay.Body.String() != response.Body.String() {
				t.Fatal("receipt replay changed the complete discussion response")
			}
			request.Body += "changed"
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request), http.StatusConflict)
		})
	}
}

func TestRunHistoryUpgradesCompletedVersionOne(t *testing.T) {
	for _, envelope := range []bool{false, true} {
		t.Run(fmt.Sprintf("detail=%t", envelope), func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "completed legacy history")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			ctx := f.service.freshRunDetailContext(t.Context())
			data := tracker.NativeRunData{AttemptID: "legacy-attempt", CompletionBody: "complete legacy output"}
			stored := storedRun{NativeRunData: data}
			if envelope {
				entry := runDetailEntry(data)
				raw, err := json.Marshal(data)
				if err != nil {
					t.Fatal(err)
				}
				ref, err := prepareChangeContent(ctx, string(scope.organization), raw)
				if err != nil {
					t.Fatal(err)
				}
				stored = storedRun{NativeRunData: tracker.NativeRunData{AttemptID: data.AttemptID}, Detail: newRunDetailReference(ref, string(scope.organization), entry)}
				stored.Detail.ObjectKey = ref.Key
			}
			run, err := json.Marshal(stored)
			if err != nil {
				t.Fatal(err)
			}
			tx := f.tx(t)
			for i := range storageConversionRows + 1 {
				if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(issue.WorkItemID), "run.finished", tracker.CollaborationData{ReasonDetail: strconv.Itoa(i), Run: &data}, f.now); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE storage_conversion_progress SET format_version=1,complete=1,cursor_rowid=through_rowid WHERE family='runs'"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.ExecContext(t.Context(), "UPDATE collaboration_events SET conversion_state=2 WHERE work_item_id=?", issue.WorkItemID); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			if envelope {
				tx := f.tx(t)
				if _, err := tx.ExecContext(t.Context(), "INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) VALUES('legacy-envelope',?,?,?,999,'run.finished',1,'{}',?,?)", scope.organization, scope.project, issue.WorkItemID, `{"run":`+string(run)+`}`, formatHubTime(f.now)); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE collaboration_events SET conversion_state=2 WHERE id='legacy-envelope'"); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(t.Context(), "UPDATE storage_conversion_progress SET format_version=1,complete=1,cursor_rowid=through_rowid WHERE family='runs'"); err != nil {
					t.Fatal(err)
				}
				f.commit(t, tx)
			}
			for pass, want := range []int{storageConversionRows, storageConversionRows + 1} {
				if envelope && pass == 1 {
					want++
				}
				ctx := context.WithValue(f.service.freshRunDetailContext(t.Context()), storageBatchKey{}, &storageBatchBudget{})
				for range storageConversionRows {
					if err := f.service.convertStorageFamilyTurn(ctx, storageConversionFamilies[0], storageFamilyCursor{}, f.now); err != nil {
						t.Fatal(err)
					}
				}
				var converted int
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND payload_object_key<>'' AND data_json='{}'", issue.WorkItemID).Scan(&converted); err != nil || converted != want {
					t.Fatalf("pass %d converted %d rows, want %d: %v", pass, converted, want, err)
				}
			}
			var key, attempt string
			var version int
			var complete bool
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT format_version,complete FROM storage_conversion_progress WHERE family='runs' AND aggregate_id=?", issue.WorkItemID).Scan(&version, &complete); err != nil || version != storageConversionVersion || !complete {
				t.Fatalf("run conversion version=%d complete=%t: %v", version, complete, err)
			}
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT "+collaborationPayloadColumn("")+",run_attempt_id FROM collaboration_events WHERE work_item_id=? AND type='run.finished' ORDER BY sequence DESC LIMIT 1", issue.WorkItemID).Scan(&key, &attempt); err != nil {
				t.Fatal(err)
			}
			if attempt != data.AttemptID {
				t.Fatalf("upgraded history lost typed attempt identity: %q", attempt)
			}
			var restored tracker.CollaborationData
			if err := decodeRunHistory(f.service.freshRunDetailContext(t.Context()), f.service.database.reader, key, &restored); err != nil || restored.Run == nil || restored.Run.CompletionBody != data.CompletionBody {
				t.Fatalf("upgraded history lost full detail: %+v %v", restored, err)
			}
		})
	}
}

func TestRunHistoryConversionPreservesPendingArchiveFacts(t *testing.T) {
	for _, kind := range []string{"run.started", "run.finished"} {
		t.Run(kind, func(t *testing.T) {
			f := newStorageArchiveFixture(t)
			issue := f.nativeFixture.create(t, "pending archive facts")
			scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
			tx := f.tx(t)
			raw := `{"operation":"finish","from_state":"Doing","to_state":"Done","run":{"attempt_id":"archive-attempt","outcome":"succeeded","finalization":{"publication":{"external":{"head_sha":"archive-commit"}}}}}`
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO collaboration_events(id,organization_id,project_id,work_item_id,sequence,type,schema_version,actor_json,data_json,recorded_at) VALUES('pending-archive',?,?,?,999,?,1,'{}',?,?)`, scope.organization, scope.project, issue.WorkItemID, kind, raw, formatHubTime(f.now)); err != nil {
				t.Fatal(err)
			}
			f.commit(t, tx)
			for _, phase := range []string{"legacy", "converted", "backfilled"} {
				switch phase {
				case "converted":
					if err := f.service.convertStorageFamily(f.service.freshRunDetailContext(t.Context()), scope, storageConversionFamilies[0], f.now); err != nil {
						t.Fatal(err)
					}
					var converted int
					if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type=? AND payload_object_key<>'' AND data_json='{}'`, issue.WorkItemID, kind).Scan(&converted); err != nil || converted != 1 {
						t.Fatalf("converted rows=%d, want 1: %v", converted, err)
					}
				case "backfilled":
					if err := f.service.database.populateArchiveFacts(t.Context()); err != nil {
						t.Fatal(err)
					}
				}
				timeline, err := readArchiveTimeline(t.Context(), f.service.database.reader, scope, string(issue.WorkItemID))
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, entry := range timeline {
					var facts struct {
						Type    string `json:"type"`
						From    string `json:"from_state"`
						To      string `json:"to_state"`
						Attempt string `json:"attempt_id"`
						Outcome string `json:"outcome"`
						Commit  string `json:"landed_commit"`
					}
					if err := json.Unmarshal(entry, &facts); err != nil {
						t.Fatal(err)
					}
					if facts.Type == kind {
						found = true
						if facts.From != "Doing" || facts.To != "Done" || facts.Attempt != "archive-attempt" || facts.Outcome != "succeeded" || facts.Commit != "archive-commit" {
							t.Errorf("%s lost archive facts: %+v", phase, facts)
						}
					}
				}
				if !found {
					t.Fatalf("%s missing %s timeline entry", phase, kind)
				}
			}
		})
	}
}

func TestRunHistoryConversionAdvancesPastActiveIssues(t *testing.T) {
	f := newStorageArchiveFixture(t)
	active := f.nativeFixture.create(t, "active history")
	eligible := f.nativeFixture.create(t, "eligible history")
	if active.WorkItemID > eligible.WorkItemID {
		active, eligible = eligible, active
	}
	scope := nativeScope{organization: f.project.OrganizationID, project: f.project.ID}
	approveHubTestPolicy(t, f.service, f.base+"/policy", hubTestPolicy())
	lease := claimNativeAttempt(t, f.nativeFixture, f.worker(t, "conversion-worker"), "conversion-machine", "conversion-session", active.WorkItemID)
	tx := f.tx(t)
	for _, test := range []struct {
		item tracker.NativeWorkItemID
		rows int
	}{{active.WorkItemID, storageConversionRows}, {eligible.WorkItemID, 1}} {
		for range test.rows {
			if err := appendLegacyNativeHistory(t.Context(), tx, scope, string(test.item), "run.finished", tracker.CollaborationData{Run: &tracker.NativeRunData{AttemptID: "historical-attempt", CompletionBody: "historical output"}}, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.commit(t, tx)
	convert := func() {
		t.Helper()
		ctx := context.WithValue(f.service.freshRunDetailContext(t.Context()), storageBatchKey{}, &storageBatchBudget{})
		for range storageConversionRows {
			if err := f.service.convertStorageFamilyTurn(ctx, storageConversionFamilies[0], storageFamilyCursor{}, f.now); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertConverted := func(item tracker.NativeWorkItemID, want int) {
		t.Helper()
		var count int
		if err := f.service.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FROM collaboration_events WHERE work_item_id=? AND type='run.finished' AND payload_object_key<>''`, item).Scan(&count); err != nil || count != want {
			t.Fatalf("item %s converted=%d, want %d: %v", item, count, want, err)
		}
	}
	convert()
	var firstPass int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM collaboration_events WHERE work_item_id IN (?,?) AND type='run.finished' AND payload_object_key<>''", active.WorkItemID, eligible.WorkItemID).Scan(&firstPass); err != nil || firstPass != storageConversionRows {
		t.Fatalf("first pass converted=%d, want %d: %v", firstPass, storageConversionRows, err)
	}
	convert()
	assertConverted(eligible.WorkItemID, 1)
	assertConverted(active.WorkItemID, storageConversionRows)
	var live bool
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT released_at IS NULL FROM leases WHERE lease_id=?", lease.ID).Scan(&live); err != nil || !live {
		t.Fatalf("historical conversion changed the live lease: live=%v error=%v", live, err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), `UPDATE leases SET released_at=? WHERE lease_id=?`, formatHubTime(f.now), lease.ID); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		convert()
	}
	assertConverted(active.WorkItemID, storageConversionRows)
}

func TestReportObjectDownloadsReleaseReaders(t *testing.T) {
	objects := &changeObjectFixture{}
	f := newHostedSecurityFixture(t, func(cfg *Config) { cfg.Content = objects.call })
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := f.service.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	f.service.dashboard.stop()
	f.service.outbox.stop()
	f.service.stopHostedBilling()
	f.service.stopHealthDetector()
	f.service.stopIssueMailer()
	owner := f.user(t, "owner", "owner", "owner@example.test", "", "")
	f.grant(t, owner, false, false)
	f.service.tokenUseWork.Wait()
	scope := nativeScope{organization: "org_security", project: f.project}
	now := f.service.config.now().UTC()
	ctx := f.service.contentContext(t.Context())
	data := tracker.NativeRunData{AttemptID: "example-attempt", Runtime: &tracker.NativeRuntimeObservation{}}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := prepareChangeContent(ctx, string(scope.organization), raw)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.service.database.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO machines(id,organization_id,hostname,capacity,version,last_heartbeat_at,registered_at,updated_at) VALUES('example-runner','org_security','example-runner',1,'example',?,?,?)", formatHubTime(now), formatHubTime(now), formatHubTime(now)); err != nil {
		t.Fatal(err)
	}
	issue, err := createNativeFixtureIssueTx(t.Context(), tx, scope, tracker.CreateIssue{Title: "report attempt", Body: nativeContractTestBody, State: "Todo"}, now)
	if err != nil {
		t.Fatal(err)
	}
	id := seedHealthRateAttempt(t, tx, scope, issue, "example-runner", "succeeded", data, now, -1)
	if _, err := tx.ExecContext(t.Context(), "UPDATE native_attempts SET data_json='{}',payload_object_key=? WHERE id=?", ref.Key, id); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	w := operatortool.AnalyticsWindow{From: now.Add(-time.Hour), To: now.Add(time.Hour), Bucket: time.Hour}
	credential := apiCredential{Hosted: owner.identity.Hosted}
	for _, name := range []string{"analytics", "cloud diagnostics", "hosted diagnostics"} {
		t.Run(name, func(t *testing.T) {
			reads := 0
			f.service.config.Content = func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
				if !request.Write {
					reads++
					if held := f.service.database.reader.Stats().InUse; held != 0 {
						t.Errorf("object download holds %d shared reader connections", held)
					}
				}
				return objects.call(ctx, request)
			}
			identity := operatortool.Identity{PrincipalID: owner.identity.Subject, OrganizationID: string(scope.organization), CredentialID: "test"}
			authorityCtx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
				return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error { return nil }}, nil
			}})
			readCtx := f.service.freshRunDetailContext(authorityCtx)
			var err error
			switch name {
			case "analytics":
				_, err = f.service.readAnalyticsReport(readCtx, credential, operatortool.AnalyticsRequest{Limit: 1}, w)
			case "cloud diagnostics":
				_, err = f.service.readCloudDiagnostics(readCtx, credential, w)
			case "hosted diagnostics":
				_, err = f.service.readHostedDiagnostics(readCtx, nativeAnalyticsReport{OrganizationID: string(scope.organization), Window: w, Projects: []nativeAnalyticsProject{{ProjectID: string(scope.project)}}})
			}
			if err != nil {
				t.Fatal(err)
			}
			if reads == 0 {
				t.Fatal("report did not download an attempt payload")
			}
		})
	}
}
