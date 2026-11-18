package reportreads

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgcore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type usageKey struct {
	attempt, provider, model, currency, origin, source string
	period                                             time.Time
}

type usageKeys struct {
	attempts, providers, models, currencies, origins, sources []string
	periods                                                   []time.Time
}

func (keys *usageKeys) add(k usageKey) {
	keys.attempts = append(keys.attempts, k.attempt)
	keys.periods = append(keys.periods, k.period)
	keys.providers = append(keys.providers, k.provider)
	keys.models = append(keys.models, k.model)
	keys.currencies = append(keys.currencies, k.currency)
	keys.origins = append(keys.origins, k.origin)
	keys.sources = append(keys.sources, k.source)
}

func (keys usageKeys) input(name string, current bool, args *[]any) string {
	count := len(keys.attempts)
	var pages, names []string
	for start := 0; start < max(1, count); start += CandidateRows {
		end := min(start+CandidateRows, count)
		at := len(*args) + 1
		var source string
		if current {
			source = fmt.Sprintf("unnest($%d::text[]) AS k(source_key)", at)
			*args = append(*args, keys.sources[start:end])
		} else {
			source = fmt.Sprintf("unnest($%d::text[],$%d::timestamptz[],$%d::text[],$%d::text[],$%d::text[],$%d::text[]) AS k(attempt_id,period,provider,model,currency,usage_origin)", at, at+1, at+2, at+3, at+4, at+5)
			*args = append(*args, keys.attempts[start:end], keys.periods[start:end], keys.providers[start:end], keys.models[start:end], keys.currencies[start:end], keys.origins[start:end])
		}
		*args = append(*args, max(1, end-start))
		page := name
		if count > CandidateRows {
			page = fmt.Sprintf("%s_%d", name, start/CandidateRows)
		}
		pages = append(pages, fmt.Sprintf("%s AS MATERIALIZED (SELECT k.* FROM %s LIMIT $%d)", page, source, len(*args)))
		names = append(names, "SELECT * FROM "+page)
	}
	if len(names) > 1 {
		pages = append(pages, name+" AS ("+strings.Join(names, " UNION ALL ")+")")
	}
	return strings.Join(pages, ",")
}

type usagePage struct {
	current  bool
	from, to time.Time
}

func (page usagePage) read(ctx context.Context, db pgquery.DBTX, org tracker.OrganizationID, ids []string, limit int) (usageKeys, error) {
	keys := usageKeys{}
	pending := make([]usageKey, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			pending = append(pending, usageKey{attempt: id})
			seen[id] = true
		}
	}
	for size := 1; len(pending) > 0; size = min(size*2, CandidateRows+1) {
		var next []usageKey
		for len(pending) > 0 {
			available := min(CandidateRows+1, limit+1-len(keys.attempts))
			q := min(size, available)
			n := min(len(pending), (CandidateRows+1)/q)
			batch := pending[:n]
			pending = pending[n:]
			last, err := page.fetch(ctx, db, org, batch, size > 1, q, available, &keys)
			if err != nil {
				return usageKeys{}, err
			}
			if len(keys.attempts) > limit {
				return usageKeys{}, reportstore.ErrPopulationExceeded
			}
			next = append(next, last...)
		}
		pending = next
	}
	return keys, nil
}

func (page usagePage) fetch(ctx context.Context, db pgquery.DBTX, org tracker.OrganizationID, batch []usageKey, continuation bool, q, available int, out *usageKeys) ([]usageKey, error) {
	var cursors usageKeys
	for _, k := range batch {
		cursors.add(k)
	}
	args := []any{org, len(batch), q, cursors.attempts}
	driver := "unnest($4::text[]) AS d(attempt_id)"
	order := "period,provider,model,currency,usage_origin"
	source := "attempt_usage"
	columns := "u.period,u.provider,u.model,u.currency,u.usage_origin,''::text AS source_key"
	if page.current {
		source, order = "report_usage_current", "period,source_key"
		columns = "u.period,''::text AS provider,''::text AS model,''::text AS currency,''::text AS usage_origin,u.source_key"
	}
	where := "u.organization_id=$1 AND u.attempt_id=d.attempt_id"
	if continuation {
		args = append(args, cursors.periods)
		if page.current {
			driver = "unnest($4::text[],$5::timestamptz[],$6::text[]) AS d(attempt_id,period,source_key)"
			args = append(args, cursors.sources)
			where += " AND (u.period,u.source_key)>(d.period,d.source_key)"
		} else {
			driver = "unnest($4::text[],$5::timestamptz[],$6::text[],$7::text[],$8::text[],$9::text[]) AS d(attempt_id,period,provider,model,currency,usage_origin)"
			args = append(args, cursors.providers, cursors.models, cursors.currencies, cursors.origins)
			where += " AND (u.period,u.provider,u.model,u.currency,u.usage_origin)>(d.period,d.provider,d.model,d.currency,d.usage_origin)"
		}
	}
	if !page.to.IsZero() {
		args = append(args, page.from, page.to)
		where += fmt.Sprintf(" AND u.period>=$%d AND u.period<$%d", len(args)-1, len(args))
	}
	query := "WITH driver AS MATERIALIZED (SELECT d.* FROM " + driver + " LIMIT $2) SELECT d.attempt_id,k.* FROM driver d CROSS JOIN LATERAL (SELECT " + columns + " FROM " + source + " u WHERE " + where + " ORDER BY " + order + " LIMIT $3) k"
	if len(batch)*q > available {
		args = append(args, available)
		query = "SELECT * FROM (" + query + fmt.Sprintf(" LIMIT $%d", len(args)) + ") AS remaining"
	}
	rows, err := db.Query(ctx, query+" ORDER BY attempt_id,"+order, args...)
	if err != nil {
		return nil, pgcore.MapError(ctx, err)
	}
	defer rows.Close()
	var next []usageKey
	var count int
	var previous string
	for rows.Next() {
		var k usageKey
		if err := rows.Scan(&k.attempt, &k.period, &k.provider, &k.model, &k.currency, &k.origin, &k.source); err != nil {
			return nil, pgcore.MapError(ctx, err)
		}
		out.add(k)
		if k.attempt != previous {
			previous, count = k.attempt, 0
		}
		count++
		if count == q {
			next = append(next, k)
		}
	}
	return next, pgcore.MapError(ctx, rows.Err())
}
