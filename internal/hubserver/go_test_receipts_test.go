package hubserver

import (
	"bytes"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

func TestGoTestReceiptsAreProjectScopedAndExpire(t *testing.T) {
	service := openTestService(t, Config{SecretKeys: secretTestKeys(t, "1", "1"), DatabasePath: filepath.Join(t.TempDir(), "hub.db")})
	first := newNativeFixture(t, service, "", "receipts-first")
	second := newNativeFixture(t, service, "", "receipts-second")
	firstWorker, secondWorker := first.worker(t, "first-runner"), second.worker(t, "second-runner")
	firstRunner, secondRunner := prepareRunner(t, first, "events"), prepareRunner(t, second, "events")
	firstRunner.enroll(t)
	secondRunner.enroll(t)
	otherRunner := prepareRunner(t, first, "events")
	otherRunner.enroll(t)
	begin := func(f nativeFixture, token string) gotestreceipt.Execution {
		t.Helper()
		response := performHubAPIRequest(t, service, http.MethodPost, f.base+"/go-test-receipts/executions", token, nil)
		requireNativeStatus(t, response, http.StatusCreated)
		var execution gotestreceipt.Execution
		decodeHubResponse(t, response, &execution)
		return execution
	}
	firstExecution := begin(first, firstRunner.redemption.Credential)
	secondExecution := begin(second, secondRunner.redemption.Credential)
	repeatExecution := begin(first, firstRunner.redemption.Credential)
	if firstExecution.ID == repeatExecution.ID || !bytes.Equal(firstExecution.DigestKey, repeatExecution.DigestKey) || bytes.Equal(firstExecution.DigestKey, secondExecution.DigestKey) {
		t.Fatal("execution identity or project digest key scope is incorrect")
	}
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
		execution := firstExecution.ID
		if digest == 'c' {
			execution = secondExecution.ID
		}
		return gotestreceipt.Receipt{ExecutionID: execution, Key: key, Package: "internal/example", Inputs: []string{"env\tHOME", "open\tm\tinternal/example/testdata/a.txt"}, Digest: strings.Repeat(string(digest), 64)}
	}

	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", firstRunner.redemption.Credential, receipt('b')), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", firstRunner.redemption.Credential, receipt('b')), http.StatusNoContent)
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, second.base+"/go-test-receipts", secondRunner.redemption.Credential, receipt('c')), http.StatusNoContent)

	firstCandidates, secondCandidates := lookup(first, firstWorker), lookup(second, secondWorker)
	if len(firstCandidates) != 1 || firstCandidates[0].Digest != receipt('b').Digest || !slices.Equal(firstCandidates[0].Inputs, receipt('b').Inputs) {
		t.Fatalf("first project candidates = %+v", firstCandidates)
	}
	if len(secondCandidates) != 1 || secondCandidates[0].Digest != receipt('c').Digest {
		t.Fatalf("second project candidates = %+v, want only its own receipt", secondCandidates)
	}
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodGet, second.base+"/go-test-receipts?key="+key, firstWorker, nil), http.StatusNotFound)

	missingExecution := receipt('b')
	missingExecution.ExecutionID = ""
	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", firstRunner.redemption.Credential, missingExecution), http.StatusNotFound)
	for name, token := range map[string]string{"worker": firstWorker, "other runner": otherRunner.redemption.Credential} {
		forged := receipt('b')
		forged.Key = strings.Repeat("f", 64)
		forged.Inputs = nil
		var digestErr error
		forged.Digest, digestErr = gotestreceipt.Digest(nil, gotestreceipt.Environment{})
		if digestErr != nil {
			t.Fatal(digestErr)
		}
		response := performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts", token, forged)
		if response.Code < 400 || response.Code >= 500 {
			t.Fatalf("%s forged receipt: %d", name, response.Code)
		}
		response = performHubAPIRequest(t, service, http.MethodPost, first.base+"/go-test-receipts/executions", firstWorker, nil)
		if response.Code < 400 || response.Code >= 500 {
			t.Fatal("project worker obtained digest key")
		}
		candidates, err := readGoTestReceipts(t.Context(), service.database.reader, string(first.project.OrganizationID), string(first.project.ID), forged.Key)
		if err != nil || len(candidates) != 0 {
			t.Fatalf("forged receipt returned: %+v %v", candidates, err)
		}
	}
	invalid := receipt('b')
	invalid.Inputs = []string{"open\tm\tline\nbreak"}
	absolute := receipt('b')
	absolute.Inputs = []string{"open\ta\t/etc/hosts"}
	disguised := receipt('b')
	disguised.Inputs = []string{"open\tm\t/etc/hosts"}
	for name, request := range map[string]struct {
		method string
		path   string
		body   any
	}{
		"absolute host path":           {http.MethodPost, first.base + "/go-test-receipts", absolute},
		"disguised absolute host path": {http.MethodPost, first.base + "/go-test-receipts", disguised},
		"malformed key":                {http.MethodGet, first.base + "/go-test-receipts?key=ABC", nil},
		"malformed input":              {http.MethodPost, first.base + "/go-test-receipts", invalid},
	} {
		if response := performHubAPIRequest(t, service, request.method, request.path, firstRunner.redemption.Credential, request.body); response.Code < 400 || response.Code >= 500 {
			t.Fatalf("%s: status = %d, want a client error: %s", name, response.Code, response.Body.String())
		}
	}

	requireNativeStatus(t, performHubAPIRequest(t, service, http.MethodPost, second.base+"/go-test-receipts", firstRunner.redemption.Credential, receipt('b')), http.StatusNotFound)
	legacyUnbound := receipt('b')
	legacyUnbound.Key = strings.Repeat("e", 64)
	legacyUnbound.ExecutionID = ""
	legacyUnbound.Inputs = nil
	if err := storeGoTestReceipt(t.Context(), service.database.db, string(first.project.OrganizationID), string(first.project.ID), "legacy", legacyUnbound, service.config.now()); err != nil {
		t.Fatal(err)
	}
	unbound, err := readGoTestReceipts(t.Context(), service.database.reader, string(first.project.OrganizationID), string(first.project.ID), legacyUnbound.Key)
	if err != nil || len(unbound) != 0 {
		t.Fatalf("legacy unbound receipt returned: %+v %v", unbound, err)
	}
	legacy := absolute
	legacy.Digest = strings.Repeat("d", 64)
	if err := storeGoTestReceipt(t.Context(), service.database.db, string(first.project.OrganizationID), string(first.project.ID), "legacy", legacy, service.config.now()); err != nil {
		t.Fatal(err)
	}
	if candidates := lookup(first, firstWorker); len(candidates) != 1 || !slices.Equal(candidates[0].Inputs, receipt('b').Inputs) {
		t.Fatalf("rejected path inputs were returned by lookup: %+v", candidates)
	}
	if err := service.expireNativeRecords(t.Context(), time.Now().Add(goTestReceiptRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if candidates := lookup(first, firstWorker); len(candidates) != 0 {
		t.Fatalf("expired candidates = %+v", candidates)
	}
}
