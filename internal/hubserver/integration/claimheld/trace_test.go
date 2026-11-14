package claimheld_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

const candidateStatement = "SELECT i.id,i.revision,i.number,i.native_id,"

type capturedCandidate struct {
	statement string
	args      []any
}

type candidateTrace struct {
	queries atomic.Int64
	rows    atomic.Int64
	mu      sync.Mutex
	scan    capturedCandidate
}

type candidateQueryKey struct{}

func (c *candidateTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.HasPrefix(strings.TrimSpace(data.SQL), candidateStatement) {
		return ctx
	}
	c.queries.Add(1)
	if strings.Contains(data.SQL, "latest.claim_disposition") {
		c.mu.Lock()
		c.scan = capturedCandidate{statement: data.SQL, args: append([]any(nil), data.Args...)}
		c.mu.Unlock()
	}
	return context.WithValue(ctx, candidateQueryKey{}, true)
}

func (c *candidateTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(candidateQueryKey{}) == true && data.Err == nil {
		c.rows.Add(data.CommandTag.RowsAffected())
	}
}

func (c *candidateTrace) captured() capturedCandidate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.scan
}

type planNode struct {
	NodeType     string     `json:"Node Type"`
	RelationName string     `json:"Relation Name"`
	IndexName    string     `json:"Index Name"`
	ActualLoops  float64    `json:"Actual Loops"`
	ActualRows   float64    `json:"Actual Rows"`
	SharedHit    int64      `json:"Shared Hit Blocks"`
	SharedRead   int64      `json:"Shared Read Blocks"`
	Plans        []planNode `json:"Plans"`
}

type planSummary struct {
	buffers        int64
	attemptProbes  float64
	eventProbes    float64
	sequentialScan []string
}

func explainCandidate(t *testing.T, h *hubfixture.Hub, query capturedCandidate) planSummary {
	t.Helper()
	if query.statement == "" {
		t.Fatal("candidate query was not captured")
	}
	var raw []byte
	if err := h.Admin.QueryRowContext(t.Context(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+query.statement, query.args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plan=%s err=%v", raw, err)
	}
	summary := planSummary{buffers: plans[0].Plan.SharedHit + plans[0].Plan.SharedRead}
	var walk func(planNode)
	walk = func(node planNode) {
		switch node.RelationName {
		case "native_attempts":
			summary.attemptProbes += node.ActualLoops
		case "collaboration_events":
			summary.eventProbes += node.ActualLoops
		}
		if node.NodeType == "Seq Scan" && node.ActualLoops > 1 && (node.RelationName == "native_attempts" || node.RelationName == "collaboration_events" || node.RelationName == "issues") {
			summary.sequentialScan = append(summary.sequentialScan, node.RelationName)
		}
		for _, child := range node.Plans {
			walk(child)
		}
	}
	walk(plans[0].Plan)
	return summary
}
