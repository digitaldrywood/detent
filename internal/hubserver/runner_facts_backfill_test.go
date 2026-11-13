package hubserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerCommandFacts(t *testing.T) {
	t.Parallel()
	clock := time.Now().UTC().Truncate(time.Microsecond)
	first := newDefaultNativeFixture(t, Config{now: func() time.Time { return clock }})
	second := newNativeFixture(t, first.service, first.project.OrganizationID, "other-command-facts")
	for _, f := range []nativeFixture{first, second} {
		credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
		content := eventstore.NewContent(f.service.runDetailStore())
		now := f.service.config.now().UTC().Truncate(time.Microsecond)
		store := func(key, operation, raw string, typed, referenced, pending bool, request string) eventstore.Receipt {
			t.Helper()
			historical := []byte(raw)
			if referenced {
				ref, err := content.PreparePayload(t.Context(), f.project.OrganizationID, historical)
				if err != nil {
					t.Fatal(err)
				}
				historical, err = f.service.runDetailStore().ReadDetail(t.Context(), string(f.project.OrganizationID), ref)
				if err != nil {
					t.Fatal(err)
				}
			}
			row, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID, ActorID: credential.ID, Operation: operation, CommandKey: key, RequestHash: artifact.Digest([]byte(key)), ResponseRequestID: "contradictory", CreatedAt: now, ResponseReplayUntilNs: now.Add(time.Hour).UnixNano()}, historical)
			if err != nil {
				t.Fatal(err)
			}
			if typed {
				row.ResponsePending, row.ResponseRequestID = pending, request
			}
			encodedSource, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			source := receiptstore.Import{Record: row, SnapshotID: "command-facts-" + string(f.project.ID), SourceHash: artifact.Digest(encodedSource), ObservedAt: now}
			for range 2 {
				if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.(receiptstore.ImportWriter).ImportReceipt(t.Context(), source)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			var saved eventstore.Receipt
			if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				saved, err = r.ReplayReceipt(t.Context(), credential.ID, strings.SplitN(operation, " ", 2)[0], key)
				if err != nil {
					return err
				}
				snapshot, hash, err := r.(receiptstore.ImportReader).ReceiptSource(t.Context(), credential.ID, operation, key)
				if err != nil {
					return err
				}
				if snapshot != source.SnapshotID || hash != source.SourceHash {
					return errors.New("receipt source provenance changed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			archived, err := content.Receipt(t.Context(), saved)
			if err != nil || string(archived) != raw || saved.ResponsePending != pending || saved.ResponseRequestID != request || saved.ResponseExpired || saved.Operation != operation || saved.ActorID != credential.ID || saved.CommandKey != key {
				t.Fatalf("typed=%t referenced=%t receipt=%s facts=%+v error=%v", typed, referenced, archived, saved, err)
			}
			return saved
		}
		for _, format := range []struct{ typed, referenced bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
			for i := range 2 {
				key := fmt.Sprintf("%s-command-%t-%t-%d", f.project.ID, format.typed, format.referenced, i)
				request := "request-" + key
				raw := fmt.Sprintf(`{"request":{"id":%q},"pending":true}`, request)
				if format.typed {
					raw = `{"request":{"id":"contradictory"}}`
				}
				store(key, "urgent_runner_update", raw, format.typed, format.referenced, true, request)
				operation := "drain_local_project " + key + " project"
				completed, running := `{"pending":false}`, `{"pending":true}`
				if format.typed {
					completed, running = `{"pending":true}`, `{"pending":false}`
				}
				done := store(key+"-done", operation, completed, format.typed, format.referenced, false, "")
				store(key+"-running", operation, running, format.typed, format.referenced, true, "")
				replay, found, err := f.service.configurationReceipt(t.Context(), scope, credential.ID, operation, done.CommandKey, done.RequestHash)
				if err != nil || !found || string(replay) != completed {
					t.Fatalf("typed=%t completed=%s operation=%s error=%v", format.typed, replay, operation, err)
				}
			}
		}
		if err := f.service.config.TenantRepository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.ReplayReceipt(t.Context(), credential.ID, "urgent_runner_update", "absent")
			if !errors.Is(err, corestore.ErrNotFound) {
				return fmt.Errorf("absent request: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfigurationReceiptsExcludeExpiredHistory(t *testing.T) {
	t.Parallel()
	for _, history := range []int{100, 10000} {
		t.Run(fmt.Sprint(history), func(t *testing.T) {
			clock := time.Now().UTC().Truncate(time.Microsecond)
			first := newDefaultNativeFixture(t, Config{now: func() time.Time { return clock }})
			second := newNativeFixture(t, first.service, first.project.OrganizationID, "other-configuration-receipts")
			for _, f := range []nativeFixture{first, second} {
				runner := prepareRunner(t, f, runnerauth.Read, runnerauth.Heartbeat)
				runner.enroll(t)
				now := f.service.config.now().UTC().Truncate(time.Microsecond)
				credential, _, err := f.service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
				if err != nil {
					t.Fatal(err)
				}
				scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
				approved := hubTestPolicy()
				approveHubTestPolicy(t, f.service, f.base+"/policy", approved)
				view := runnerauth.ProjectConfiguration{ProjectID: string(f.project.ID), ConfigRevision: strings.Repeat("a", 64), Registered: true, RuntimeRegistered: true, SelectedPolicy: &approved, EffectivePolicy: &approved, ObservedAt: now}
				seedHubConfiguration(t, f, runner.binding.RunnerID, view)
				operation := "drain_local_project " + runner.binding.RunnerID + " " + string(f.project.ID)
				operations := []string{operation, "resume_local_project " + runner.binding.RunnerID + " " + string(f.project.ID)}
				for _, op := range operations {
					for offset := 0; offset < history; offset += receiptstore.MaxImportBatch {
						var page []receiptstore.Import
						for i := offset; i < min(offset+receiptstore.MaxImportBatch, history); i++ {
							key := fmt.Sprintf("expired-%s-%d", f.project.ID, i)
							hash := artifact.Digest([]byte(op + key))
							page = append(page, receiptstore.Import{Record: receiptstore.Record{OrganizationID: f.project.OrganizationID, ActorID: credential.ID, Operation: "configuration result " + op, CommandKey: key, RequestHash: hash, CreatedAt: now.Add(-time.Hour), ResponseExpired: true, ResponseReplayUntilNs: now.Add(-time.Minute).UnixNano()}, SnapshotID: "expired-history-" + string(f.project.ID), SourceHash: hash, ObservedAt: now})
						}
						if err := f.service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Background, func(w tenantstore.Writer) error {
							return w.(receiptstore.ImportWriter).ImportReceiptStates(t.Context(), page)
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				repository := f.service.config.TenantRepository
				counted := &runnerFactsReceiptRepository{Repository: repository}
				readService := &Service{database: f.service.database, config: f.service.config}
				readService.config.TenantRepository = counted
				read := func(live bool) {
					t.Helper()
					counted.lookups = nil
					plan, err := readService.readProjectConfigurationPlan(t.Context(), scope, operatortool.LocalProjectArguments{ProjectID: string(f.project.ID), RunnerID: runner.binding.RunnerID})
					if err != nil {
						t.Fatal(err)
					}
					if !live {
						if plan.View.LastOperation != nil || len(counted.lookups) != 0 {
							t.Fatalf("expired history returned completion: view=%+v reads=%v", plan.View, counted.lookups)
						}
					} else if plan.Command.Operation != operation || plan.Command.RequestID != "live-done" || plan.View.LastOperation == nil || plan.View.LastOperation.RequestID != "live-done" || plan.View.LastOperation.Operation != "drain_local_project" || !plan.View.LastOperation.Applied || len(counted.lookups) != 1 || counted.lookups[0] != [3]string{credential.ID, "configuration result " + operation, "live-done"} {
						t.Fatalf("current completed receipt lost among history: view=%+v reads=%v", plan.View, counted.lookups)
					}
				}
				read(false)
				completed := view
				completed.RequestID, completed.Applied, completed.Saved = "live-done", true, true
				raw, err := json.Marshal(completed)
				if err != nil {
					t.Fatal(err)
				}
				content := eventstore.NewContent(f.service.runDetailStore())
				receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID, ActorID: credential.ID, Operation: "configuration result " + operation, CommandKey: "live-done", RequestHash: artifact.Digest([]byte(operation)), CreatedAt: now.Add(-2 * time.Hour), ResponseReplayUntilNs: now.Add(time.Hour).UnixNano()}, raw)
				if err != nil {
					t.Fatal(err)
				}
				pending := completed
				pending.RequestID, pending.Pending, pending.Applied = "live-running", true, false
				raw, err = json.Marshal(pending)
				if err != nil {
					t.Fatal(err)
				}
				running, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: f.project.OrganizationID, ActorID: credential.ID, Operation: receipt.Operation, CommandKey: "live-running", RequestHash: artifact.Digest([]byte(operation + "running")), CreatedAt: now.Add(-time.Minute), ResponseReplayUntilNs: now.Add(time.Hour).UnixNano()}, raw)
				if err != nil {
					t.Fatal(err)
				}
				intent, err := f.service.prepareConfigurationPayload(t.Context(), f.project.OrganizationID, configurationIntent{Arguments: operatortool.LocalProjectArguments{ProjectID: string(f.project.ID), RunnerID: runner.binding.RunnerID, RequestID: "live-done"}, Previous: view, Desired: completed})
				if err != nil {
					t.Fatal(err)
				}
				if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					if _, err := w.PutReceipt(t.Context(), receipt); err != nil {
						return err
					}
					if _, err := w.PutReceipt(t.Context(), running); err != nil {
						return err
					}
					owner := w.(postgres.ConfigurationWriter)
					row, err := owner.ConfigurationProject(t.Context(), runner.binding.RunnerID, f.project.ID)
					if err != nil {
						return err
					}
					identity, err := w.RunnerIdentity(t.Context(), runner.binding.RunnerID)
					if err != nil {
						return err
					}
					return owner.SetConfigurationCommand(t.Context(), postgres.ConfigurationCommand{OrganizationID: f.project.OrganizationID, RunnerID: runner.binding.RunnerID, ProjectID: f.project.ID, RequestID: "live-done", Operation: operation, ActorID: credential.ID, RequestHash: receipt.RequestHash, RunnerRevision: identity.Revision, Status: "completed", Intent: intent}, row.Revision)
				}); err != nil {
					t.Fatal(err)
				}
				read(true)

				if err := repository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
					expired, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), receipt, now.Add(2*time.Hour))
					if err != nil {
						return err
					}
					if !expired {
						return errors.New("current completed receipt did not expire")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				counted.lookups = nil
				if _, err := readService.readProjectConfigurationPlan(t.Context(), scope, operatortool.LocalProjectArguments{ProjectID: string(f.project.ID), RunnerID: runner.binding.RunnerID}); !errors.Is(err, receiptstore.ErrExpired) || len(counted.lookups) != 1 {
					t.Fatalf("expired current completion fell back to history: error=%v reads=%v", err, counted.lookups)
				}
			}
		})
	}
}

type runnerFactsReceiptRepository struct {
	tenantstore.Repository
	lookups [][3]string
}

func (r *runnerFactsReceiptRepository) ReadTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, read func(tenantstore.Reader) error) error {
	return r.Repository.ReadTenant(ctx, organization, workload, func(reader tenantstore.Reader) error {
		return read(&runnerFactsReceiptReader{Reader: reader, ConfigurationReader: reader.(postgres.ConfigurationReader), repository: r})
	})
}

type runnerFactsReceiptReader struct {
	tenantstore.Reader
	postgres.ConfigurationReader
	repository *runnerFactsReceiptRepository
}

func (r *runnerFactsReceiptReader) Receipt(ctx context.Context, actor, operation, key string) (eventstore.Receipt, error) {
	r.repository.lookups = append(r.repository.lookups, [3]string{actor, operation, key})
	return r.Reader.Receipt(ctx, actor, operation, key)
}

func setSpriteFactsFixtureMode(t *testing.T, _ nativeFixture, _ bool) {
	t.Helper()
}
