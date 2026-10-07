package hubserver

import (
	"bytes"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeSourceSurvivesRestartAndPreservesScope(t *testing.T) {
	t.Parallel()
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}
	f := newChangeFixture(t, openTestService(t, config))
	input := changeTestInput()
	bundle := bytes.Repeat([]byte("retained source"), 100000)
	input.Source = &tracker.ChangeSource{Format: "git-bundle", BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, BundleSHA256: tracker.ChangeSourceDigest(bundle), DiffSHA256: strings.Repeat("d", 64), Bytes: int64(len(bundle))}
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source"}, ChangeVersionInput: input, SourceBundle: bundle}
	response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var version tracker.ChangeVersion
	decodeHubResponse(t, response, &version)
	response = performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
	requireNativeStatus(t, response, http.StatusOK)
	var replay tracker.ChangeVersion
	decodeHubResponse(t, response, &replay)
	if replay.ID != version.ID {
		t.Fatal("source replay changed the immutable version")
	}
	for _, statement := range []string{"UPDATE change_sources SET bundle = x'00'", "DELETE FROM change_sources"} {
		if _, err := f.service.database.db.ExecContext(t.Context(), statement); err == nil {
			t.Fatal("source bytes lost version immutability")
		}
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, config)
	path := f.path + "/versions/" + version.ID + "/source"
	response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), bundle) || response.Header().Get("Content-Type") != "application/x-git-bundle" {
		t.Fatal("source bytes were lost across Hub restart")
	}
	other := newNativeFixture(t, f.service, "", "other-source")
	foreignItem := other.create(t, "foreign")
	foreign := other.base + "/work-items/" + string(foreignItem.WorkItemID) + "/changes/" + f.change.ID + "/versions/" + version.ID + "/source"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign, other.token, nil), http.StatusNotFound)
	if detail := f.detail(t); detail.Versions[0].Source == nil || detail.Versions[0].Source.BundleSHA256 != input.Source.BundleSHA256 {
		t.Fatal("metadata read lost the stored source identity")
	}
}
