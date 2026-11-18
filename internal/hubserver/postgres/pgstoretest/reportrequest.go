package pgstoretest

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

const ReportCandidateCap = 50000

func WaitForReportSeed(t *testing.T, db *sql.DB) {
	t.Helper()
	var seed string
	if err := db.QueryRowContext(t.Context(), "SELECT pg_current_xact_id()::xid::text").Scan(&seed); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	testenv.WaitUntil(t, ctx, func() error {
		visible, err := reportSeedVisible(ctx, db, seed)
		if err != nil {
			t.Fatal(err)
		}
		if !visible {
			return fmt.Errorf("snapshot still holds report seed transaction %s", seed)
		}
		return nil
	})
}

func reportSeedVisible(ctx context.Context, db *sql.DB, seed string) (bool, error) {
	var visible bool
	err := db.QueryRowContext(ctx, `
SELECT NOT EXISTS (
	SELECT FROM pg_stat_activity
	WHERE datname=current_database() AND pid<>pg_backend_pid()
	  AND greatest(age(backend_xmin), age(backend_xid)) >= age($1::xid)
) AND NOT EXISTS (
	SELECT FROM pg_prepared_xacts WHERE database=current_database() AND age(transaction) >= age($1::xid)
) AND NOT EXISTS (
	SELECT FROM pg_replication_slots WHERE age(xmin) >= age($1::xid)
)`, seed).Scan(&visible)
	return visible, err
}

type ReportRequestBudget struct {
	Overhead  int
	Probes    int
	Allowance int
	Branches  int
}

func (b ReportRequestBudget) Ceiling(candidates int) float64 {
	return float64(b.Overhead) + float64(b.Branches)*float64(candidates+1) + float64(b.Probes*b.Allowance)*float64(candidates)
}

func ReportRequestProblems(results []ReportPlanResult, budget ReportRequestBudget, candidates, limitCap int) []string {
	problems := []string{}
	probes, work, ceiling, rounding := 0, 0.0, 0.0, 0.0
	for _, result := range results {
		probes += result.Probes()
		work += result.Work
		ceiling += result.Ceiling
		for _, scan := range result.Scans {
			if scan.Probe {
				rounding = max(rounding, scan.rounding)
			}
		}
	}
	if probes > budget.Probes {
		problems = append(problems, fmt.Sprintf("request runs %d keyed probe scans above its budget of %d", probes, budget.Probes))
	}
	if limit := budget.Ceiling(limitCap); ceiling > limit {
		problems = append(problems, fmt.Sprintf("request proven worst case %.4g rows exceeds H+B*(C+1)+P*K*C=%.4g at the %d candidate cap", ceiling, limit, limitCap))
	}
	observed := float64(budget.Overhead) + float64(budget.Branches)*float64(candidates+1) + float64(budget.Probes)*(float64(budget.Allowance)+rounding)*float64(candidates)
	if work > observed {
		problems = append(problems, fmt.Sprintf("request examined %.0f rows above H+B*(c+1)+P*(K+r)*c=%.0f for %d relevant candidates", work, observed, candidates))
	}
	return problems
}

func ReportCandidateCount(args []any) int {
	count := 0
	for _, arg := range args {
		value := reflect.ValueOf(arg)
		if value.Kind() == reflect.Slice && value.Type().Elem().Kind() != reflect.Uint8 {
			count = max(count, value.Len())
		}
	}
	return count
}

type ReportStatement struct {
	Query string
	Args  []any
}

type ReportRequestReport struct {
	Problems   []string
	Relations  map[string]bool
	Results    []ReportPlanResult
	Candidates int
}

func ExplainReportPlan(ctx context.Context, db *sql.DB, organization string, statement ReportStatement, settings ...string) ([]byte, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin report explain: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE detent_runtime_v2"); err != nil {
		return nil, fmt.Errorf("set report explain role: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('detent.organization_id',$1,true)", organization); err != nil {
		return nil, fmt.Errorf("set report explain tenant: %w", err)
	}
	for _, setting := range settings {
		if _, err := tx.ExecContext(ctx, "SET LOCAL "+setting); err != nil {
			return nil, fmt.Errorf("set report explain planner: %w", err)
		}
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, VERBOSE, FORMAT JSON) "+statement.Query, statement.Args...).Scan(&raw); err != nil {
		return nil, fmt.Errorf("explain report statement: %w", err)
	}
	return raw, nil
}

func CheckReportRequest(ctx context.Context, db *sql.DB, organization string, statements []ReportStatement, relations []string, budget ReportRequestBudget, settings ...string) (ReportRequestReport, error) {
	indexes, err := LoadReportIndexes(ctx, db)
	if err != nil {
		return ReportRequestReport{}, err
	}
	report := ReportRequestReport{Relations: map[string]bool{}}
	contract := ReportPlanContract{Relations: relations, Indexes: indexes, Cap: ReportCandidateCap}
	for execution, statement := range statements {
		head := strings.ToUpper(strings.TrimSpace(statement.Query))
		for strings.HasPrefix(head, "--") {
			_, head, _ = strings.Cut(head, "\n")
			head = strings.TrimSpace(head)
		}
		if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") {
			continue
		}
		relevant := false
		for _, relation := range relations {
			if strings.Contains(statement.Query, relation) {
				relevant, report.Relations[relation] = true, true
			}
		}
		if !relevant {
			continue
		}
		report.Candidates = max(report.Candidates, ReportCandidateCount(statement.Args))
		raw, err := ExplainReportPlan(ctx, db, organization, statement, settings...)
		if err != nil {
			return ReportRequestReport{}, fmt.Errorf("execution %d: %w", execution, err)
		}
		result, err := CheckReportPlan(raw, statement.Query, statement.Args, contract)
		if err != nil {
			return ReportRequestReport{}, fmt.Errorf("execution %d: %w", execution, err)
		}
		for _, problem := range result.Problems {
			report.Problems = append(report.Problems, fmt.Sprintf("execution %d: %s\nstatement: %s", execution, problem, statement.Query))
		}
		report.Results = append(report.Results, result)
	}
	for _, problem := range ReportRequestProblems(report.Results, budget, report.Candidates, ReportCandidateCap) {
		report.Problems = append(report.Problems, fmt.Sprintf("request: %s (probes %d, candidates %d)", problem, reportProbes(report.Results), report.Candidates))
	}
	return report, nil
}

func reportProbes(results []ReportPlanResult) int {
	probes := 0
	for _, result := range results {
		probes += result.Probes()
	}
	return probes
}

var (
	reportPageBudget   = ReportRequestBudget{Overhead: 1000, Probes: 4, Allowance: 1, Branches: 2}
	reportsBudget      = ReportRequestBudget{Overhead: 1000, Probes: 82, Allowance: 1, Branches: 16}
	reportFilterBudget = ReportRequestBudget{Overhead: 1000, Probes: 46, Allowance: 1, Branches: 16}
)

var ReportRequestBudgets = map[string]ReportRequestBudget{
	"activity":                                 {Overhead: 1000, Probes: 8, Allowance: 1, Branches: 4},
	"newest issue history":                     reportPageBudget,
	"newest issue attempts":                    reportPageBudget,
	"newest issue comments":                    reportPageBudget,
	"work items list":                          reportPageBudget,
	"board lanes":                              reportPageBudget,
	"projects":                                 reportPageBudget,
	"reports":                                  reportsBudget,
	"reports filtered by issue":                reportFilterBudget,
	"reports filtered by attempt":              reportFilterBudget,
	"reports all projects over a custom range": reportFilterBudget,
	"report filter options":                    {Overhead: 1000, Probes: 18, Allowance: 1, Branches: 10},
	"diagnostics":                              {Overhead: 1000, Probes: 14, Allowance: 1, Branches: 6},
}
