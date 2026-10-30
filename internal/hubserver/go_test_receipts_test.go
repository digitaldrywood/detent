package hubserver

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

func TestGoTestReceiptsAreProjectScopedAndExpire(t *testing.T) {
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	first := newNativeFixture(t, service, "", "receipts-first")
	second := newNativeFixture(t, service, "", "receipts-second")
	firstWorker, secondWorker := first.worker(t, "first-runner"), second.worker(t, "second-runner")
	key := strings.Repeat("a", 64)
	lookup := func(f nativeFixture, token string) []gotestreceipt.Candidate {
		t.Helper()
		response := performHubAPIRequest(t, service, http.MethodGet, f.base+"/go-test-receipts?key="+key, token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var body struct {
			Candidates []gotestreceipt.Candidate `json:"candidates"`
		}
		decodeHubResponse(t, response, &body)
		return body.Candidates
	}
	receipt := func(digest byte) gotestreceipt.Receipt {
		return gotestreceipt.Receipt{Key: key, Package: "internal/example", Inputs: []string{"env\tHOME", "open\tm\tinternal/example/testdata/a.txt"}, Digest: strings.Repeat(string(digest), 64)}
	}

	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", firstWorker, receipt('b')), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", firstWorker, receipt('b')), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, second.base+"/go-test-receipts", secondWorker, receipt('c')), http.StatusNoContent)

	firstCandidates, secondCandidates := lookup(first, firstWorker), lookup(second, secondWorker)
	if len(firstCandidates) != 1 || firstCandidates[0].Digest != receipt('b').Digest || !slices.Equal(firstCandidates[0].Inputs, receipt('b').Inputs) {
		t.Fatalf("first project candidates = %+v", firstCandidates)
	}
	if len(secondCandidates) != 1 || secondCandidates[0].Digest != receipt('c').Digest {
		t.Fatalf("second project candidates = %+v, want only its own receipt", secondCandidates)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, second.base+"/go-test-receipts?key="+key, firstWorker, nil), http.StatusNotFound)

	invalid := receipt('b')
	invalid.Inputs = []string{"open\tm\tline\nbreak"}
	for name, request := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"malformed key":   {http.MethodGet, first.base + "/go-test-receipts?key=ABC", nil},
		"malformed input": {http.MethodPost, first.base + "/go-test-receipts", invalid},
	} {
		if response := performHubAPIRequest(t, service, request.method, request.path, firstWorker, request.body); response.Code < 400 || response.Code >= 500 {
			t.Fatalf("%s: status = %d, want a client error: %s", name, response.Code, response.Body.String())
		}
	}

	later := time.Now().Add(goTestReceiptRetention + time.Hour)
	expired, err := readGoTestReceipts(t.Context(), service.database.reader, string(first.project.OrganizationID), string(first.project.ID), key, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 0 {
		t.Fatalf("lookup before pruning returned expired candidates %+v", expired)
	}
	if err := service.database.expireGoTestReceipts(t.Context(), later, time.Now); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM go_test_receipts").Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("receipts after pruning = %d (%v), want 0", remaining, err)
	}
}

type receiptPruneObserver struct {
	db      *sql.DB
	deleted []int64
}

func (o *receiptPruneObserver) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := o.db.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	o.deleted = append(o.deleted, n)
	return result, nil
}

type steppedClock struct {
	now  time.Time
	step time.Duration
}

func (c *steppedClock) Now() time.Time {
	c.now = c.now.Add(c.step)
	return c.now
}

func receiptQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(plan, ";")
}

func TestGoTestReceiptPruningIsBoundedPerTransactionAndTick(t *testing.T) {
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	db := service.database.db
	now := time.Now()
	clock := &steppedClock{now: now, step: time.Millisecond}
	statementsPerTick := int(goTestReceiptPruneBudget/clock.step) - 1
	perTenant := statementsPerTick/2 + 50
	tenants := []string{"org_prune_first", "org_prune_second"}
	for _, org := range tenants {
		if _, err := db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i+1<?)
INSERT INTO go_test_receipts (organization_id, project_id, binary_key, inputs_digest, inputs, package, runner_id, recorded_at_ns)
SELECT ?, ?, printf('%064x', i), ?, '', 'internal/example', 'runner', ? FROM n`, perTenant, org, "prj_"+org, strings.Repeat("a", 64), now.Add(-goTestReceiptRetention-time.Hour).UnixNano()); err != nil {
			t.Fatal(err)
		}
		fresh := gotestreceipt.Receipt{Key: strings.Repeat("f", 64), Digest: strings.Repeat("b", 64), Package: "internal/example", Inputs: []string{}}
		if err := storeGoTestReceipt(t.Context(), db, org, "prj_"+org, "runner", fresh, now); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []struct {
		query string
		args  []any
		index string
	}{
		{goTestReceiptPruneQuery, []any{now.UnixNano()}, "INDEX go_test_receipts_recorded (recorded_at_ns<?)"},
		{goTestReceiptLookupQuery, []any{tenants[0], "prj_" + tenants[0], strings.Repeat("f", 64), now.UnixNano(), gotestreceipt.MaxCandidates}, "INDEX go_test_receipts_lookup (organization_id=? AND project_id=? AND binary_key=? AND recorded_at_ns>?)"},
	} {
		if joined := receiptQueryPlan(t, db, statement.query, statement.args...); !strings.Contains(joined, statement.index) || strings.Contains(joined, "TEMP B-TREE") {
			t.Fatalf("plan %q is not served by %s", joined, statement.index)
		}
	}

	for _, want := range []int{statementsPerTick, 2*perTenant - statementsPerTick} {
		observer := &receiptPruneObserver{db: db}
		if err := expireGoTestReceipts(t.Context(), observer, now, clock.Now); err != nil {
			t.Fatal(err)
		}
		var total int64
		for _, deleted := range observer.deleted {
			if deleted > 1 {
				t.Fatalf("one transaction deleted %d receipts, want one receipt per transaction", deleted)
			}
			total += deleted
		}
		if total != int64(want) {
			t.Fatalf("tick deleted %d receipts, want %d", total, want)
		}
	}
	for _, org := range tenants {
		var expired, fresh int
		if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FILTER (WHERE recorded_at_ns < ?), count(*) FILTER (WHERE recorded_at_ns >= ?) FROM go_test_receipts WHERE organization_id=?", now.Add(-goTestReceiptRetention).UnixNano(), now.Add(-goTestReceiptRetention).UnixNano(), org).Scan(&expired, &fresh); err != nil {
			t.Fatal(err)
		}
		if expired != 0 || fresh != 1 {
			t.Fatalf("%s: expired=%d fresh=%d, want 0 expired and its fresh receipt kept", org, expired, fresh)
		}
	}
}

type blockedPruneWriter struct {
	cause error
}

func (w *blockedPruneWriter) ExecContext(ctx context.Context, _ string, _ ...any) (sql.Result, error) {
	<-ctx.Done()
	w.cause = context.Cause(ctx)
	return nil, ctx.Err()
}

func TestGoTestReceiptPruningStopsWaitingOnBusyWriterAtBudget(t *testing.T) {
	errGuard := errors.New("deadlock guard expired")
	ctx, cancel := context.WithTimeoutCause(t.Context(), time.Minute, errGuard)
	defer cancel()
	now := time.Now()
	clock := &steppedClock{now: now, step: goTestReceiptPruneBudget - time.Millisecond}
	writer := &blockedPruneWriter{}
	err := expireGoTestReceipts(ctx, writer, now, clock.Now)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(writer.cause, context.DeadlineExceeded) {
		t.Fatalf("prune error = %v, statement cause = %v; want the statement bounded by the remaining budget", err, writer.cause)
	}
}
