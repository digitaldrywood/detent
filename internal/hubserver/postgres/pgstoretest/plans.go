package pgstoretest

import (
	"context"
	"encoding/json"
	"math"
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

	rowsRounding, removedRounding, indexRemovedRounding float64
}

func (p *SearchPlanNode) UnmarshalJSON(data []byte) error {
	type node SearchPlanNode
	var decoded struct {
		node
		Rows         json.Number `json:"Actual Rows"`
		Removed      json.Number `json:"Rows Removed by Filter"`
		IndexRemoved json.Number `json:"Rows Removed by Index Recheck"`
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = SearchPlanNode(decoded.node)
	var err error
	if p.Rows, p.rowsRounding, err = reportedAverage(decoded.Rows); err != nil {
		return err
	}
	if p.Removed, p.removedRounding, err = reportedAverage(decoded.Removed); err != nil {
		return err
	}
	p.IndexRemoved, p.indexRemovedRounding, err = reportedAverage(decoded.IndexRemoved)
	return err
}

func reportedAverage(value json.Number) (float64, float64, error) {
	if value == "" {
		return 0, 0, nil
	}
	average, err := value.Float64()
	if err != nil {
		return 0, 0, err
	}
	decimals := 0
	if _, fraction, found := strings.Cut(value.String(), "."); found {
		decimals = len(fraction)
	}
	return average, math.Pow10(-decimals) / 2, nil
}

func (p SearchPlanNode) examined() float64 {
	perLoop := p.Rows + p.Removed + p.IndexRemoved
	if p.Loops > 1 {
		perLoop += p.rowsRounding + p.removedRounding + p.indexRemovedRounding
	}
	return perLoop * p.Loops
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
		work = p.examined()
	}
	if p.Relation == "" && (strings.HasPrefix(p.Index, "search_documents_") || strings.HasPrefix(p.Index, "search_postings_")) {
		work += p.examined()
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
