package searchsql

import (
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/viewstore"
)

const TermsUpsert = `INSERT INTO native_work_item_search(organization_id,project_id,work_item_id,number,terms)
VALUES($1,$2,$3,$4,setweight(to_tsvector('pg_catalog.english'::regconfig,lower(left($5::text,1024) COLLATE pg_catalog.pg_c_utf8)),'A')||strip(to_tsvector('pg_catalog.english'::regconfig,lower(left($6::text,32768) COLLATE pg_catalog.pg_c_utf8))))
ON CONFLICT(organization_id,project_id,work_item_id) DO UPDATE SET number=EXCLUDED.number,terms=EXCLUDED.terms
WHERE native_work_item_search.number<>EXCLUDED.number OR native_work_item_search.terms<>EXCLUDED.terms`

const TermsDelete = "DELETE FROM native_work_item_search WHERE organization_id=$1 AND project_id=$2 AND work_item_id=$3"

func Window(args *[]any, f viewstore.BoardSelection, columns string) string {
	bound := ""
	if p := f.After; p != nil && f.SearchRanked && p.SearchWindow > 0 && !f.WorkOrder {
		*args = append(*args, p.SearchWindow)
		bound = fmt.Sprintf(" AND number<$%d", len(*args))
	}
	return fmt.Sprintf("(SELECT %s FROM native_work_item_search WHERE organization_id=$1 AND project_id=$2%s ORDER BY number DESC,work_item_id LIMIT %d)", columns, bound, searchstore.WindowRows)
}

func FingerprintScan(args *[]any, f viewstore.BoardSelection) string {
	*args = append(*args, searchstore.FingerprintDigest(f.Fingerprint))
	digest := len(*args)
	start := ""
	if p := f.After; p != nil && f.SearchRanked && p.SearchFingerprintAfter != "" && f.SearchText == "" {
		*args = append(*args, p.SearchFingerprintAfter)
		start = fmt.Sprintf(" AND f.work_item_id>$%d", len(*args))
	}
	next := func(after string) string {
		return fmt.Sprintf("(SELECT f.work_item_id FROM native_work_item_fingerprints f WHERE f.organization_id=$1 AND f.project_id=$2 AND f.fingerprint_digest=$%d%s ORDER BY f.work_item_id LIMIT 1)", digest, after)
	}
	return "(WITH RECURSIVE scan(work_item_id,steps) AS (SELECT " + next(start) + ",1 UNION ALL SELECT " + next(" AND f.work_item_id>scan.work_item_id") + fmt.Sprintf(",scan.steps+1 FROM scan WHERE scan.work_item_id IS NOT NULL AND scan.steps<%d) SELECT work_item_id FROM scan WHERE work_item_id IS NOT NULL)", searchstore.WindowRows)
}

func Ranked(f viewstore.BoardSelection, where string, args []any, columns string) (string, []any, error) {
	if f.WorkOrder && f.After != nil {
		return "", nil, corestore.ErrInvalidPage
	}
	number, name := searchstore.Number(f.SearchText)
	args = append(args, number, name)
	numberArg, nameArg := len(args)-1, len(args)
	named := fmt.Sprintf("($%d='' OR lower($%d)=lower($2) OR lower($%d)=(SELECT lower(p.name) FROM projects p WHERE p.organization_id=$1 AND p.id=$2))", nameArg, nameArg, nameArg)
	var matched string
	if f.SearchText != "" {
		if err := (searchstore.Query{Kind: "work", ProjectID: f.ProjectID, Text: f.SearchText, Limit: 1}).Validate(); err != nil {
			return "", nil, err
		}
		window := Window(&args, f, "work_item_id,number,terms")
		args = append(args, searchstore.WindowQuery(f.SearchText))
		query := fmt.Sprintf("to_tsquery('pg_catalog.english',$%d)", len(args))
		keyed := func(string) string { return "" }
		if f.Fingerprint != "" {
			args = append(args, searchstore.FingerprintDigest(f.Fingerprint))
			digest := len(args)
			keyed = func(alias string) string {
				return fmt.Sprintf(" AND EXISTS(SELECT 1 FROM native_work_item_fingerprints k WHERE k.organization_id=$1 AND k.project_id=$2 AND k.fingerprint_digest=$%d AND k.work_item_id=%s)", digest, alias)
			}
		}
		matched = "WITH text_matches AS MATERIALIZED (SELECT w.work_item_id,w.number,ts_rank(w.terms," + query + ")::double precision AS rank FROM " + window + " w WHERE w.terms @@ " + query + keyed("w.work_item_id") + "), matched AS MATERIALIZED (SELECT work_item_id,number,rank FROM text_matches"
		if number > 0 {
			matched += fmt.Sprintf(" UNION ALL SELECT i.native_id,i.number,0::double precision FROM issues i WHERE i.organization_id=$1 AND i.project_id=$2 AND i.number=$%d AND %s AND NOT EXISTS(SELECT 1 FROM text_matches t WHERE t.work_item_id=i.native_id)", numberArg, named) + keyed("i.native_id")
		}
		matched += ")"
	} else {
		matched = "WITH matched AS MATERIALIZED (SELECT k.work_item_id,i.number,0::double precision AS rank FROM " + FingerprintScan(&args, f) + " k JOIN issues i ON i.organization_id=$1 AND i.project_id=$2 AND i.native_id=k.work_item_id)"
	}
	exact := fmt.Sprintf("($%d>0 AND m.number=$%d AND %s)", numberArg, numberArg, named)
	after := ""
	if p := f.After; p != nil {
		if p.Identifier != "" {
			args = append(args, p.SearchExact, p.SearchRank, p.SearchNumber)
			base := len(args) - 3
			after = fmt.Sprintf(" WHERE exact<$%d OR (exact=$%d AND (rank<$%d OR (rank=$%d AND number<$%d)))", base+1, base+1, base+2, base+2, base+3)
		} else if p.SearchWindow > 0 {
			after = " WHERE NOT exact"
		}
	}
	inner, outer := "s.exact DESC,s.rank DESC,s.number DESC", "page.exact DESC,page.rank DESC,page.number DESC,b.identifier"
	if f.WorkOrder {
		inner, outer = "s.exact DESC,b.running DESC,b.dispatchable DESC,s.number DESC,b.identifier", "page.exact DESC,page.running DESC,page.dispatchable DESC,page.number DESC,b.identifier"
	}
	args = append(args, f.Limit)
	query := matched + ", scored AS MATERIALIZED (SELECT m.work_item_id,m.number,m.rank," + exact + " AS exact FROM matched m)" +
		", page AS (SELECT s.work_item_id,s.exact,s.rank,s.number,b.running,b.dispatchable FROM (SELECT * FROM scored" + after + " ORDER BY exact DESC,rank DESC,number DESC OFFSET 0) s" +
		" CROSS JOIN LATERAL (SELECT b.identifier,b.running,b.dispatchable FROM native_board_items b WHERE " + where + " AND b.work_item_id=s.work_item_id LIMIT 1) b" +
		" ORDER BY " + inner + fmt.Sprintf(" LIMIT $%d)", len(args)) +
		" SELECT " + columns + ",page.exact,page.rank,page.number FROM page JOIN native_board_items b ON b.organization_id=$1 AND b.project_id=$2 AND b.work_item_id=page.work_item_id ORDER BY " + outer
	return query, args, nil
}
