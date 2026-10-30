package hubserver

import (
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

	if err := service.expireNativeRecords(t.Context(), time.Now().Add(goTestReceiptRetention+time.Hour)); err != nil {
		t.Fatal(err)
	}
	if candidates := lookup(first, firstWorker); len(candidates) != 0 {
		t.Fatalf("expired candidates = %+v", candidates)
	}
}
