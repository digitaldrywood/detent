package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var _ reportstore.UsageReader = (*session)(nil)

func (s *session) ReadReportUsage(ctx context.Context, query reportstore.OverviewQuery) ([]reportstore.UsageFact, error) {
	if query.Limit < 1 || query.Limit > 100000 {
		return nil, errors.New("invalid report usage limit")
	}
	attempts, err := s.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: query.Projects, Principal: query.Principal, From: query.From, To: query.To, Filter: query.Filter, Limit: 50000})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(attempts))
	for i, a := range attempts {
		ids[i] = a.AttemptID
	}
	rows, err := s.db.QueryContext(ctx, `WITH usage AS (
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,'USD' AS currency FROM report_usage_current u WHERE u.organization_id=$1 AND u.attempt_id=ANY($2::text[])
 UNION ALL
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=ANY($2::text[]) AND NOT EXISTS(SELECT 1 FROM report_usage_current c WHERE c.organization_id=u.organization_id AND c.attempt_id=u.attempt_id)
 ) SELECT i.project_id,i.native_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,coalesce(r.runner_id,''),coalesce(r.display_name,'') FROM usage u
 JOIN native_attempts a ON a.organization_id=u.organization_id AND a.id=u.attempt_id JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=u.project_id
 LEFT JOIN runner_lease_assignments la ON la.organization_id=a.organization_id AND la.lease_id=a.lease_id LEFT JOIN runner_identities r ON r.organization_id=la.organization_id AND r.runner_id=la.runner_id
 WHERE u.period>=$3 AND u.period<$4 AND upper(u.currency)='USD' ORDER BY u.period,u.attempt_id,u.provider,u.model LIMIT $5`, s.organization, pq.Array(ids), query.From.UTC(), query.To.UTC(), query.Limit+1)
	if err != nil {
		return nil, mapError(ctx, err)
	}
	defer rows.Close()
	out := []reportstore.UsageFact{}
	for rows.Next() {
		var fact reportstore.UsageFact
		if err := rows.Scan(&fact.ProjectID, &fact.WorkItemID, &fact.AttemptID, &fact.Period, &fact.Provider, &fact.Model, &fact.Input, &fact.CachedInput, &fact.Output, &fact.Cost, &fact.Currency, &fact.RunnerID, &fact.RunnerName); err != nil {
			return nil, mapError(ctx, err)
		}
		out = append(out, fact)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, mapError(ctx, err)
	}
	if len(out) > query.Limit {
		return nil, reportstore.ErrPopulationExceeded
	}
	return out, nil
}

func (r *Repository) ImportReportUsageFact(ctx context.Context, record reportstore.UsageRecord) error {
	return r.ImportReportUsageFacts(ctx, []reportstore.UsageRecord{record})
}

func (r *Repository) ImportReportUsageFacts(ctx context.Context, records []reportstore.UsageRecord) error {
	if len(records) < 1 || len(records) > 100 || records[0].OrganizationID == "" {
		return errors.New("invalid typed report usage batch")
	}
	organization := records[0].OrganizationID
	for _, record := range records {
		if record.OrganizationID != organization || record.ProjectID == "" || record.AttemptID == "" || record.WorkItemID == "" || record.Period.IsZero() || record.UpdatedAt.IsZero() || record.Provider == "" || record.Model == "" || record.Input < 0 || record.CachedInput < 0 || record.Output < 0 || record.Cost < 0 || math.IsInf(record.Cost, 0) || math.IsNaN(record.Cost) || !strings.EqualFold(record.Currency, "usd") {
			return errors.New("invalid typed report usage fact")
		}
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		return s.importReportUsage(ctx, records)
	})
}

func (s *session) RefreshReportUsage(ctx context.Context, records []billingstore.CostObservation) error {
	if len(records) < 1 || len(records) > 128 {
		return errors.New("invalid report usage refresh batch")
	}
	selected := make([]billingstore.CostObservation, 0, len(records))
	positions := map[string]int{}
	for _, record := range records {
		if err := s.validateBillingCost(record); err != nil {
			return err
		}
		o := record.Observation
		if record.OrganizationID != s.organization || record.ProjectID == "" || o.AttemptID == "" || o.WorkItemID == "" || o.Input < 0 || o.CachedInput < 0 || o.Output < 0 {
			return errors.New("invalid report usage attribution")
		}
		key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
		if index, ok := positions[key]; ok {
			previous := selected[index]
			if previous.Observation.Revision == o.Revision && (previous.ProjectID != record.ProjectID || previous.Reference.SHA256 != record.Reference.SHA256 || !reflect.DeepEqual(previous.Observation, o)) {
				return corestore.ErrRevisionConflict
			}
			if previous.Observation.Revision < o.Revision {
				selected[index] = record
			}
			continue
		}
		positions[key] = len(selected)
		selected = append(selected, record)
	}
	b := newReportBatch(s.organization, "source_key,revision,payload_sha256,project_id,work_item_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete", "text", "bigint", "text", "text", "text", "text", "timestamptz", "text", "text", "bigint", "bigint", "bigint", "double precision", "boolean")
	for _, record := range selected {
		o := record.Observation
		key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))

		amount := o.EstimatedAmountMicros
		if amount == nil {
			amount = o.AmountMicros
		}
		if amount == nil {
			amount = o.ReportedAmountMicros
		}
		known := amount != nil && strings.EqualFold(o.Currency, "USD") && o.Coverage == "complete"
		cost := float64(0)
		if amount != nil {
			if *amount < 0 {
				return errors.New("negative report usage amount")
			}
			cost = float64(*amount) / 1e6
		}
		input, cached, output := o.Input, o.CachedInput, o.Output
		if o.Voided {
			cost = 0
			input, cached, output = 0, 0, 0
			known = true
		}
		period := recordTime(record.ReceivedAt).Truncate(time.Hour)
		if record.ReceivedAt.IsZero() {
			return errors.New("missing report usage receipt time")
		}
		b.add(key, o.Revision, record.Reference.SHA256, record.ProjectID, o.WorkItemID, o.AttemptID, period, o.Provider, o.Model, input, cached, output, cost, known)
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN cost_sources c ON c.organization_id=$1 AND c.source_key=source.source_key AND c.resource_id=source.attempt_id AND c.bucket='runner_api' WHERE c.revision>source.revision OR (c.revision=source.revision AND c.payload_hash=source.payload_sha256)`, corestore.ErrRevisionConflict); err != nil {
		return err
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN cost_sources c ON c.organization_id=$1 AND c.source_key=source.source_key WHERE c.revision>source.revision OR EXISTS(SELECT 1 FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND a.id=source.attempt_id AND i.project_id=source.project_id AND i.native_id=source.work_item_id)`, corestore.ErrTenantMismatch); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, b.source()+`INSERT INTO report_usage_current(organization_id,source_key,revision,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete)
 SELECT $1,s.source_key,s.revision,s.project_id,s.attempt_id,s.period,s.provider,s.model,s.input,s.cached_input,s.output,s.cost_estimate,s.complete FROM source s JOIN cost_sources c ON c.organization_id=$1 AND c.source_key=s.source_key AND c.revision=s.revision ORDER BY s.source_key
 ON CONFLICT(organization_id,source_key) DO UPDATE SET revision=EXCLUDED.revision,input=EXCLUDED.input,cached_input=EXCLUDED.cached_input,output=EXCLUDED.output,cost_estimate=EXCLUDED.cost_estimate,complete=EXCLUDED.complete WHERE report_usage_current.revision<=EXCLUDED.revision`, b.args...)
	return mapError(ctx, err)
}

func (s *session) ReadReportUsageCoverage(ctx context.Context, query reportstore.OverviewQuery) (bool, error) {
	attempts, err := s.ReadAttemptFacts(ctx, reportstore.AttemptQuery{Projects: query.Projects, Principal: query.Principal, From: query.From, To: query.To, Filter: query.Filter, Limit: 50000})
	if err != nil {
		return false, err
	}
	ids := make([]string, len(attempts))
	for i, a := range attempts {
		ids[i] = a.AttemptID
	}
	rows, err := s.db.QueryContext(ctx, `WITH sources AS MATERIALIZED(SELECT c.source_key,c.resource_id,c.revision FROM cost_sources c WHERE c.organization_id=$1 AND c.bucket='runner_api' AND c.resource_id=ANY($2::text[]) ORDER BY c.resource_id,c.source_key LIMIT 100001)
 SELECT c.resource_id,u.complete AND u.revision=c.revision FROM sources c LEFT JOIN report_usage_current u ON u.organization_id=$1 AND u.source_key=c.source_key`, s.organization, pq.Array(ids))
	if err != nil {
		return false, mapError(ctx, err)
	}
	seen := map[string]bool{}
	complete := true
	count := 0
	for rows.Next() {
		var id string
		var known sql.NullBool
		if err := rows.Scan(&id, &known); err != nil {
			rows.Close()
			return false, mapError(ctx, err)
		}
		seen[id] = true
		complete = complete && known.Valid && known.Bool
		count++
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, mapError(ctx, err)
	}
	if count > 100000 {
		return false, reportstore.ErrPopulationExceeded
	}
	missing := []string{}
	for _, id := range ids {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	rows, err = s.db.QueryContext(ctx, `WITH facts AS MATERIALIZED(SELECT u.attempt_id FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=ANY($2::text[]) ORDER BY u.attempt_id,u.period,u.provider,u.model LIMIT 100001) SELECT f.attempt_id,count(*),coalesce(c.row_count,0) FROM facts f LEFT JOIN report_usage_import_coverage c ON c.organization_id=$1 AND c.attempt_id=f.attempt_id GROUP BY f.attempt_id,c.row_count`, s.organization, pq.Array(missing))
	if err != nil {
		return false, mapError(ctx, err)
	}
	imported := map[string]bool{}
	count = 0
	for rows.Next() {
		var id string
		var actual, expected int
		if err := rows.Scan(&id, &actual, &expected); err != nil {
			rows.Close()
			return false, mapError(ctx, err)
		}
		count += actual
		imported[id] = actual == expected && expected > 0
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, mapError(ctx, err)
	}
	if count > 100000 {
		return false, reportstore.ErrPopulationExceeded
	}
	for _, id := range missing {
		complete = complete && imported[id]
	}
	return complete, nil
}

func (r *Repository) VerifyReportUsageImport(ctx context.Context, organization tracker.OrganizationID, attempt, sha string, expected int64) error {
	return r.VerifyReportUsageImports(ctx, organization, sha, []ReportUsageImportCoverage{{AttemptID: attempt, RowCount: expected}})
}

type ReportUsageImportCoverage struct {
	AttemptID string
	RowCount  int64
}

func (r *Repository) VerifyReportUsageImports(ctx context.Context, organization tracker.OrganizationID, sha string, records []ReportUsageImportCoverage) error {
	if organization == "" || len(sha) != 64 || len(records) < 1 || len(records) > 100 {
		return errors.New("invalid report usage import coverage")
	}
	for _, record := range records {
		if record.AttemptID == "" || record.RowCount < 1 {
			return errors.New("invalid report usage import coverage")
		}
	}
	return r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		return s.verifyReportUsageBatch(ctx, sha, records)
	})
}

func (r *Repository) RefreshReportUsagePage(ctx context.Context, organization tracker.OrganizationID, objects attachment.DetailStore, after string, limit int) (string, int, error) {
	if organization == "" || objects == nil || limit < 1 || limit > 100 {
		return after, 0, errors.New("invalid report usage refresh page")
	}
	type row struct {
		key    string
		record billingstore.CostObservation
	}
	page := []row{}
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		rows, err := s.db.QueryContext(ctx, `SELECT c.source_key,c.project_id,c.revision,r.received_at,r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind,r.retention FROM cost_sources c JOIN native_attempts a ON a.organization_id=c.organization_id AND a.id=c.resource_id JOIN cost_observation_revisions r ON r.organization_id=c.organization_id AND r.source_key=c.source_key AND r.revision=c.revision WHERE c.organization_id=$1 AND c.bucket='runner_api' AND c.source_key>$2 ORDER BY c.source_key LIMIT $3`, organization, after, limit)
		if err != nil {
			return mapError(ctx, err)
		}
		for rows.Next() {
			var value row
			var received string
			value.record.OrganizationID = organization
			ref := &value.record.Reference
			if err := rows.Scan(&value.key, &value.record.ProjectID, &value.record.Observation.Revision, &received, &ref.BindingID, &ref.CustomerOwned, &ref.Key, &ref.SHA256, &ref.CompressedBytes, &ref.ContentBytes, &ref.Codec, &ref.FormatVersion, &ref.Kind, &ref.Retention); err != nil {
				rows.Close()
				return mapError(ctx, err)
			}
			value.record.ReceivedAt, err = time.Parse(time.RFC3339Nano, received)
			if err != nil {
				rows.Close()
				return err
			}
			page = append(page, value)
		}
		return mapError(ctx, errors.Join(rows.Err(), rows.Close()))
	})
	if err != nil {
		return after, 0, err
	}
	prepared := make([]billingstore.CostObservation, 0, len(page))
	for _, value := range page {
		raw, err := objects.ReadDetail(ctx, string(organization), value.record.Reference.ArchiveReference())
		if err != nil {
			return after, 0, err
		}
		if err := json.Unmarshal(raw, &value.record.Observation); err != nil {
			return after, 0, errors.New("decode accepted report usage observation")
		}
		prepared = append(prepared, value.record)
	}
	if len(prepared) > 0 {
		if err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error { return s.RefreshReportUsage(ctx, prepared) }); err != nil {
			return after, 0, err
		}
		after = page[len(page)-1].key
	}
	return after, len(page), nil
}
