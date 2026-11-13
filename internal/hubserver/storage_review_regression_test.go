package hubserver

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeCommandReceiptsExcludeEvidence(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	service := openTestService(t, Config{Content: objects.call})
	if err := service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	f := newChangeFixture(t, service)
	for _, size := range []int{32, 60 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			request := tracker.DiscussChange{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("discussion-%d", size)}, Body: strings.Repeat("x", size)}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/discussion", f.token, request)
			requireNativeStatus(t, response, http.StatusOK)
			admin := usagePostgresAdmin(t, f.service)
			var inlineColumns, receipts int
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM information_schema.columns WHERE table_schema='public' AND table_name='native_commands' AND (data_type IN ('json','jsonb') OR column_name='response_json')").Scan(&inlineColumns); err != nil || inlineColumns != 0 {
				t.Fatalf("command receipt inline columns=%d error=%v", inlineColumns, err)
			}
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM native_commands WHERE organization_id=$1 AND command_key=$2 AND payload_object_key<>'' AND payload_bytes>0", f.project.OrganizationID, request.IdempotencyKey).Scan(&receipts); err != nil || receipts != 1 {
				t.Fatalf("object-backed command receipts=%d error=%v", receipts, err)
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
	t.Run("diagnostics", func(t *testing.T) {
		for _, fixture := range diagnosticsPostgresFixtures(t) {
			t.Run(string(fixture.organization), func(t *testing.T) {
				now := fixture.hosted.service.config.now().UTC().Truncate(time.Hour)
				diagnosticsFixtureAttempts(t.Context(), t, fixture, fixture.hosted.project, "download-source", "download", 1, 1, now.Add(-2*time.Hour), now.Add(-time.Hour), tracker.NativeRunData{Outcome: "failed", TerminalFailure: &tracker.NativeTerminalFailure{Error: "Recorded failure"}})
				fixture.hosted.service.tokenUseWork.Wait()
				guard := &diagnosticsSnapshotGuard{Repository: fixture.repository}
				fixture.hosted.service.config.TenantRepository = guard
				t.Cleanup(func() { fixture.hosted.service.config.TenantRepository = fixture.repository })
				credential, status, err := fixture.hosted.service.hostedSessionCredential(t.Context(), auth.Session{Identity: fixture.owner.identity.Hosted, Email: fixture.owner.identity.Email}, apikey.HashToken(fixture.owner.token))
				if err != nil || status != http.StatusOK {
					t.Fatalf("fixture owner authorization: status=%d error=%v", status, err)
				}
				w := operatortool.AnalyticsWindow{From: now.Add(-24 * time.Hour), To: now, Bucket: time.Hour}
				for _, name := range []string{"analytics", "cloud diagnostics", "project diagnostics"} {
					t.Run(name, func(t *testing.T) {
						reads := 0
						fixture.details.beforeRead = func(context.Context) error {
							reads++
							if held := fixture.repository.Stats().AcquiredConns(); held != 0 {
								t.Errorf("object download holds %d shared PostgreSQL connections", held)
							}
							if held := guard.active.Load(); held != 0 {
								t.Errorf("object download holds %d shared reader snapshots", held)
							}
							return nil
						}
						defer func() { fixture.details.beforeRead = nil }()
						ctx := fixture.hosted.service.freshRunDetailContext(t.Context())
						var err error
						if name == "analytics" {
							identity := operatortool.Identity{PrincipalID: fixture.owner.identity.Subject, OrganizationID: string(fixture.organization), CredentialID: "test"}
							ctx = operatortool.WithConnection(ctx, operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
								return operatortool.Authority{Identity: identity, Check: func(context.Context, operatortool.Requirement) error { return nil }}, nil
							}})
							_, err = fixture.hosted.service.readAnalyticsReport(ctx, credential, operatortool.AnalyticsRequest{Limit: 1}, w)
						} else if name == "cloud diagnostics" {
							_, err = fixture.hosted.service.readCloudDiagnostics(ctx, credential, w)
						} else {
							_, err = fixture.hosted.service.readDiagnosticsTenant(ctx, credential, w, fixture.hosted.project)
						}
						if err != nil {
							t.Fatal(err)
						}
						if reads == 0 {
							t.Fatal("report did not download an attempt payload")
						}
					})
				}
			})
		}
	})
}
