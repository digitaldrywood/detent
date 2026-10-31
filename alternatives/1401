package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func attestGoTestReceipts(t *testing.T, service *Service, base, token string) gotestreceipt.Attestation {
	t.Helper()
	response := performHubAPIRequest(t, service, http.MethodPost, base+"/go-test-receipts/attestations", token, nil)
	requireNativeStatus(t, response, http.StatusCreated)
	var attestation gotestreceipt.Attestation
	decodeHubResponse(t, response, &attestation)
	if err := attestation.Validate(); err != nil {
		t.Fatal(err)
	}
	return attestation
}

func lookupGoTestReceipts(t *testing.T, service *Service, base, token, key string) []gotestreceipt.Candidate {
	t.Helper()
	response := performHubAPIRequest(t, service, http.MethodGet, base+"/go-test-receipts?key="+key, token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	var body struct {
		Candidates []gotestreceipt.Candidate `json:"candidates"`
	}
	decodeHubResponse(t, response, &body)
	return body.Candidates
}

func TestGoTestReceiptsRequireALiveAttestationOwnedByTheRecorder(t *testing.T) {
	service := openTestService(t, Config{SecretKeys: secretTestKeys(t, "1", "1"), DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	first := newNativeFixture(t, service, "", "receipts-first")
	second := newNativeFixture(t, service, "", "receipts-second")
	firstWorker := first.worker(t, "first-worker")
	enroll := func(f nativeFixture) string {
		t.Helper()
		r := prepareRunner(t, f, runnerauth.Read, runnerauth.Events)
		r.enroll(t)
		return r.redemption.Credential
	}
	firstRunner, otherRunner, secondRunner := enroll(first), enroll(first), enroll(second)

	firstAttestation := attestGoTestReceipts(t, service, first.base, firstRunner)
	repeatAttestation := attestGoTestReceipts(t, service, first.base, otherRunner)
	secondAttestation := attestGoTestReceipts(t, service, second.base, secondRunner)
	if firstAttestation.ID == repeatAttestation.ID || !bytes.Equal(firstAttestation.DigestKey, repeatAttestation.DigestKey) || bytes.Equal(firstAttestation.DigestKey, secondAttestation.DigestKey) {
		t.Fatal("attestations must be distinct while digest keys are shared within a project and differ across projects")
	}
	for name, request := range map[string]struct {
		base, token string
	}{
		"project worker without a lease": {first.base, firstWorker},
		"runner outside the project":     {first.base, secondRunner},
	} {
		if response := performHubAPIRequest(t, service, http.MethodPost, request.base+"/go-test-receipts/attestations", request.token, nil); response.Code < 400 || response.Code >= 500 {
			t.Fatalf("%s: attestation status = %d, want a client error: %s", name, response.Code, response.Body.String())
		}
	}

	key := strings.Repeat("a", 64)
	legitimate := gotestreceipt.Receipt{Attestation: firstAttestation.ID, Key: key, Package: "internal/example", Inputs: []string{"env\tHOME", "open\tm\tinternal/example/testdata/a.txt"}, Digest: strings.Repeat("b", 64)}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", firstRunner, legitimate), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", firstRunner, legitimate), http.StatusNoContent)
	secondReceipt := legitimate
	secondReceipt.Attestation, secondReceipt.Digest = secondAttestation.ID, strings.Repeat("c", 64)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, second.base+"/go-test-receipts", secondRunner, secondReceipt), http.StatusNoContent)

	for name, f := range map[string]struct {
		base, token string
		want        gotestreceipt.Receipt
	}{
		"first project reuses its own receipt":  {first.base, firstWorker, legitimate},
		"second project reuses its own receipt": {second.base, secondRunner, secondReceipt},
	} {
		candidates := lookupGoTestReceipts(t, service, f.base, f.token, key)
		if len(candidates) != 1 || candidates[0].Digest != f.want.Digest || !slices.Equal(candidates[0].Inputs, f.want.Inputs) {
			t.Fatalf("%s: candidates = %+v", name, candidates)
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, second.base+"/go-test-receipts?key="+key, firstWorker, nil), http.StatusNotFound)

	expired := attestGoTestReceipts(t, service, first.base, firstRunner)
	if _, err := service.database.db.ExecContext(t.Context(), "UPDATE go_test_receipt_attestations SET expires_at_ns = ? WHERE id = ?", time.Now().Add(-time.Minute).UnixNano(), expired.ID); err != nil {
		t.Fatal(err)
	}
	forgedKey := strings.Repeat("f", 64)
	forged := func(attestation string, inputs ...string) gotestreceipt.Receipt {
		return gotestreceipt.Receipt{Attestation: attestation, Key: forgedKey, Package: "internal/target", Inputs: inputs, Digest: strings.Repeat("e", 64)}
	}
	for name, attempt := range map[string]struct {
		base, token string
		receipt     gotestreceipt.Receipt
	}{
		"empty-input receipt without an attestation":    {first.base, firstRunner, forged("")},
		"empty-input receipt with an invented session":  {first.base, firstRunner, forged("gtra_00000000000000000000000000000000")},
		"another runner's attestation":                  {first.base, otherRunner, forged(firstAttestation.ID)},
		"a project worker replaying a runner's session": {first.base, firstWorker, forged(firstAttestation.ID)},
		"another project's attestation":                 {first.base, firstRunner, forged(secondAttestation.ID)},
		"an attestation used in another project":        {second.base, secondRunner, forged(firstAttestation.ID)},
		"an expired attestation":                        {first.base, firstRunner, forged(expired.ID)},
		"an absolute host path input":                   {first.base, firstRunner, forged(firstAttestation.ID, "open\ta\t/etc/hosts")},
		"an absolute path disguised as checkout input":  {first.base, firstRunner, forged(firstAttestation.ID, "open\tm\t/etc/hosts")},
		"a malformed input":                             {first.base, firstRunner, forged(firstAttestation.ID, "open\tm\tline\nbreak")},
	} {
		if response := performHubAPIRequest(t, service, http.MethodPost, attempt.base+"/go-test-receipts", attempt.token, attempt.receipt); response.Code < 400 || response.Code >= 500 {
			t.Fatalf("%s: status = %d, want a client error: %s", name, response.Code, response.Body.String())
		}
	}
	for _, base := range []string{first.base, second.base} {
		token := firstRunner
		if base == second.base {
			token = secondRunner
		}
		if candidates := lookupGoTestReceipts(t, service, base, token, forgedKey); len(candidates) != 0 {
			t.Fatalf("forged receipt returned by lookup: %+v", candidates)
		}
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, first.base+"/go-test-receipts?key=ABC", firstRunner, nil), http.StatusUnprocessableEntity)

	unbound := forged("")
	if err := storeGoTestReceipt(t.Context(), service.database.db, string(first.project.OrganizationID), string(first.project.ID), "legacy", unbound, time.Now()); err != nil {
		t.Fatal(err)
	}
	if candidates := lookupGoTestReceipts(t, service, first.base, firstRunner, forgedKey); len(candidates) != 0 {
		t.Fatalf("receipt without an attestation returned by lookup: %+v", candidates)
	}

	later := time.Now().Add(goTestReceiptRetention + time.Hour)
	if expiredCandidates, err := readGoTestReceipts(t.Context(), service.database.reader, string(first.project.OrganizationID), string(first.project.ID), key, later); err != nil || len(expiredCandidates) != 0 {
		t.Fatalf("lookup before pruning returned expired candidates %+v (%v)", expiredCandidates, err)
	}
	if err := service.database.expireGoTestReceipts(t.Context(), later, time.Now); err != nil {
		t.Fatal(err)
	}
	var receipts, attestations int
	if err := service.database.db.QueryRowContext(t.Context(), "SELECT (SELECT count(*) FROM go_test_receipts), (SELECT count(*) FROM go_test_receipt_attestations)").Scan(&receipts, &attestations); err != nil || receipts != 0 || attestations != 0 {
		t.Fatalf("after pruning receipts = %d, attestations = %d (%v), want 0", receipts, attestations, err)
	}
}

func TestGoTestReceiptsRefuseLeasedWorkers(t *testing.T) {
	f := claimedAttemptDiffFixture(t, false, Config{SecretKeys: secretTestKeys(t, "1", "1")})
	r := prepareRunner(t, f.nativeFixture, runnerauth.Read, runnerauth.Events)
	r.enroll(t)
	runnerAttestation := attestGoTestReceipts(t, f.service, f.base, r.redemption.Credential)
	lease := map[string]any{"lease_id": f.lease.ID, "fencing_token": f.lease.FencingToken}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/go-test-receipts/attestations", f.worker, lease)
	if response.Code < 400 || response.Code >= 500 || strings.Contains(response.Body.String(), "digest_key") {
		t.Fatalf("leased worker attestation: status = %d, want a client error without a digest key: %s", response.Code, response.Body.String())
	}
	seedHubAPIToken(t, f.service, "receipt-admin", "receipt-admin-token", apiScopeAdmin)
	if response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/go-test-receipts/attestations", "receipt-admin-token", nil); response.Code < 400 || response.Code >= 500 || strings.Contains(response.Body.String(), "digest_key") {
		t.Fatalf("hosted administrator attestation: status = %d, want a client error without a digest key: %s", response.Code, response.Body.String())
	}
	forged := gotestreceipt.Receipt{Attestation: runnerAttestation.ID, Key: strings.Repeat("f", 64), Package: "internal/target", Inputs: []string{}, Digest: strings.Repeat("e", 64)}
	if response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/go-test-receipts", f.worker, forged); response.Code < 400 || response.Code >= 500 {
		t.Fatalf("leased worker record: status = %d, want a client error: %s", response.Code, response.Body.String())
	}
	if candidates := lookupGoTestReceipts(t, f.service, f.base, f.worker, forged.Key); len(candidates) != 0 {
		t.Fatalf("leased worker receipt returned by lookup: %+v", candidates)
	}
}

type receiptPruneObserver struct {
	db      *sql.DB
	deleted []int64
}

func (o *receiptPruneObserver) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := o.db.ExecContext(context.WithoutCancel(ctx), query, args...)
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
	const expiredAttestations = 10
	if _, err := db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i+1<?)
INSERT INTO go_test_receipt_attestations (id, organization_id, project_id, credential_id, runner_id, created_at_ns, expires_at_ns)
SELECT printf('gtra_%032x', i), ?, ?, 'credential', 'runner', ?, ? FROM n`, expiredAttestations, tenants[0], "prj_"+tenants[0], now.Add(-2*goTestReceiptAttestationTTL).UnixNano(), now.Add(-goTestReceiptAttestationTTL).UnixNano()); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
		index string
	}{
		{goTestReceiptAttestationPruneQuery, []any{now.UnixNano()}, "INDEX go_test_receipt_attestations_expiry (expires_at_ns<?)"},
		{goTestReceiptPruneQuery, []any{now.UnixNano()}, "INDEX go_test_receipts_recorded (recorded_at_ns<?)"},
		{goTestReceiptLookupQuery, []any{tenants[0], "prj_" + tenants[0], strings.Repeat("f", 64), now.UnixNano(), gotestreceipt.MaxCandidates}, "INDEX go_test_receipts_attested_lookup (organization_id=? AND project_id=? AND binary_key=? AND recorded_at_ns>?)"},
	} {
		if joined := receiptQueryPlan(t, db, statement.query, statement.args...); !strings.Contains(joined, statement.index) || strings.Contains(joined, "TEMP B-TREE") {
			t.Fatalf("plan %q is not served by %s", joined, statement.index)
		}
	}

	firstTick := statementsPerTick - 1
	for tick, want := range []int{firstTick, 2*perTenant + expiredAttestations - firstTick} {
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
			t.Fatalf("tick deleted %d rows, want %d", total, want)
		}
		var attestations int
		if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM go_test_receipt_attestations").Scan(&attestations); err != nil || attestations != 0 {
			t.Fatalf("tick %d left %d expired attestations (%v); expired receipts must not starve attestation pruning", tick, attestations, err)
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

func TestGoTestReceiptsKeepOnlyTheReadableCandidatesPerBinary(t *testing.T) {
	service := openTestService(t, Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	db := service.database.db
	now := time.Now()
	projects := []string{"prj_evict_first", "prj_evict_second"}
	key := strings.Repeat("e", 64)
	writes := gotestreceipt.MaxCandidates + 3
	for _, project := range projects {
		for i := range writes {
			receipt := gotestreceipt.Receipt{Key: key, Digest: fmt.Sprintf("%064x", i), Package: "internal/example", Inputs: []string{}, Attestation: "gtra_example"}
			if err := storeGoTestReceipt(t.Context(), db, "org_evict", project, "runner", receipt, now.Add(time.Duration(i)*time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, project := range projects {
		var count int
		var oldest string
		if err := service.database.reader.QueryRowContext(t.Context(), "SELECT count(*), min(inputs_digest) FROM go_test_receipts WHERE organization_id='org_evict' AND project_id=? AND binary_key=?", project, key).Scan(&count, &oldest); err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("%064x", writes-gotestreceipt.MaxCandidates); count != gotestreceipt.MaxCandidates || oldest != want {
			t.Fatalf("%s kept %d receipts, oldest %s; want the newest %d, oldest %s", project, count, oldest, gotestreceipt.MaxCandidates, want)
		}
	}
	if joined := receiptQueryPlan(t, db, goTestReceiptEvictQuery, "org_evict", projects[0], key, gotestreceipt.MaxCandidates); !strings.Contains(joined, "INDEX go_test_receipts_lookup") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("evict plan %q is not served by go_test_receipts_lookup", joined)
	}
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
