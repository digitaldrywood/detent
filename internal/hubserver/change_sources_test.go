package hubserver

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestChangeSourceSurvivesRestartAndPreservesScope(t *testing.T) {
	t.Parallel()
	config := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db")}
	f := newChangeFixture(t, openTestService(t, config))
	input := changeTestInput()
	bundle := changeTestSource(t, &input, bytes.Repeat([]byte("retained source\n"), 100000))
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source"}, ChangeVersionInput: input, SourceBundle: bundle}
	for _, invalid := range []struct {
		name   string
		bundle []byte
	}{
		{"invalid pack", append(bytes.Clone(bytes.SplitN(bundle, []byte("\n\n"), 2)[0]), []byte("\n\nretained source")...)},
		{"truncated pack", bundle[:len(bundle)-1]},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			refused := request
			metadata := *input.Source
			metadata.BundleSHA256, metadata.Bytes = tracker.ChangeSourceDigest(invalid.bundle), int64(len(invalid.bundle))
			refused.Source, refused.SourceBundle = &metadata, invalid.bundle
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, refused)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			if !bytes.Contains(response.Body.Bytes(), []byte("git bundle create")) || len(f.detail(t).Versions) != 0 {
				t.Fatal("unusable source was retained or lacks actionable refusal guidance")
			}
		})
	}
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

func changeTestSource(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) []byte {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("source fixture git %v: %s: %v", args, output, err)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-b", "main", "--object-format=sha1", "--template=")
	path := filepath.Join(directory, "source.txt")
	if err := os.WriteFile(path, []byte("base source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "base")
	base := git("rev-parse", "HEAD")
	data := []byte(input.HeadSHA + "\n")
	if len(content) > 0 {
		data = content[0]
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "reviewed source")
	head := git("rev-parse", "HEAD")
	capture, err := workspace.CaptureChangeSource(t.Context(), directory, base, head)
	if err != nil {
		t.Fatal(err)
	}
	input.BaseSHA, input.HeadSHA, input.MergeBaseSHA, input.Source = base, head, base, &capture.Source
	return capture.Bundle
}
