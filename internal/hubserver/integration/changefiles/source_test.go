package changefiles_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/changefiles/sourcetest"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func TestChangeSourceSurvivesRestartAndPreservesScope(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	f := newChangeFixtureOn(t, openHubWith(t, func(config *hubserver.Config) { config.Content = objects.call }))
	input := hubtest.ChangeInput(hubtest.PolicyDescriptor().ID)
	bundle := sourcetest.Source(t, &input, bytes.Repeat([]byte("retained source\n"), 100000))
	request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: "source"}, ChangeVersionInput: input, SourceBundle: bundle}
	header, _, _ := bytes.Cut(bundle, []byte("\n\n"))
	withPack := func(pack []byte) []byte {
		return append(append(bytes.Clone(header), '\n', '\n'), pack...)
	}
	delta := binary.AppendUvarint([]byte{1}, tracker.MaxChangeDiffBytes+1)
	for _, invalid := range []struct {
		name   string
		bundle []byte
	}{
		{"invalid pack", withPack([]byte("retained source"))},
		{"truncated pack", bundle[:len(bundle)-1]},
		{"inflated object budget", withPack(sourcetest.Pack(t, 2, tracker.MaxChangeDiffBytes+1, nil, 1))},
		{"cumulative inflation budget", withPack(sourcetest.Pack(t, 3, 1<<20, bytes.Repeat([]byte("x"), 1<<20), 128))},
		{"delta result budget", withPack(sourcetest.Pack(t, 6, uint64(len(delta)), delta, 1))},
		{"object count budget", withPack(sourcetest.Pack(t, 0, 0, nil, tracker.MaxChangeDiffBytes+1))},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			refused := request
			metadata := *input.Source
			metadata.BundleSHA256, metadata.Bytes = tracker.ChangeSourceDigest(invalid.bundle), int64(len(invalid.bundle))
			refused.Source, refused.SourceBundle = &metadata, invalid.bundle
			err := workspace.VerifyChangeSource(t.Context(), metadata, invalid.bundle)
			if err == nil || !strings.Contains(err.Error(), "git bundle create") {
				t.Fatalf("unusable source lacks actionable verifier guidance: %v", err)
			}
			if strings.Contains(invalid.name, "budget") && !strings.Contains(err.Error(), "verification budget") {
				t.Fatalf("pack reached Git before the verification budget refused it: %v", err)
			}
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, refused)
			requireNativeStatus(t, response, http.StatusUnprocessableEntity)
			var failure struct {
				Code    string `json:"code"`
				Details struct {
					Rejection runnerauth.PublicationRejection `json:"rejection"`
				} `json:"details"`
			}
			decodeHubResponse(t, response, &failure)
			if failure.Code != "invalid_request" || failure.Details.Rejection != (runnerauth.PublicationRejection{Category: "source_verification", Field: "source"}) {
				t.Fatalf("unusable source lacks typed refusal details: %s", response.Body.Bytes())
			}
			if len(f.detail(t).Versions) != 0 {
				t.Fatal("unusable source was retained")
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
	admin := f.admin
	for _, statement := range []string{"UPDATE change_versions SET source_content_bytes=source_content_bytes+1 WHERE id=$1", "DELETE FROM change_versions WHERE id=$1"} {
		if _, err := admin.ExecContext(t.Context(), statement, version.ID); err == nil {
			t.Fatal("source bytes lost version immutability")
		}
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = f.hub.reopen(t)
	path := f.path + "/versions/" + version.ID + "/source"
	response = performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), bundle) || response.Header().Get("Content-Type") != "application/x-git-bundle" {
		t.Fatal("source bytes were lost across Hub restart")
	}
	other := f.hub.fixture(t, f.hub.organization(t))
	foreignItem := other.create(t, "foreign")
	foreign := other.base + "/work-items/" + string(foreignItem.WorkItemID) + "/changes/" + f.change.ID + "/versions/" + version.ID + "/source"
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, foreign, other.token, nil), http.StatusNotFound)
	if detail := f.detail(t); detail.Versions[0].Source == nil || detail.Versions[0].Source.BundleSHA256 != input.Source.BundleSHA256 {
		t.Fatal("metadata read lost the stored source identity")
	}
}

type changeObjectFixture struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *changeObjectFixture) call(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Reference.Validate(request.Organization); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	if request.Write {
		if int64(len(request.Data)) != request.Reference.CompressedSize || artifact.Digest(request.Data) != request.Reference.SHA256 {
			return nil, artifact.ErrIntegrity
		}
		if previous, found := f.objects[request.Reference.Key]; found && !bytes.Equal(previous, request.Data) {
			return nil, artifact.ErrIntegrity
		}
		f.objects[request.Reference.Key] = bytes.Clone(request.Data)
		return nil, nil
	}
	data, found := f.objects[request.Reference.Key]
	if !found {
		return nil, artifact.ErrStorage
	}
	return bytes.Clone(data), nil
}
