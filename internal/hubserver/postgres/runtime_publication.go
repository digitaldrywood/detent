package postgres

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
)

func (s *session) RuntimePublished(ctx context.Context, id string) (bool, error) {
	value, err := s.queries.RuntimePublished(ctx, pgquery.RuntimePublishedParams{OrganizationID: s.organization, RevisionID: id})
	return value.Bool, mapError(ctx, err)
}

func (s *session) CurrentRuntime(ctx context.Context) (billingstore.RuntimeRevision, error) {
	if !s.readOnly {
		if _, err := s.queries.LockRuntimeHead(ctx, s.organization); err != nil {
			return billingstore.RuntimeRevision{}, mapError(ctx, err)
		}
	}
	row, err := s.queries.CurrentRuntime(ctx, s.organization)
	return billingstore.RuntimeRevision{ID: row.ID, Published: row.LocalPublished, Reference: billingstore.AuditReference{BindingID: row.BindingID, CustomerOwned: row.CustomerOwned, Key: row.ObjectKey, SHA256: row.Sha256, CompressedBytes: row.CompressedBytes, ContentBytes: row.ContentBytes, Codec: row.Codec, FormatVersion: int(row.FormatVersion), Kind: row.Kind, Retention: row.Retention}}, mapError(ctx, err)
}

func (s *session) StageRuntime(ctx context.Context, revision billingstore.RuntimeRevision) error {
	head, err := s.queries.LockRuntimeHead(ctx, s.organization)
	if err != nil && !errors.Is(mapError(ctx, err), corestore.ErrNotFound) {
		return mapError(ctx, err)
	}
	active := head.ActiveID
	if err != nil && revision.Published {
		active = new(revision.ID)
	} else if err == nil {
		published, err := s.RuntimePublished(ctx, head.PreparedID)
		if err != nil {
			return err
		}
		if published {
			active = new(head.PreparedID)
		}
	}
	ref := revision.Reference
	if err := s.queries.InsertRuntimeRevision(ctx, pgquery.InsertRuntimeRevisionParams{OrganizationID: s.organization, ID: revision.ID, LocalPublished: revision.Published, BindingID: ref.BindingID, CustomerOwned: ref.CustomerOwned, ObjectKey: ref.Key, Sha256: ref.SHA256, CompressedBytes: ref.CompressedBytes, ContentBytes: ref.ContentBytes, Codec: ref.Codec, FormatVersion: int32(ref.FormatVersion), Kind: ref.Kind, Retention: ref.Retention}); err != nil {
		return mapError(ctx, err)
	}
	return mapError(ctx, s.queries.SetRuntimeHead(ctx, pgquery.SetRuntimeHeadParams{OrganizationID: s.organization, ActiveID: active, PreparedID: revision.ID}))
}

func (s *session) ApplyPublishedRuntimeCatalog(ctx context.Context, revision billingstore.RuntimeRevision, plans []billingstore.Plan, prices []billingstore.Price, audits []billingstore.PlanAudit) error {
	if len(plans) > 100 || len(prices) > 20 || len(audits) != len(plans)+1 {
		return errors.New("invalid runtime catalog bounds")
	}
	if err := s.StageRuntime(ctx, revision); err != nil {
		return err
	}
	published, err := s.RuntimePublished(ctx, revision.ID)
	if err != nil {
		return err
	}
	if !published {
		return corestore.ErrRevisionConflict
	}
	upsert := pgquery.UpsertRuntimePlansParams{OrganizationID: s.organization, RevisionID: revision.ID}
	allowances := pgquery.InsertRuntimeAllowancesParams{OrganizationID: s.organization}
	features := pgquery.InsertRuntimeFeaturesParams{OrganizationID: s.organization}
	for _, plan := range plans {
		if err := s.checkOrganization(plan.OrganizationID); err != nil {
			return err
		}
		upsert.Ids = append(upsert.Ids, plan.ID)
		upsert.Versions = append(upsert.Versions, plan.Version)
		upsert.Names = append(upsert.Names, plan.Name)
		upsert.Priced = append(upsert.Priced, plan.MonthlyUSDCents != nil)
		var price int64
		if plan.MonthlyUSDCents != nil {
			price = *plan.MonthlyUSDCents
		}
		upsert.Prices = append(upsert.Prices, price)
		for _, metric := range slices.Sorted(maps.Keys(plan.Allowances)) {
			allowances.Ids = append(allowances.Ids, plan.ID)
			allowances.Versions = append(allowances.Versions, plan.Version)
			allowances.Metrics = append(allowances.Metrics, metric)
			allowances.Amounts = append(allowances.Amounts, plan.Allowances[metric])
		}
		for _, feature := range plan.Features {
			features.Ids = append(features.Ids, plan.ID)
			features.Versions = append(features.Versions, plan.Version)
			features.Features = append(features.Features, feature)
		}
	}
	rows, err := s.queries.RuntimePlanCatalog(ctx, pgquery.RuntimePlanCatalogParams{OrganizationID: s.organization, Ids: upsert.Ids, Versions: upsert.Versions})
	if err != nil {
		return mapError(ctx, err)
	}
	existing := make(map[billingstore.PlanReference]*billingstore.Plan, len(plans))
	for _, row := range rows {
		ref := billingstore.PlanReference{ID: row.ID, Version: row.Version}
		plan := existing[ref]
		if plan == nil {
			plan = &billingstore.Plan{PlanReference: ref, Name: row.Name, Allowances: make(map[string]int64)}
			if row.MonthlyUsdCents.Valid {
				plan.MonthlyUSDCents = new(row.MonthlyUsdCents.Int64)
			}
			existing[ref] = plan
		}
		if row.Metric != "" {
			plan.Allowances[row.Metric] = row.Amount
		}
		if row.Feature != "" {
			plan.Features = append(plan.Features, row.Feature)
		}
	}
	selected := []billingstore.PlanAudit{audits[len(audits)-1]}
	for i, plan := range plans {
		old := existing[plan.PlanReference]
		if old == nil {
			selected = append(selected, audits[i])
			continue
		}
		slices.Sort(old.Features)
		if old.Name != plan.Name || !reflect.DeepEqual(old.MonthlyUSDCents, plan.MonthlyUSDCents) || !slices.Equal(old.Features, plan.Features) || !maps.Equal(old.Allowances, plan.Allowances) {
			selected = append(selected, billingstore.PlanAudit{OrganizationID: s.organization, CommandID: fmt.Sprintf("configuration_%s_%s_%d", revision.ID, plan.ID, plan.Version), RequestHash: revision.Reference.SHA256, ActorID: "configuration", Action: "update_plan", RecordedAt: audits[len(audits)-1].RecordedAt, Reference: revision.Reference})
		}
	}
	if err := s.queries.UpsertRuntimePlans(ctx, upsert); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.DeleteRuntimeAllowances(ctx, pgquery.DeleteRuntimeAllowancesParams{OrganizationID: s.organization, Ids: upsert.Ids, Versions: upsert.Versions}); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.DeleteRuntimeFeatures(ctx, pgquery.DeleteRuntimeFeaturesParams{OrganizationID: s.organization, Ids: upsert.Ids, Versions: upsert.Versions}); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.InsertRuntimeAllowances(ctx, allowances); err != nil {
		return mapError(ctx, err)
	}
	if err := s.queries.InsertRuntimeFeatures(ctx, features); err != nil {
		return mapError(ctx, err)
	}
	priceRows := pgquery.InsertRuntimePricesParams{OrganizationID: s.organization}
	for _, price := range prices {
		priceRows.Prices = append(priceRows.Prices, price.PriceID)
		priceRows.Ids = append(priceRows.Ids, price.Plan.ID)
		priceRows.Versions = append(priceRows.Versions, price.Plan.Version)
	}
	stored, err := s.queries.InsertRuntimePrices(ctx, priceRows)
	if err != nil {
		return mapError(ctx, err)
	}
	if len(stored) != len(prices) {
		return errors.New("hosted Stripe price mappings are immutable; configure a new price")
	}
	return s.insertRuntimeAudits(ctx, selected)
}

func (s *session) insertRuntimeAudits(ctx context.Context, audits []billingstore.PlanAudit) error {
	rows := pgquery.InsertRuntimeAuditsParams{OrganizationID: s.organization}
	for _, audit := range audits {
		if err := s.checkOrganization(audit.OrganizationID); err != nil {
			return err
		}
		rows.Commands = append(rows.Commands, audit.CommandID)
		rows.Hashes = append(rows.Hashes, audit.RequestHash)
		rows.Actors = append(rows.Actors, audit.ActorID)
		rows.Actions = append(rows.Actions, audit.Action)
		rows.Times = append(rows.Times, audit.RecordedAt)
		ref := audit.Reference
		rows.Bindings = append(rows.Bindings, ref.BindingID)
		rows.CustomerOwned = append(rows.CustomerOwned, ref.CustomerOwned)
		rows.Keys = append(rows.Keys, ref.Key)
		rows.Digests = append(rows.Digests, ref.SHA256)
		rows.Compressed = append(rows.Compressed, ref.CompressedBytes)
		rows.Content = append(rows.Content, ref.ContentBytes)
		rows.Codecs = append(rows.Codecs, ref.Codec)
		rows.Formats = append(rows.Formats, int32(ref.FormatVersion))
		rows.Kinds = append(rows.Kinds, ref.Kind)
		rows.Retentions = append(rows.Retentions, ref.Retention)
	}
	return mapError(ctx, s.queries.InsertRuntimeAudits(ctx, rows))
}

func (s *session) RuntimePrepared(ctx context.Context) (string, error) {
	head, err := s.queries.LockRuntimeHead(ctx, s.organization)
	return head.PreparedID, mapError(ctx, err)
}
