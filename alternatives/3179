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
