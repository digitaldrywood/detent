package pgstoretest

import (
	"context"
	"strings"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgquery"
	"github.com/jackc/pgx/v5"
)

type SearchPlanProbe struct {
	pgquery.DBTX
	Statement string
	Args      []any
}

func (p *SearchPlanProbe) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	p.Statement, p.Args = statement, args
	return p.DBTX.Query(ctx, statement, args...)
}

type SearchPlanNode struct {
	Alias        string           `json:"Alias"`
	Index        string           `json:"Index Name"`
	Relation     string           `json:"Relation Name"`
	Rows         float64          `json:"Actual Rows"`
	Loops        float64          `json:"Actual Loops"`
	Removed      float64          `json:"Rows Removed by Filter"`
	IndexRemoved float64          `json:"Rows Removed by Index Recheck"`
	Plans        []SearchPlanNode `json:"Plans"`
}

func (p SearchPlanNode) UsesIndex(name string) bool {
	if p.Index == name && p.Loops > 0 {
		return true
	}
	for _, child := range p.Plans {
		if child.UsesIndex(name) {
			return true
		}
	}
	return false
}

func (p SearchPlanNode) DocumentWork() float64 {
	work := float64(0)
	if p.Relation == "search_documents" || p.Relation == "issues" || p.Relation == "search_postings" || p.Relation == "search_scope_refresh" || p.Relation == "search_scope_documents" || p.Relation == "projects" || p.Relation == "native_comments" {
		work = (p.Rows + p.Removed + p.IndexRemoved) * p.Loops
	}
	if p.Relation == "" && (strings.HasPrefix(p.Index, "search_documents_") || strings.HasPrefix(p.Index, "search_postings_")) {
		work += (p.Rows + p.Removed + p.IndexRemoved) * p.Loops
	}
	for _, child := range p.Plans {
		work += child.DocumentWork()
	}
	return work
}

type SearchPublicationProbe struct {
	pgx.Tx
	Statement string
	Args      []any
}

func (p *SearchPublicationProbe) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	p.Statement, p.Args = statement, args
	return p.Tx.QueryRow(ctx, statement, args...)
}
