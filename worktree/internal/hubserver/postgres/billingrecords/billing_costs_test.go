package billingrecords_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/usagecost"
)

func TestBillingCostsCorrectMonthlySpendWithoutDoubleCounting(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	boundary := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedTenantAccess(t, repository, org)
		for _, project := range []tracker.ProjectID{"project_first", "project_second"} {
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.CreateProject(t.Context(), corestore.Project{OrganizationID: org, ID: project, Name: string(project), Profile: "native"})
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		amount := int64(101)
		base := billingstore.CostObservation{OrganizationID: org, ProjectID: "project_first", ReceivedAt: boundary.Add(time.Hour + time.Nanosecond), Observation: usagecost.Observation{Provider: "example", ProviderAccount: "account", ResourceID: "resource", SourceID: "source", Bucket: "runner_api", Metric: "tokens", Currency: "USD", Unit: "tokens", Revision: 1, From: boundary.Add(-time.Nanosecond), To: boundary.Add(time.Nanosecond), AmountMicros: &amount}}
		historyImport := false
		write := func(record billingstore.CostObservation, applied bool, wantErr error, rollback bool) {
			t.Helper()
			raw, err := json.Marshal(record.Observation)
			if err != nil {
				t.Fatal(err)
			}
			archive, _, err := attachment.PrepareArchive(string(org), 1, bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			record.Reference = billingstore.AuditReferenceFromArchive(archive)
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var got bool
				var err error
				if historyImport {
					got, err = w.ImportBillingCostRevision(t.Context(), record)
				} else {
					got, err = w.RecordBillingCost(t.Context(), record)
				}
				if got != applied {
					t.Errorf("applied = %v, want %v", got, applied)
				}
				if err == nil && rollback {
					return mutation.ErrConflict
				}
				return err
			})
			if !errors.Is(err, wantErr) {
				t.Fatalf("write error = %v, want %v", err, wantErr)
			}
		}
		assertSpend := func(project tracker.ProjectID, january, february int64) {
			t.Helper()
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				for _, period := range []struct {
					at   time.Time
					want int64
				}{{boundary.Add(-time.Nanosecond), january}, {boundary, february}} {
					spend, err := r.BillingMonthlySpend(t.Context(), project, period.at)
					if err != nil {
						return err
					}
					if spend.RunnerMicros != period.want || spend.LunaMicros != 0 || spend.SpriteMicros != 0 {
						t.Errorf("project %s month %v spend=%+v want runner=%d", project, period.at, spend, period.want)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		write(base, true, nil, false)
		write(base, false, nil, false)
		assertSpend("project_first", 50, 51)
		assertSpend("project_second", 0, 0)
		changed := base
		otherAmount := int64(103)
		changed.Observation.AmountMicros = &otherAmount
		write(changed, false, mutation.ErrConflict, false)
		for _, mutate := range []func(*usagecost.Observation){
			func(o *usagecost.Observation) { o.RunnerID = "different_runner" },
			func(o *usagecost.Observation) { o.MachineID = "different_machine" },
			func(o *usagecost.Observation) { o.Placement = "sprite" },
		} {
			reattributed := base
			reattributed.Observation.Revision = 2
			mutate(&reattributed.Observation)
			write(reattributed, false, mutation.ErrConflict, false)
		}
		changed.Observation.Revision = 2
		write(changed, true, mutation.ErrConflict, true)
		assertSpend("", 50, 51)
		write(changed, true, nil, false)
		write(base, false, nil, false)
		assertSpend("", 51, 52)
		overlap := base
		overlap.Observation.SourceID = "overlapping"
		write(overlap, false, mutation.ErrConflict, false)
		other := base
		other.ProjectID = "project_second"
		other.Observation.ResourceID = "second_resource"
		other.Observation.SourceID = "second_source"
		write(other, true, nil, false)
		assertSpend("project_second", 50, 51)
		assertSpend("", 101, 103)
		changed.Observation.Revision = 3
		changed.Observation.Voided = true
		write(changed, true, nil, false)
		assertSpend("project_first", 0, 0)
		assertSpend("", 50, 51)
		historical := changed
		historical.Observation.SourceID = "history_source"
		historical.Observation.ResourceID = other.Observation.ResourceID
		write(historical, true, nil, false)
		historyImport = true
		historical.Observation.Revision = 1
		historical.Observation.Voided = false
		write(historical, true, nil, false)
		write(historical, false, nil, false)
		historical.Observation.Revision = 4
		write(historical, false, mutation.ErrConflict, false)
		assertSpend("", 50, 51)
		historyImport = false
		write(historical, false, mutation.ErrConflict, false)
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			latest, err := r.BillingCostRevision(t.Context(), billingstore.CostSource{Provider: base.Observation.Provider, ProviderAccount: base.Observation.ProviderAccount, SourceID: base.Observation.SourceID, From: base.Observation.From, To: base.Observation.To})
			if err != nil {
				return err
			}
			if !latest.ReceivedAt.Equal(base.ReceivedAt) || latest.Revision != 3 || latest.ProjectID != "project_first" || latest.Reference.ArchiveReference().Validate(string(org)) != nil {
				t.Errorf("latest revision lost correction or tenant evidence: %+v", latest)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoredUsageArchivesRevalidateWithoutRecompression(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	ctx := t.Context()
	at := time.Now().UTC().Truncate(time.Microsecond)
	for _, org := range []tracker.OrganizationID{"org_stored_first", "org_stored_second"} {
		if _, err := admin.ExecContext(ctx, `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, pgstoretest.HistoryAt()); err != nil {
			t.Fatal(err)
		}
		pgstoretest.SeedLeaseFamily(t, repository, org)
		lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		objects := pgstoretest.NewHistoryObjects()
		payload, err := objects.StoreDetail(ctx, string(org), []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordAttempt(ctx, leasestore.AttemptEvent{ID: "attempt_stored", Fence: pgstoretest.LeaseFence(lease), RunID: "run_stored", Sequence: 1, Status: "running", StartedAt: pgstoretest.HistoryAt(), PayloadObjectKey: payload.Key})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		cost := billingCostObservation(t, org, "prj_1", "stored_source", "attempt_stored", "runner_api", at.Add(-time.Hour), at, 1000000)
		o := &cost.Observation
		o.AttemptID, o.WorkItemID, o.Model, o.Input, o.Coverage, o.UsageKind = "attempt_stored", "wi_1", "stored-model", 100, "complete", "cumulative"
		cost.ReceivedAt = at.Add(-time.Hour)
		raw, err := json.Marshal(cost.Observation)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := objects.StoreDetail(ctx, string(org), raw)
		if err != nil {
			t.Fatal(err)
		}
		cost.Reference = billingstore.AuditReferenceFromArchive(canonical)
		if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RecordBillingCosts(ctx, []billingstore.CostObservation{cost})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var compressed bytes.Buffer
		writer, err := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		writer.Name = "observation.json"
		if _, err := writer.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		stored := attachment.ArchiveReference{SHA256: artifact.Digest(compressed.Bytes()), CompressedSize: int64(compressed.Len()), UncompressedSize: int64(len(raw)), Codec: "gzip", SchemaVersion: 1, Kind: "archive", Retention: attachment.ArchiveRetention}
		if stored.Key, err = attachment.ArchiveKey(string(org), stored.SHA256); err != nil {
			t.Fatal(err)
		}
		if decoded, err := attachment.DecodeArchive(string(org), stored, compressed.Bytes()); err != nil || !bytes.Equal(decoded, raw) {
			t.Fatalf("historical archive does not decode: %v", err)
		}
		if again, _, err := attachment.PrepareArchive(string(org), 1, bytes.NewReader(raw)); err != nil || again.SHA256 == stored.SHA256 {
			t.Fatalf("historical archive recompresses to its stored digest, so it cannot pin the regression: %v", err)
		}
		objects.Mu.Lock()
		objects.Objects[stored.Key] = bytes.Clone(compressed.Bytes())
		objects.Mu.Unlock()
		var sourceKey string
		if err := admin.QueryRowContext(ctx, `SELECT source_key FROM cost_observation_revisions WHERE organization_id=$1 AND object_key=$2 AND revision=1`, org, canonical.Key).Scan(&sourceKey); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(ctx, `WITH changed AS (UPDATE cost_observation_revisions SET object_key=$3,sha256=$4,compressed_bytes=$5,content_bytes=$6 WHERE organization_id=$1 AND source_key=$2 AND revision=1 RETURNING source_key) UPDATE cost_sources SET payload_hash=$4 WHERE organization_id=$1 AND source_key IN(SELECT source_key FROM changed)`, org, sourceKey, stored.Key, stored.SHA256, stored.CompressedSize, stored.UncompressedSize); err != nil {
			t.Fatal(err)
		}

		t.Run(string(org)+"/maintenance refresh", func(t *testing.T) {
			for _, test := range []struct {
				name    string
				object  []byte
				wantErr bool
			}{
				{name: "corrupt stored bytes are rejected", object: append(bytes.Clone(compressed.Bytes()[:compressed.Len()-1]), compressed.Bytes()[compressed.Len()-1]^0xff), wantErr: true},
				{name: "historical gzip encoding refreshes", object: compressed.Bytes()},
			} {
				t.Run(test.name, func(t *testing.T) {
					objects.Mu.Lock()
					objects.Objects[stored.Key] = bytes.Clone(test.object)
					objects.Mu.Unlock()
					t.Cleanup(func() {
						objects.Mu.Lock()
						objects.Objects[stored.Key] = bytes.Clone(compressed.Bytes())
						objects.Mu.Unlock()
					})
					after, count, err := repository.RefreshReportUsagePage(ctx, org, objects, "", 100)
					if test.wantErr {
						if err == nil || after != "" || count != 0 {
							t.Fatalf("corrupt archive advanced refresh: cursor=%q count=%d error=%v", after, count, err)
						}
						return
					}
					if err != nil || count != 1 || after != sourceKey {
						t.Fatalf("refresh cursor=%q count=%d error=%v", after, count, err)
					}
					var input, revision int64
					if err := admin.QueryRowContext(ctx, `SELECT input,revision FROM report_usage_current WHERE organization_id=$1 AND source_key=$2`, org, sourceKey).Scan(&input, &revision); err != nil {
						t.Fatal(err)
					}
					if input != 100 || revision != 1 {
						t.Fatalf("refreshed usage input=%d revision=%d", input, revision)
					}
				})
			}
		})

		t.Run(string(org)+"/run event history", func(t *testing.T) {
			verify := func(ref attachment.ArchiveReference) attachment.VerifiedDetail {
				t.Helper()
				verified, err := attachment.ReadVerifiedDetail(ctx, objects, string(org), ref)
				if err != nil {
					t.Fatal(err)
				}
				return verified
			}
			verified := verify(stored)
			content, _ := verified.Content(stored)
			var observation usagecost.Observation
			if err := json.Unmarshal(content, &observation); err != nil {
				t.Fatal(err)
			}
			historical := billingstore.CostObservation{OrganizationID: org, ProjectID: "prj_1", Observation: observation, Reference: billingstore.AuditReferenceFromArchive(stored), ReceivedAt: cost.ReceivedAt, Stored: verified}
			forged := observation
			forged.Input = 900
			forgedRaw, err := json.Marshal(forged)
			if err != nil {
				t.Fatal(err)
			}
			if len(forgedRaw) != len(content) {
				t.Fatalf("forged content length %d differs from stored %d", len(forgedRaw), len(content))
			}
			var forgedArchive bytes.Buffer
			forgedWriter := gzip.NewWriter(&forgedArchive)
			if _, err := forgedWriter.Write(forgedRaw); err != nil {
				t.Fatal(err)
			}
			if err := forgedWriter.Close(); err != nil {
				t.Fatal(err)
			}
			objects.Mu.Lock()
			objects.Objects[stored.Key] = forgedArchive.Bytes()
			objects.Mu.Unlock()
			_, forgedErr := attachment.ReadVerifiedDetail(ctx, objects, string(org), stored)
			objects.Mu.Lock()
			objects.Objects[stored.Key] = bytes.Clone(compressed.Bytes())
			objects.Mu.Unlock()
			if !errors.Is(forgedErr, artifact.ErrIntegrity) {
				t.Fatalf("content not matching the stored digest was verified: %v", forgedErr)
			}
			foreign := tracker.OrganizationID("org_stored_first")
			if org == foreign {
				foreign = "org_stored_second"
			}
			for _, test := range []struct {
				name   string
				change func(*billingstore.CostObservation)
				want   error
			}{
				{name: "foreign tenant is rejected", change: func(r *billingstore.CostObservation) { r.OrganizationID = foreign }, want: corestore.ErrTenantMismatch},
				{name: "observation differing from verified content is rejected", change: func(r *billingstore.CostObservation) { r.Observation = forged }, want: billingstore.ErrInvalidUsage},
				{name: "missing verified content is rejected", change: func(r *billingstore.CostObservation) { r.Stored = attachment.VerifiedDetail{} }, want: billingstore.ErrInvalidUsage},
				{name: "content verified under another reference is rejected", change: func(r *billingstore.CostObservation) { r.Stored = verify(canonical) }, want: billingstore.ErrInvalidUsage},
				{name: "reference not matching its verified digest is rejected", change: func(r *billingstore.CostObservation) { r.Reference.SHA256 = strings.Repeat("0", 64) }, want: billingstore.ErrInvalidUsage},
				{name: "verified archive that is not the persisted revision is rejected", change: func(r *billingstore.CostObservation) {
					r.Reference, r.Stored = billingstore.AuditReferenceFromArchive(canonical), verify(canonical)
				}, want: corestore.ErrRevisionConflict},
				{name: "historical gzip encoding is accepted"},
			} {
				t.Run(test.name, func(t *testing.T) {
					record := historical
					if test.change != nil {
						test.change(&record)
					}
					report := reportstore.AttemptUsage{OrganizationID: org, ProjectID: "prj_1", WorkItemID: "wi_1", AttemptID: "attempt_stored", LeaseID: lease.LeaseID, FencingToken: lease.FencingToken, Sequence: 1, ReceivedAt: at, Currency: "USD", Totals: []reportstore.AttemptUsageTotal{{Provider: observation.Provider, Model: "stored-model", Input: 250, ReportedCost: new(2.5)}}, Historical: []billingstore.CostObservation{record}}
					err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.(reportstore.UsageWriter).RecordAttemptUsage(ctx, report)
					})
					if test.want != nil {
						if !errors.Is(err, test.want) {
							t.Fatalf("error=%v want %v", err, test.want)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					var input int64
					var cost float64
					if err := admin.QueryRowContext(ctx, `SELECT sum(input),sum(cost_estimate) FROM attempt_usage WHERE organization_id=$1 AND attempt_id='attempt_stored' AND model='stored-model'`, org).Scan(&input, &cost); err != nil {
						t.Fatal(err)
					}
					if input != 250 || cost != 2.5 {
						t.Fatalf("usage over stored baseline input=%d cost=%v want 250/2.5", input, cost)
					}
				})
			}
		})
	}
}
