package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
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
	rows, err := s.db.Query(ctx, `WITH usage AS (
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,coalesce(u.currency,c.currency) AS currency FROM report_usage_current u JOIN cost_sources c ON c.organization_id=u.organization_id AND c.source_key=u.source_key WHERE u.organization_id=$1 AND u.attempt_id=ANY($2::text[]) AND (u.usage_kind IS DISTINCT FROM 'cumulative' OR NOT EXISTS(SELECT 1 FROM attempt_usage active WHERE active.organization_id=u.organization_id AND active.attempt_id=u.attempt_id AND active.provider=u.provider AND active.model=u.model))
 UNION ALL
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=ANY($2::text[]) AND NOT EXISTS(SELECT 1 FROM report_usage_current unknown WHERE unknown.organization_id=u.organization_id AND unknown.attempt_id=u.attempt_id AND unknown.provider=u.provider AND unknown.model=u.model AND unknown.usage_kind IS NULL)
 ) SELECT i.project_id,i.native_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,coalesce(r.runner_id,''),coalesce(r.display_name,'') FROM usage u
 JOIN native_attempts a ON a.organization_id=u.organization_id AND a.id=u.attempt_id JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=u.project_id
 LEFT JOIN runner_lease_assignments la ON la.organization_id=a.organization_id AND la.lease_id=a.lease_id LEFT JOIN runner_identities r ON r.organization_id=la.organization_id AND r.runner_id=la.runner_id
 WHERE u.period>=$3 AND u.period<$4 AND upper(u.currency)='USD' ORDER BY u.period,u.attempt_id,u.provider,u.model LIMIT $5`, s.organization, ids, query.From.UTC(), query.To.UTC(), query.Limit+1)
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
	if err := rows.Err(); err != nil {
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
		if record.OrganizationID != organization || record.ProjectID == "" || record.AttemptID == "" || record.WorkItemID == "" || record.Period.IsZero() || record.UpdatedAt.IsZero() || record.Provider == "" || record.Model == "" || record.Input < 0 || record.CachedInput < 0 || record.Output < 0 || record.Cost < 0 || math.IsInf(record.Cost, 0) || math.IsNaN(record.Cost) || len(record.Currency) != 3 || strings.Trim(strings.ToUpper(record.Currency), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
			return errors.New("invalid typed report usage fact")
		}
	}
	return r.internalWrite(ctx, organization, func(s *session) error {
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
	b := newReportBatch(s.organization, "source_key,revision,payload_sha256,project_id,work_item_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete,currency,usage_kind", "text", "bigint", "text", "text", "text", "text", "timestamptz", "text", "text", "bigint", "bigint", "bigint", "double precision", "boolean", "text", "text")
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
		var kind *string
		if o.UsageKind == "cumulative" || o.UsageKind == "incremental" {
			kind = new(o.UsageKind)
		}
		b.add(key, o.Revision, record.Reference.SHA256, record.ProjectID, o.WorkItemID, o.AttemptID, period, o.Provider, o.Model, input, cached, output, cost, known, o.Currency, kind)
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN cost_sources c ON c.organization_id=$1 AND c.source_key=source.source_key AND c.resource_id=source.attempt_id AND c.bucket='runner_api' WHERE c.revision>source.revision OR (c.revision=source.revision AND c.payload_hash=source.payload_sha256)`, corestore.ErrRevisionConflict); err != nil {
		return err
	}
	if err := s.verifyReportBatch(ctx, b, `SELECT count(*) FROM source JOIN cost_sources c ON c.organization_id=$1 AND c.source_key=source.source_key WHERE c.revision>source.revision OR EXISTS(SELECT 1 FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id WHERE a.organization_id=$1 AND a.id=source.attempt_id AND i.project_id=source.project_id AND i.native_id=source.work_item_id)`, corestore.ErrTenantMismatch); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, b.source()+`INSERT INTO report_usage_current(organization_id,source_key,revision,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,complete,currency,usage_kind)
 SELECT $1,s.source_key,s.revision,s.project_id,s.attempt_id,s.period,s.provider,s.model,s.input,s.cached_input,s.output,s.cost_estimate,s.complete,s.currency,s.usage_kind FROM source s JOIN cost_sources c ON c.organization_id=$1 AND c.source_key=s.source_key AND c.revision=s.revision ORDER BY s.source_key
 ON CONFLICT(organization_id,source_key) DO UPDATE SET revision=EXCLUDED.revision,input=EXCLUDED.input,cached_input=EXCLUDED.cached_input,output=EXCLUDED.output,cost_estimate=EXCLUDED.cost_estimate,complete=EXCLUDED.complete,currency=EXCLUDED.currency,usage_kind=EXCLUDED.usage_kind WHERE report_usage_current.revision<=EXCLUDED.revision`, b.args...)
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
	rows, err := s.db.Query(ctx, `WITH sources AS MATERIALIZED(SELECT c.source_key,c.resource_id,c.revision FROM cost_sources c WHERE c.organization_id=$1 AND c.bucket='runner_api' AND c.resource_id=ANY($2::text[]) ORDER BY c.resource_id,c.source_key LIMIT 100001)
 SELECT c.resource_id,u.complete AND u.revision=c.revision AND a.usage_pending_sequence=0 AND (u.usage_kind IS NOT NULL OR NOT EXISTS(SELECT 1 FROM attempt_usage active WHERE active.organization_id=u.organization_id AND active.attempt_id=u.attempt_id AND active.provider=u.provider AND active.model=u.model AND active.usage_sequence>0)) FROM sources c CROSS JOIN LATERAL (SELECT a.usage_pending_sequence FROM native_attempts a WHERE a.organization_id=$1 AND a.id=c.resource_id LIMIT 1) a LEFT JOIN report_usage_current u ON u.organization_id=$1 AND u.source_key=c.source_key`, s.organization, ids)
	if err != nil {
		return false, mapError(ctx, err)
	}
	seen := map[string]bool{}
	complete := true
	count := 0
	for rows.Next() {
		var id string
		var known pgtype.Bool
		if err := rows.Scan(&id, &known); err != nil {
			rows.Close()
			return false, mapError(ctx, err)
		}
		seen[id] = true
		complete = complete && known.Valid && known.Bool
		count++
	}
	if err := rows.Err(); err != nil {
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
	rows, err = s.db.Query(ctx, `WITH facts AS MATERIALIZED(SELECT u.attempt_id,u.usage_origin FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=ANY($2::text[]) ORDER BY u.attempt_id,u.period,u.provider,u.model LIMIT 100001), counted AS (SELECT f.attempt_id,count(*) AS actual,count(*) FILTER(WHERE f.usage_origin='import') AS historical FROM facts f GROUP BY f.attempt_id)
 SELECT f.attempt_id,f.actual,f.historical,coalesce(c.row_count,0),a.usage_pending_sequence FROM counted f CROSS JOIN LATERAL (SELECT a.usage_pending_sequence FROM native_attempts a WHERE a.organization_id=$1 AND a.id=f.attempt_id LIMIT 1) a LEFT JOIN report_usage_import_coverage c ON c.organization_id=$1 AND c.attempt_id=f.attempt_id`, s.organization, missing)
	if err != nil {
		return false, mapError(ctx, err)
	}
	imported := map[string]bool{}
	count = 0
	for rows.Next() {
		var id string
		var actual, historical, expected, pending int
		if err := rows.Scan(&id, &actual, &historical, &expected, &pending); err != nil {
			rows.Close()
			return false, mapError(ctx, err)
		}
		count += actual
		imported[id] = historical == expected && (expected > 0 || actual > 0 && historical == 0) && pending == 0
	}
	if err := rows.Err(); err != nil {
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
	return r.internalWrite(ctx, organization, func(s *session) error {
		return s.verifyReportUsageBatch(ctx, sha, records)
	})
}

func (r *Repository) RefreshReportUsagePage(ctx context.Context, organization tracker.OrganizationID, objects attachment.DetailStore, after string, limit int) (string, int, error) {
	if organization == "" || objects == nil || limit < 1 || limit > 100 {
		return after, 0, errors.New("invalid report usage refresh page")
	}
	if count, err := r.refreshHistoricalReportUsage(ctx, organization, limit, ""); err != nil || count > 0 {
		return after, count, err
	}
	type row struct {
		key           string
		attempt, item string
		reportable    bool
		record        billingstore.CostObservation
	}
	page := []row{}
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		rows, err := s.db.Query(ctx, `WITH candidates AS MATERIALIZED (
 SELECT c.source_key,c.project_id,c.resource_id,c.revision,c.payload_hash FROM cost_sources c WHERE c.organization_id=$1 AND c.bucket='runner_api' AND c.source_key>$2 ORDER BY c.source_key LIMIT $3
)
SELECT c.source_key,c.project_id,c.resource_id,coalesce(i.native_id,''),a.id IS NOT NULL AND i.id IS NOT NULL,c.revision,c.payload_hash,r.received_at,r.binding_id,r.customer_owned,r.object_key,r.sha256,r.compressed_bytes,r.content_bytes,r.codec,r.format_version,r.kind,r.retention
FROM candidates c LEFT JOIN native_attempts a ON a.organization_id=$1 AND a.id=c.resource_id
LEFT JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=c.project_id
LEFT JOIN cost_observation_revisions r ON r.organization_id=$1 AND r.source_key=c.source_key AND r.revision=c.revision ORDER BY c.source_key`, organization, after, limit)
		if err != nil {
			return mapError(ctx, err)
		}
		for rows.Next() {
			var value row
			var received, expectedHash string
			value.record.OrganizationID = organization
			ref := &value.record.Reference
			if err := rows.Scan(&value.key, &value.record.ProjectID, &value.attempt, &value.item, &value.reportable, &value.record.Observation.Revision, &expectedHash, &received, &ref.BindingID, &ref.CustomerOwned, &ref.Key, &ref.SHA256, &ref.CompressedBytes, &ref.ContentBytes, &ref.Codec, &ref.FormatVersion, &ref.Kind, &ref.Retention); err != nil {
				rows.Close()
				return mapError(ctx, err)
			}
			if expectedHash != ref.SHA256 {
				rows.Close()
				return errors.New("report usage archive identity mismatch")
			}
			value.record.ReceivedAt, err = time.Parse(time.RFC3339Nano, received)
			if err != nil {
				rows.Close()
				return err
			}
			page = append(page, value)
		}
		return mapError(ctx, rows.Err())
	})
	if err != nil {
		return after, 0, err
	}
	prepared := make([]billingstore.CostObservation, 0, len(page))
	for _, value := range page {
		if !value.reportable {
			continue
		}
		expectedRevision := value.record.Observation.Revision
		raw, err := objects.ReadDetail(ctx, string(organization), value.record.Reference.ArchiveReference())
		if err != nil {
			return after, 0, err
		}
		if err := json.Unmarshal(raw, &value.record.Observation); err != nil {
			return after, 0, errors.New("decode accepted report usage observation")
		}
		o := value.record.Observation
		key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
		if key != value.key || o.Revision != expectedRevision || o.Bucket != "runner_api" || o.ResourceID != value.attempt || o.AttemptID != value.attempt || o.WorkItemID != value.item {
			return after, 0, errors.New("report usage observation identity mismatch")
		}
		prepared = append(prepared, value.record)
	}
	if len(prepared) > 0 {
		if err := r.internalWrite(ctx, organization, func(s *session) error { return s.RefreshReportUsage(ctx, prepared) }); err != nil {
			return after, 0, err
		}
	}
	if len(page) > 0 {
		after = page[len(page)-1].key
	}
	return after, len(page), nil
}

func (s *session) ReadAnalyticsUsagePage(ctx context.Context, query reportstore.AnalyticsUsageQuery) (reportstore.AnalyticsUsagePage, error) {
	out := reportstore.AnalyticsUsagePage{Items: []reportstore.UsageFact{}, Partial: !query.After.Period.IsZero()}
	after := query.After
	if query.ProjectID == "" || !query.From.Before(query.To) || query.Limit < 1 || query.Limit > reportstore.AnalyticsAttemptLimit || after.Source < 0 || after.Source > 1 || after.Source == 0 && after.Origin != "" || after.Source == 1 && after.Origin != "" && after.Origin != "import" && after.Origin != "current" || after.Period.IsZero() && (after.Provider != "" || after.Model != "" || after.AttemptID != "" || after.Source != 0 || after.SourceKey != "" || after.Origin != "") || !after.Period.IsZero() && (after.AttemptID == "" || after.Source == 0 && after.SourceKey == "" || after.Source == 1 && after.SourceKey != "" && (len(after.SourceKey) != 3 || strings.Trim(strings.ToUpper(after.SourceKey), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "")) {
		return out, corestore.ErrInvalidPage
	}
	var pending bool
	if err := s.db.QueryRow(ctx, `WITH pending AS MATERIALIZED(SELECT a.issue_id FROM native_attempts a WHERE a.organization_id=$1 AND a.project_id=$2 AND a.usage_pending_sequence>0 ORDER BY a.id LIMIT 1001)
 SELECT (SELECT count(*) FROM pending)>1000 OR EXISTS(SELECT 1 FROM pending a JOIN issues i ON i.organization_id=$1 AND i.id=a.issue_id WHERE $3='' OR coalesce(i.actor_principal_id,'')=$3)`, s.organization, query.ProjectID, query.Principal).Scan(&pending); err != nil {
		return out, mapError(ctx, err)
	}
	out.Partial = out.Partial || pending
	args := []any{s.organization, query.ProjectID, query.From.UTC(), query.To.UTC(), query.Limit + 1}
	currentSeek, historySeek := "", ""
	if !after.Period.IsZero() {
		if after.Source == 0 {
			currentSeek = " AND (u.period,u.provider,u.model,u.attempt_id,u.source_key)>($6,$7,$8,$9,$10)"
			historySeek = " AND (u.period,u.provider,u.model,u.attempt_id)>=($6,$7,$8,$9)"
		} else {
			currentSeek = " AND (u.period,u.provider,u.model,u.attempt_id)>($6,$7,$8,$9)"
			historySeek = " AND (u.period,u.provider,u.model,u.attempt_id)>($6,$7,$8,$9)"
			if after.SourceKey != "" {
				historySeek = " AND (u.period,u.provider,u.model,u.attempt_id,u.currency,u.usage_origin)>($6,$7,$8,$9,$10,$11)"
			}
		}
		args = append(args, after.Period.UTC(), after.Provider, after.Model, after.AttemptID)
		if after.Source == 0 || after.SourceKey != "" {
			args = append(args, after.SourceKey)
		}
		if after.Source == 1 && after.SourceKey != "" {
			origin := after.Origin
			if origin == "" {
				origin = "import"
			}
			args = append(args, origin)
		}
	}
	principalArg := len(args) + 1
	args = append(args, query.Principal)
	statement := `WITH current_candidates AS MATERIALIZED (
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,0::smallint AS source,u.source_key,''::text AS usage_origin
 FROM report_usage_current u WHERE u.organization_id=$1 AND u.project_id=$2 AND u.period>=$3 AND u.period<$4` + currentSeek + `
 ORDER BY u.period,u.provider,u.model,u.attempt_id,u.source_key LIMIT $5
), history_candidates AS MATERIALIZED (
 SELECT u.organization_id,u.project_id,u.attempt_id,u.period,u.provider,u.model,1::smallint AS source,u.currency AS source_key,u.usage_origin
 FROM attempt_usage u WHERE u.organization_id=$1 AND u.project_id=$2 AND u.period>=$3 AND u.period<$4` + historySeek + `
 ORDER BY u.period,u.provider,u.model,u.attempt_id,u.currency,u.usage_origin LIMIT $5
), examined AS MATERIALIZED (
 SELECT * FROM current_candidates UNION ALL SELECT * FROM history_candidates
 ORDER BY period,provider,model,attempt_id,source,source_key,usage_origin LIMIT $5
), attempt_metadata AS MATERIALIZED (
 SELECT requested.attempt_id,a.organization_id,a.project_id,a.issue_id,a.lease_id,a.usage_pending_sequence
 FROM (SELECT DISTINCT attempt_id FROM examined) requested
 LEFT JOIN LATERAL (
  SELECT a.organization_id,a.project_id,a.issue_id,a.lease_id,a.usage_pending_sequence FROM native_attempts a WHERE a.organization_id=$1 AND a.id=requested.attempt_id LIMIT 1
 ) a ON true
), issue_metadata AS MATERIALIZED (
 SELECT requested.project_id,requested.issue_id,i.id,i.native_id,i.actor_principal_id
 FROM (SELECT DISTINCT project_id,issue_id FROM attempt_metadata WHERE issue_id IS NOT NULL) requested
 LEFT JOIN LATERAL (
  SELECT i.id,i.native_id,i.actor_principal_id FROM issues i WHERE i.organization_id=$1 AND i.project_id=requested.project_id AND i.id=requested.issue_id LIMIT 1
 ) i ON true
)
SELECT u.project_id,coalesce(i.native_id,''),u.attempt_id,u.period,u.provider,u.model,
 coalesce(current_metric.input,history_metric.input),coalesce(current_metric.cached_input,history_metric.cached_input),coalesce(current_metric.output,history_metric.output),coalesce(current_metric.cost_estimate,history_metric.cost_estimate),coalesce(current_metric.currency,history_metric.currency),
 coalesce(r.runner_id,''),coalesce(r.display_name,''),u.source,u.source_key,coalesce(current_metric.complete,history_metric.complete) AND coalesce(current_coverage.complete,true) AND coalesce(a.usage_pending_sequence,0)=0,
 i.id IS NOT NULL AND ($` + strconv.Itoa(principalArg) + `='' OR coalesce(i.actor_principal_id,'')=$` + strconv.Itoa(principalArg) + `) AND upper(coalesce(current_metric.currency,history_metric.currency))='USD' AND ((u.source=0 AND (current_metric.usage_kind IS DISTINCT FROM 'cumulative' OR hourly_coverage.present IS NULL)) OR (u.source=1 AND (current_coverage.present IS NULL OR current_coverage.usage_kind IS NOT NULL))) AS eligible,
 i.id IS NOT NULL AND ($` + strconv.Itoa(principalArg) + `='' OR coalesce(i.actor_principal_id,'')=$` + strconv.Itoa(principalArg) + `) AND (coalesce(a.usage_pending_sequence,0)>0 OR u.source=1 AND current_coverage.present IS NOT NULL AND current_coverage.usage_kind IS NULL) AS unavailable,u.usage_origin
FROM examined u
LEFT JOIN LATERAL (
 SELECT metric_current.input,metric_current.cached_input,metric_current.output,metric_current.cost_estimate,coalesce(metric_current.currency,source.currency) AS currency,metric_current.complete,metric_current.usage_kind
 FROM report_usage_current metric_current JOIN cost_sources source ON source.organization_id=metric_current.organization_id AND source.source_key=metric_current.source_key WHERE u.source=0 AND metric_current.organization_id=u.organization_id AND metric_current.source_key=u.source_key LIMIT 1
) current_metric ON true
LEFT JOIN LATERAL (
 SELECT metric_history.input,metric_history.cached_input,metric_history.output,metric_history.cost_estimate,metric_history.currency,true AS complete
 FROM attempt_usage metric_history WHERE u.source=1 AND metric_history.organization_id=u.organization_id AND metric_history.attempt_id=u.attempt_id AND metric_history.period=u.period AND metric_history.provider=u.provider AND metric_history.model=u.model AND metric_history.currency=u.source_key AND metric_history.usage_origin=u.usage_origin LIMIT 1
) history_metric ON true
LEFT JOIN attempt_metadata a ON a.attempt_id=u.attempt_id
LEFT JOIN issue_metadata i ON i.project_id=a.project_id AND i.issue_id=a.issue_id AND a.project_id=u.project_id
LEFT JOIN LATERAL (
 SELECT la.organization_id,la.runner_id FROM runner_lease_assignments la WHERE la.organization_id=a.organization_id AND la.lease_id=a.lease_id LIMIT 1
) la ON true
LEFT JOIN LATERAL (
 SELECT r.runner_id,r.display_name FROM runner_identities r WHERE r.organization_id=la.organization_id AND r.runner_id=la.runner_id LIMIT 1
) r ON true
LEFT JOIN LATERAL (
 SELECT coverage_current.period AS present,coverage_current.usage_kind,coverage_current.complete FROM report_usage_current coverage_current WHERE coverage_current.organization_id=u.organization_id AND coverage_current.attempt_id=u.attempt_id AND coverage_current.provider=u.provider AND coverage_current.model=u.model ORDER BY coverage_current.usage_kind NULLS FIRST,coverage_current.source_key LIMIT 1
 ) current_coverage ON true
LEFT JOIN LATERAL (
 SELECT active.usage_sequence AS present FROM attempt_usage active WHERE u.source=0 AND current_metric.usage_kind='cumulative' AND active.organization_id=u.organization_id AND active.attempt_id=u.attempt_id AND active.provider=u.provider AND active.model=u.model ORDER BY active.usage_sequence DESC,active.period DESC,active.updated_at DESC,active.currency COLLATE "C" DESC LIMIT 1
) hourly_coverage ON u.source=0 AND current_metric.usage_kind='cumulative'
ORDER BY u.period,u.provider,u.model,u.attempt_id,u.source,u.source_key,u.usage_origin`
	rows, err := s.db.Query(ctx, statement, args...)
	if err != nil {
		return out, mapError(ctx, err)
	}
	defer rows.Close()
	var last reportstore.AnalyticsUsageCursor
	for rows.Next() {
		var row reportstore.UsageFact
		var source int16
		var sourceKey, origin string
		var complete, eligible, unavailable bool
		if err := rows.Scan(&row.ProjectID, &row.WorkItemID, &row.AttemptID, &row.Period, &row.Provider, &row.Model, &row.Input, &row.CachedInput, &row.Output, &row.Cost, &row.Currency, &row.RunnerID, &row.RunnerName, &source, &sourceKey, &complete, &eligible, &unavailable, &origin); err != nil {
			return out, mapError(ctx, err)
		}
		out.Examined++
		if out.Examined > query.Limit {
			out.Next = last
			out.Partial = true
			break
		}
		last = reportstore.AnalyticsUsageCursor{Period: row.Period, Provider: row.Provider, Model: row.Model, AttemptID: row.AttemptID, Source: source, SourceKey: sourceKey, Origin: origin}
		out.Partial = out.Partial || unavailable
		if eligible {
			out.Items = append(out.Items, row)
			out.Partial = out.Partial || !complete
		}
	}
	return out, mapError(ctx, rows.Err())
}

func (s *session) RecordAttemptUsage(ctx context.Context, report reportstore.AttemptUsage) error {
	if err := s.checkOrganization(report.OrganizationID); err != nil {
		return err
	}
	if s.readOnly || report.ProjectID == "" || report.WorkItemID == "" || report.AttemptID == "" || report.LeaseID == "" || report.FencingToken <= 0 || report.Sequence <= 0 || report.ReceivedAt.IsZero() || len(report.Currency) != 3 || strings.Trim(report.Currency, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || len(report.Totals) == 0 && report.ReplayEventID == "" || len(report.Totals) > 32 {
		return corestore.ErrInvalidPage
	}
	if len(report.Historical) > 32 {
		return corestore.ErrInvalidPage
	}
	if len(report.Historical) > 0 {
		if err := s.RefreshReportUsage(ctx, report.Historical); err != nil {
			return err
		}
		for _, record := range report.Historical {
			o := record.Observation
			if record.OrganizationID != report.OrganizationID || record.ProjectID != report.ProjectID || o.AttemptID != report.AttemptID || o.WorkItemID != string(report.WorkItemID) || o.Bucket != "runner_api" {
				return corestore.ErrTenantMismatch
			}
			if o.UsageKind != "cumulative" {
				continue
			}
			amount := o.EstimatedAmountMicros
			if amount == nil {
				amount = o.AmountMicros
			}
			if amount == nil {
				amount = o.ReportedAmountMicros
			}
			if amount == nil && !o.Voided {
				continue
			}
			input, cached, output, cost := o.Input, o.CachedInput, o.Output, float64(0)
			if amount != nil {
				cost = float64(*amount) / 1e6
			}
			if o.Voided {
				input, cached, output, cost = 0, 0, 0, 0
			}
			key := costIdentity(o.Provider, o.ProviderAccount, o.SourceID, o.From.UTC().Format(costPeriodLayout), o.To.UTC().Format(costPeriodLayout))
			query := `WITH authority AS MATERIALIZED (
 SELECT a.id FROM native_attempts a JOIN issues i ON i.organization_id=a.organization_id AND i.id=a.issue_id JOIN cost_sources c ON c.organization_id=a.organization_id AND c.resource_id=a.id
 WHERE a.organization_id=$1 AND a.id=$2 AND i.project_id=$3 AND i.native_id=$4 AND a.lease_id=$5 AND a.fencing_token=$6 AND a.sequence=$7 AND c.source_key=$8 AND c.revision=$9 AND c.payload_hash=$10
), written AS (
 INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at,usage_input_total,usage_cached_input_total,usage_output_total,usage_cost_total,usage_sequence,usage_origin)
 SELECT $1,$3,$2,date_trunc('hour',$11::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',$12,$13,$14,$15,$16,$17,$18,$11,$14,$15,$16,$17,0,'current'
 WHERE EXISTS(SELECT 1 FROM authority) AND NOT EXISTS(SELECT 1 FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=$2 AND u.provider=$12 AND u.model=$13) RETURNING attempt_id
)
SELECT EXISTS(SELECT 1 FROM authority),(SELECT count(*) FROM written)`
			check := func(row pgx.Row) error {
				var valid bool
				var count int64
				if err := row.Scan(&valid, &count); err != nil {
					return err
				}
				if !valid {
					return corestore.ErrRevisionConflict
				}
				return nil
			}
			args := []any{s.organization, report.AttemptID, report.ProjectID, report.WorkItemID, report.LeaseID, report.FencingToken, report.Sequence, key, o.Revision, record.Reference.SHA256, recordTime(record.ReceivedAt), o.Provider, o.Model, input, cached, output, cost, o.Currency}
			if queue, ok := s.db.(runEventQueue); ok {
				queue.batch.Queue(query, args...).QueryRow(check)
			} else if err := check(s.db.QueryRow(ctx, query, args...)); err != nil {
				return mapError(ctx, err)
			}
		}
	}
	var clock *time.Time
	if s.leaseClock != nil {
		clock = new(s.leaseClock().UTC())
	}
	seen := make(map[string]bool, len(report.Totals))
	for _, total := range report.Totals {
		key := total.Provider + "\n" + total.Model
		if total.Provider == "" || total.Model == "" || len(total.Provider) > 128 || len(total.Model) > 128 || seen[key] || total.Input < 0 || total.CachedInput < 0 || total.Output < 0 {
			return corestore.ErrInvalidPage
		}
		seen[key] = true
		values := []float64{total.InputPrice, total.CachedInputPrice, total.OutputPrice}
		if total.ReportedCost != nil {
			values = append(values, *total.ReportedCost)
		}
		for _, value := range values {
			if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
				return corestore.ErrInvalidPage
			}
		}
		query := `WITH owner AS MATERIALIZED (
 SELECT a.id,a.usage_pending_sequence FROM native_attempts a JOIN work_item_identities i ON i.organization_id=a.organization_id AND i.id=a.issue_id
 JOIN leases l ON l.organization_id=a.organization_id AND l.lease_id=a.lease_id AND l.fencing_token=a.fencing_token
 WHERE a.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND a.id=$4 AND a.lease_id=$5 AND a.fencing_token=$6
 AND (($20::text='' AND a.sequence=$7 AND EXISTS(SELECT 1 FROM issues live WHERE live.organization_id=a.organization_id AND live.id=a.issue_id) AND l.released_at IS NULL AND l.expires_at>coalesce($19::timestamptz,clock_timestamp())) OR ($20::text<>'' AND a.sequence>=$7 AND a.usage_pending_sequence>0 AND a.usage_pending_sequence=$7 AND a.usage_projected_sequence<$22 AND EXISTS(
 SELECT 1 FROM collaboration_events e JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id WHERE e.organization_id=$1 AND e.project_id=$2 AND e.work_item_id=$3 AND e.id=$20 AND e.sequence=$22 AND e.payload_sha256=$21 AND e.attempt_id=a.id AND e.run_id=a.run_id AND e.recorded_at=$8 AND f.run_sequence=$7
))) FOR UPDATE OF a
), previous_usage AS MATERIALIZED (
 SELECT u.* FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=(SELECT id FROM owner) AND u.provider=$10 AND u.model=$11 AND u.usage_input_total IS NOT NULL
 ORDER BY u.usage_sequence DESC,u.period DESC,u.updated_at DESC,u.currency COLLATE "C" DESC LIMIT 1
), previous_currency AS MATERIALIZED (
 SELECT u.* FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=(SELECT id FROM owner) AND u.provider=$10 AND u.model=$11 AND u.currency=$9 AND u.usage_input_total IS NOT NULL
 ORDER BY u.usage_sequence DESC,u.period DESC,u.updated_at DESC LIMIT 1
), baseline AS MATERIALIZED (
 SELECT NOT EXISTS(SELECT 1 FROM previous_usage WHERE usage_input_total IS NULL OR usage_cached_input_total IS NULL OR usage_output_total IS NULL)
 AND NOT EXISTS(SELECT 1 FROM previous_currency WHERE usage_cost_total IS NULL) AS known,
 coalesce((SELECT usage_input_total FROM previous_usage),0) AS input,
 coalesce((SELECT usage_cached_input_total FROM previous_usage),0) AS cached,
 coalesce((SELECT usage_output_total FROM previous_usage),0) AS output,
 coalesce((SELECT usage_cost_total FROM previous_currency),0) AS cost,
 EXISTS(SELECT 1 FROM attempt_usage unknown WHERE unknown.organization_id=$1 AND unknown.attempt_id=$4 AND unknown.provider=$10 AND unknown.model=$11 AND unknown.usage_input_total IS NULL) OR ($20::text='' AND EXISTS(SELECT 1 FROM owner WHERE usage_pending_sequence>0)) OR (NOT EXISTS(SELECT 1 FROM previous_usage) AND EXISTS(SELECT 1 FROM report_usage_current u WHERE u.organization_id=$1 AND u.attempt_id=$4 AND u.provider=$10 AND u.model=$11)) AS history_unknown
), token_delta AS MATERIALIZED (
 SELECT *,greatest($12::bigint-input,0) AS di,greatest($13::bigint-cached,0) AS dc,greatest($14::bigint-output,0) AS dout FROM baseline
), delta AS MATERIALIZED (
 SELECT *,CASE WHEN $15::double precision IS NOT NULL THEN greatest($15::double precision-cost,0)
 ELSE greatest(di-least(dc,di),0)::double precision/1000000*$16::double precision+least(dc,di)::double precision/1000000*$17::double precision+dout::double precision/1000000*$18::double precision END AS amount FROM token_delta
), written AS (
 INSERT INTO attempt_usage(organization_id,project_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at,usage_input_total,usage_cached_input_total,usage_output_total,usage_cost_total,usage_sequence,usage_origin)
 SELECT $1,$2,$4,date_trunc('hour',$8::timestamptz AT TIME ZONE 'UTC') AT TIME ZONE 'UTC',$10,$11,di,dc,dout,amount,$9,$8,input+di,cached+dc,output+dout,cost+amount,$7,'current' FROM delta
 WHERE known AND NOT history_unknown AND EXISTS(SELECT 1 FROM owner) AND (di<>0 OR dc<>0 OR dout<>0 OR amount<>0)
 ON CONFLICT(organization_id,attempt_id,period,provider,model,currency,usage_origin) DO UPDATE SET
 input=attempt_usage.input+excluded.input,cached_input=attempt_usage.cached_input+excluded.cached_input,output=attempt_usage.output+excluded.output,cost_estimate=attempt_usage.cost_estimate+excluded.cost_estimate,
 updated_at=excluded.updated_at,usage_input_total=excluded.usage_input_total,usage_cached_input_total=excluded.usage_cached_input_total,usage_output_total=excluded.usage_output_total,usage_cost_total=excluded.usage_cost_total,usage_sequence=greatest(attempt_usage.usage_sequence,excluded.usage_sequence)
 RETURNING attempt_id
), unavailable AS (
 UPDATE report_usage_current u SET complete=false WHERE u.organization_id=$1 AND u.source_key=(SELECT chosen.source_key FROM report_usage_current chosen WHERE chosen.organization_id=$1 AND chosen.attempt_id=$4 AND chosen.provider=$10 AND chosen.model=$11 ORDER BY chosen.usage_kind NULLS FIRST,chosen.source_key LIMIT 1) AND EXISTS(SELECT 1 FROM owner) AND (SELECT history_unknown FROM delta) RETURNING u.source_key
)
 , pending AS (
 UPDATE native_attempts a SET usage_pending_sequence=CASE WHEN $20::text='' THEN CASE WHEN a.usage_pending_sequence=0 THEN $7 ELSE a.usage_pending_sequence END ELSE a.usage_pending_sequence END,usage_projected_sequence=CASE WHEN $20::text='' AND a.usage_pending_sequence=0 THEN greatest($22::bigint-1,0) ELSE a.usage_projected_sequence END
 WHERE a.organization_id=$1 AND a.id=$4 AND EXISTS(SELECT 1 FROM owner) AND (SELECT history_unknown FROM delta) RETURNING a.id
)
SELECT EXISTS(SELECT 1 FROM owner),known,history_unknown,(di<>0 OR dc<>0 OR dout<>0 OR amount<>0),(SELECT count(*) FROM written) FROM delta`
		check := func(row pgx.Row) error {
			var owner, known, unknown, changed bool
			var count int64
			if err := row.Scan(&owner, &known, &unknown, &changed, &count); err != nil {
				return err
			}
			if !owner {
				return corestore.ErrTenantMismatch
			}
			if !known {
				return corestore.ErrInvalidPage
			}
			if unknown {
				if count != 0 {
					return corestore.ErrRevisionConflict
				}
				return nil
			}
			if changed && count != 1 || !changed && count != 0 {
				return corestore.ErrRevisionConflict
			}
			return nil
		}
		args := []any{s.organization, report.ProjectID, report.WorkItemID, report.AttemptID, report.LeaseID, report.FencingToken, report.Sequence, recordTime(report.ReceivedAt), report.Currency, total.Provider, total.Model, total.Input, total.CachedInput, total.Output, total.ReportedCost, total.InputPrice, total.CachedInputPrice, total.OutputPrice, clock, report.ReplayEventID, report.ReplayEventSHA256, report.EventSequence}
		if queue, ok := s.db.(runEventQueue); ok {
			queue.batch.Queue(query, args...).QueryRow(check)
		} else if err := check(s.db.QueryRow(ctx, query, args...)); err != nil {
			return mapError(ctx, err)
		}
	}
	return nil
}

func (r *Repository) refreshHistoricalReportUsage(ctx context.Context, organization tracker.OrganizationID, limit int, attempt string) (int, error) {
	count := 0
	err := r.transaction(ctx, organization, corestore.Background, false, func(s *session) error {
		filter := ""
		args := []any{organization, limit}
		if attempt != "" {
			filter = " AND attempt_id=$3"
			args = append(args, attempt)
		}
		rows, err := s.db.Query(ctx, `WITH selected AS MATERIALIZED (
 SELECT * FROM attempt_usage WHERE organization_id=$1 AND usage_origin='import' AND usage_input_total IS NULL`+filter+` ORDER BY attempt_id,provider,model,period,updated_at,currency LIMIT $2
) SELECT u.project_id,i.native_id,u.attempt_id,u.period,u.provider,u.model,u.input,u.cached_input,u.output,u.cost_estimate,u.currency,u.updated_at FROM selected u LEFT JOIN native_attempts a ON a.organization_id=u.organization_id AND a.id=u.attempt_id LEFT JOIN work_item_identities i ON i.organization_id=a.organization_id AND i.id=a.issue_id AND i.project_id=u.project_id ORDER BY u.attempt_id,u.provider,u.model,u.period,u.updated_at,u.currency`, args...)
		if err != nil {
			return mapError(ctx, err)
		}
		b := newReportBatch(organization, "project_id,work_item_id,attempt_id,period,provider,model,input,cached_input,output,cost_estimate,currency,updated_at,raw_currency", "text", "text", "text", "timestamptz", "text", "text", "bigint", "bigint", "bigint", "double precision", "text", "timestamptz", "text")
		for rows.Next() {
			var record reportstore.UsageRecord
			if err := rows.Scan(&record.ProjectID, &record.WorkItemID, &record.AttemptID, &record.Period, &record.Provider, &record.Model, &record.Input, &record.CachedInput, &record.Output, &record.Cost, &record.Currency, &record.UpdatedAt); err != nil {
				rows.Close()
				return mapError(ctx, err)
			}
			if len(record.Currency) != 3 || strings.Trim(strings.ToUpper(record.Currency), "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" {
				rows.Close()
				return corestore.ErrInvalidPage
			}
			b.add(record.ProjectID, record.WorkItemID, record.AttemptID, record.Period, record.Provider, record.Model, record.Input, record.CachedInput, record.Output, record.Cost, strings.ToUpper(record.Currency), record.UpdatedAt, record.Currency)
		}
		if err := rows.Err(); err != nil {
			return mapError(ctx, err)
		}
		rows.Close()
		if len(b.args) == 1 {
			return nil
		}
		if err := s.lockUsageReplay(ctx, b); err != nil {
			return err
		}
		return mapError(ctx, s.db.QueryRow(ctx, strings.TrimSuffix(b.source(), " ")+`, selected AS MATERIALIZED (
 SELECT s.* FROM source s JOIN attempt_usage u ON u.organization_id=$1 AND (u.attempt_id,u.period,u.provider,u.model,u.currency)=(s.attempt_id,s.period,s.provider,s.model,s.raw_currency) WHERE u.usage_origin='import' AND u.usage_input_total IS NULL
), pairs AS MATERIALIZED (
 SELECT DISTINCT attempt_id,provider,model FROM selected
), currencies AS MATERIALIZED (
 SELECT DISTINCT attempt_id,provider,model,currency FROM selected
), latest AS MATERIALIZED (
 SELECT p.*,u.period,u.currency,u.usage_origin,u.usage_input_total,u.usage_cached_input_total,u.usage_output_total FROM pairs p LEFT JOIN LATERAL (
 SELECT period,currency,usage_origin,usage_input_total,usage_cached_input_total,usage_output_total FROM attempt_usage WHERE organization_id=$1 AND attempt_id=p.attempt_id AND provider=p.provider AND model=p.model AND usage_input_total IS NOT NULL ORDER BY usage_sequence DESC,period DESC,updated_at DESC,currency COLLATE "C" DESC LIMIT 1
 )u ON true
), latest_currency AS MATERIALIZED (
 SELECT p.*,u.period,u.usage_origin,u.usage_cost_total FROM currencies p LEFT JOIN LATERAL (
 SELECT period,usage_origin,usage_cost_total FROM attempt_usage WHERE organization_id=$1 AND attempt_id=p.attempt_id AND provider=p.provider AND model=p.model AND currency=p.currency AND usage_input_total IS NOT NULL ORDER BY usage_sequence DESC,period DESC,updated_at DESC LIMIT 1
 )u ON true
), deltas AS MATERIALIZED (
 SELECT attempt_id,provider,model,sum(input)::bigint AS input,sum(cached_input)::bigint AS cached,sum(output)::bigint AS output FROM selected GROUP BY attempt_id,provider,model
), cost_deltas AS MATERIALIZED (
 SELECT attempt_id,provider,model,currency,sum(cost_estimate) AS cost FROM selected GROUP BY attempt_id,provider,model,currency
), totals AS MATERIALIZED (
 SELECT l.attempt_id,l.provider,l.model,coalesce(l.usage_input_total,0)+d.input AS input,coalesce(l.usage_cached_input_total,0)+d.cached AS cached,coalesce(l.usage_output_total,0)+d.output AS output FROM latest l JOIN deltas d USING(attempt_id,provider,model)
), cost_totals AS MATERIALIZED (
 SELECT c.attempt_id,c.provider,c.model,c.currency,coalesce(c.usage_cost_total,0)+d.cost AS cost FROM latest_currency c JOIN cost_deltas d USING(attempt_id,provider,model,currency)
), classified AS (
 UPDATE attempt_usage u SET currency=s.currency,usage_input_total=t.input,usage_cached_input_total=t.cached,usage_output_total=t.output,usage_cost_total=c.cost
 FROM selected s JOIN totals t USING(attempt_id,provider,model) JOIN cost_totals c USING(attempt_id,provider,model,currency)
 WHERE u.organization_id=$1 AND (u.attempt_id,u.period,u.provider,u.model,u.currency)=(s.attempt_id,s.period,s.provider,s.model,s.raw_currency) AND u.usage_origin='import' AND u.usage_input_total IS NULL RETURNING u.attempt_id
), targets AS MATERIALIZED (
 SELECT attempt_id,provider,model,period,currency,usage_origin FROM latest WHERE period IS NOT NULL UNION SELECT attempt_id,provider,model,period,currency,usage_origin FROM latest_currency WHERE period IS NOT NULL
), refreshed AS (
 UPDATE attempt_usage u SET usage_input_total=CASE WHEN (u.period,u.currency,u.usage_origin)=(l.period,l.currency,l.usage_origin) THEN t.input ELSE u.usage_input_total END,
 usage_cached_input_total=CASE WHEN (u.period,u.currency,u.usage_origin)=(l.period,l.currency,l.usage_origin) THEN t.cached ELSE u.usage_cached_input_total END,
 usage_output_total=CASE WHEN (u.period,u.currency,u.usage_origin)=(l.period,l.currency,l.usage_origin) THEN t.output ELSE u.usage_output_total END,
 usage_cost_total=CASE WHEN (u.period,u.usage_origin)=(c.period,c.usage_origin) THEN coalesce(ct.cost,u.usage_cost_total) ELSE u.usage_cost_total END
 FROM targets x JOIN latest l USING(attempt_id,provider,model) JOIN totals t USING(attempt_id,provider,model)
 LEFT JOIN latest_currency c ON (c.attempt_id,c.provider,c.model,c.currency)=(x.attempt_id,x.provider,x.model,x.currency)
 LEFT JOIN cost_totals ct ON (ct.attempt_id,ct.provider,ct.model,ct.currency)=(x.attempt_id,x.provider,x.model,x.currency)
 WHERE u.organization_id=$1 AND (u.attempt_id,u.period,u.provider,u.model,u.currency,u.usage_origin)=(x.attempt_id,x.period,x.provider,x.model,x.currency,x.usage_origin) RETURNING u.attempt_id
)
SELECT count(*) FROM classified`, b.args...).Scan(&count))
	})
	return count, err
}

func (s *session) ReplayAttemptUsage(ctx context.Context, replay reportstore.UsageReplay) (bool, error) {
	report := replay.Usage
	if report.OrganizationID != s.organization || report.ReplayEventID == "" || len(report.ReplayEventSHA256) != 64 || report.EventSequence <= 0 || replay.Projection.RunSequence != report.Sequence {
		return false, corestore.ErrInvalidPage
	}
	b := newReportBatch(s.organization, "project_id,work_item_id,attempt_id", "text", "text", "text")
	b.add(report.ProjectID, report.WorkItemID, report.AttemptID)
	if err := s.lockUsageReplay(ctx, b); err != nil {
		return false, err
	}
	var pending, projected int64
	var kind string
	var factSequence *int64
	err := s.db.QueryRow(ctx, `SELECT a.usage_pending_sequence,a.usage_projected_sequence,e.type,f.run_sequence FROM native_attempts a JOIN work_item_identities i ON i.organization_id=a.organization_id AND i.id=a.issue_id
 JOIN collaboration_events e ON e.organization_id=a.organization_id AND e.attempt_id=a.id LEFT JOIN report_event_facts f ON f.organization_id=e.organization_id AND f.event_id=e.id
 WHERE a.organization_id=$1 AND i.project_id=$2 AND i.native_id=$3 AND a.id=$4
 AND e.project_id=$2 AND e.work_item_id=$3 AND e.id=$8 AND e.payload_sha256=$9 AND e.recorded_at=$10 AND e.sequence=$11
 AND (e.type NOT IN ('run.started','run.checkpointed','run.observed','run.finished') OR (e.run_id=a.run_id AND a.lease_id=$5 AND a.fencing_token=$6 AND a.sequence>=$7))`, s.organization, report.ProjectID, report.WorkItemID, report.AttemptID, report.LeaseID, report.FencingToken, report.Sequence, report.ReplayEventID, report.ReplayEventSHA256, recordTime(report.ReceivedAt), report.EventSequence).Scan(&pending, &projected, &kind, &factSequence)
	if err != nil {
		return false, mapError(ctx, err)
	}
	if pending == 0 || report.EventSequence <= projected {
		return true, nil
	}
	var next int64
	if err := s.db.QueryRow(ctx, `SELECT sequence FROM collaboration_events WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3 AND attempt_id=$4 AND attempt_id<>'' AND sequence>$5 ORDER BY sequence LIMIT 1`, s.organization, report.ProjectID, report.WorkItemID, report.AttemptID, projected).Scan(&next); err != nil {
		return false, mapError(ctx, err)
	}
	if next != report.EventSequence {
		return false, nil
	}
	if kind != "run.started" && kind != "run.checkpointed" && kind != "run.observed" && kind != "run.finished" {
		result, err := s.db.Exec(ctx, `UPDATE native_attempts SET usage_projected_sequence=$3 WHERE organization_id=$1 AND id=$2 AND usage_pending_sequence=$4 AND usage_projected_sequence=$5`, s.organization, report.AttemptID, report.EventSequence, pending, projected)
		return err == nil && result.RowsAffected() == 1, mapError(ctx, err)
	}
	if report.Sequence <= 0 {
		return false, nil
	}
	if factSequence == nil {
		if err := s.putReportEventProjection(ctx, eventstore.Event{OrganizationID: s.organization, ProjectID: report.ProjectID, WorkItemID: report.WorkItemID, ID: report.ReplayEventID, AttemptID: report.AttemptID, Type: kind, Report: &replay.Projection}); err != nil {
			return false, err
		}
	} else if *factSequence != report.Sequence {
		return false, corestore.ErrRevisionConflict
	}
	if report.Sequence < pending {
		result, err := s.db.Exec(ctx, `UPDATE native_attempts SET usage_projected_sequence=$3 WHERE organization_id=$1 AND id=$2 AND usage_pending_sequence=$4 AND usage_projected_sequence=$5`, s.organization, report.AttemptID, report.EventSequence, pending, projected)
		return err == nil && result.RowsAffected() == 1, mapError(ctx, err)
	}
	if report.Sequence != pending || report.PricingUnavailable {
		return false, nil
	}
	if len(report.Totals) > 0 {
		if err := s.RecordAttemptUsage(ctx, report); err != nil {
			return false, err
		}
		providers, models := make([]string, len(report.Totals)), make([]string, len(report.Totals))
		for index, total := range report.Totals {
			providers[index], models[index] = total.Provider, total.Model
		}
		var ready bool
		if err := s.db.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM unnest($3::text[],$4::text[])t(provider,model) WHERE EXISTS(SELECT 1 FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=$2 AND u.provider=t.provider AND u.model=t.model AND u.usage_input_total IS NULL) OR NOT EXISTS(SELECT 1 FROM attempt_usage u WHERE u.organization_id=$1 AND u.attempt_id=$2 AND u.provider=t.provider AND u.model=t.model AND u.usage_input_total IS NOT NULL))`, s.organization, report.AttemptID, providers, models).Scan(&ready); err != nil {
			return false, mapError(ctx, err)
		}
		if !ready {
			return false, nil
		}
	}
	result, err := s.db.Exec(ctx, `UPDATE native_attempts SET usage_projected_sequence=$4,usage_pending_sequence=CASE WHEN sequence=$3 THEN 0 ELSE $3+1 END WHERE organization_id=$1 AND id=$2 AND usage_pending_sequence=$3 AND usage_projected_sequence=$5`, s.organization, report.AttemptID, report.Sequence, report.EventSequence, projected)
	if err != nil {
		return false, mapError(ctx, err)
	}
	return result.RowsAffected() == 1, nil
}

func (s *session) NextPendingUsageAttempt(ctx context.Context, afterProject tracker.ProjectID, afterAttempt string) (reportstore.PendingUsageAttempt, error) {
	var out reportstore.PendingUsageAttempt
	if afterProject == "" && afterAttempt != "" {
		return out, corestore.ErrInvalidPage
	}
	err := s.db.QueryRow(ctx, `WITH selected AS MATERIALIZED(SELECT a.project_id,a.id,a.issue_id,a.usage_projected_sequence FROM native_attempts a WHERE a.organization_id=$1 AND a.usage_pending_sequence>0 AND (a.project_id,a.id)>($2,$3) ORDER BY a.project_id,a.id LIMIT 1)
 SELECT n.project_id,i.native_id,n.id,n.usage_projected_sequence FROM selected n LEFT JOIN work_item_identities i ON i.organization_id=$1 AND i.project_id=n.project_id AND i.id=n.issue_id`, s.organization, afterProject, afterAttempt).Scan(&out.ProjectID, &out.WorkItemID, &out.AttemptID, &out.AfterEventSequence)
	return out, mapError(ctx, err)
}

func (r *Repository) RefreshAttemptReportUsage(ctx context.Context, organization tracker.OrganizationID, attempt string, limit int) (bool, error) {
	if organization == "" || attempt == "" || limit < 1 || limit > 100 {
		return false, corestore.ErrInvalidPage
	}
	if _, err := r.refreshHistoricalReportUsage(ctx, organization, limit, attempt); err != nil {
		return false, err
	}
	var unknown bool
	err := r.transaction(ctx, organization, corestore.Background, true, func(s *session) error {
		return mapError(ctx, s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM attempt_usage WHERE organization_id=$1 AND attempt_id=$2 AND usage_input_total IS NULL LIMIT 1)`, organization, attempt).Scan(&unknown))
	})
	return !unknown, err
}
