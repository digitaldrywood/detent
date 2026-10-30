package hubserver

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/runnerauth"
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
		{"inflated object budget", withPack(changeTestPack(t, 2, tracker.MaxChangeDiffBytes+1, nil, 1))},
		{"cumulative inflation budget", withPack(changeTestPack(t, 3, 1<<20, bytes.Repeat([]byte("x"), 1<<20), 128))},
		{"delta result budget", withPack(changeTestPack(t, 6, uint64(len(delta)), delta, 1))},
		{"object count budget", withPack(changeTestPack(t, 0, 0, nil, tracker.MaxChangeDiffBytes+1))},
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
	t.Run("occupied verification slot honors cancellation", func(t *testing.T) {
		f.service.sourceVerification <- struct{}{}
		defer func() { <-f.service.sourceVerification }()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := f.service.verifyChangeSource(ctx, *input.Source, bundle); !errors.Is(err, context.Canceled) {
			t.Fatalf("occupied verification slot ignored cancellation: %v", err)
		}
	})
	f.service.sourceVerifier = func(ctx context.Context, source tracker.ChangeSource, bundle []byte) error {
		if _, bounded := ctx.Deadline(); bounded {
			return errors.New("publication added a verification deadline")
		}
		return workspace.VerifyChangeSource(ctx, source, bundle)
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

func changeTestPack(t *testing.T, kind byte, size uint64, content []byte, count uint32) []byte {
	t.Helper()
	var pack bytes.Buffer
	pack.WriteString("PACK")
	pack.Write(binary.BigEndian.AppendUint32(nil, 2))
	pack.Write(binary.BigEndian.AppendUint32(nil, count))
	if kind != 0 {
		for range count {
			remaining := size >> 4
			value := kind<<4 | byte(size&15)
			for remaining != 0 {
				pack.WriteByte(value | 128)
				value, remaining = byte(remaining&127), remaining>>7
			}
			pack.WriteByte(value)
			if kind == 6 {
				pack.WriteByte(1)
			}
			writer := zlib.NewWriter(&pack)
			if _, err := writer.Write(content); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	pack.Write(make([]byte, 20))
	return pack.Bytes()
}

func changeTestSource(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) []byte {
	t.Helper()
	bundle, _ := changeTestSourceCheckout(t, input, content...)
	return bundle
}

func changeTestSourceCheckout(t *testing.T, input *tracker.ChangeVersionInput, content ...[]byte) ([]byte, string) {
	t.Helper()
	directory := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", append([]string{"-C", directory, "-c", "user.name=Source Fixture", "-c", "user.email=source@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + os.DevNull}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("source fixture git %v: %s: %v", args, output, err)
		}
		return string(output)
	}
	git("init", "-b", "main", "--object-format=sha1", "--template=")
	path := filepath.Join(directory, "source.txt")
	if err := os.WriteFile(path, []byte("base source\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-m", "base")
	base := strings.TrimSpace(git("rev-parse", "HEAD"))
	data := []byte(input.HeadSHA + "\n")
	if len(content) > 0 && content[0] != nil {
		data = content[0]
		if err := os.WriteFile(path, append(bytes.Clone(data), []byte("draft source\n")...), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
		git("commit", "-m", "source draft")
	}
	if len(content) == 0 || content[0] != nil {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "source.txt")
	}
	git("commit", "--allow-empty", "-m", "reviewed source")
	head := strings.TrimSpace(git("rev-parse", "HEAD"))
	bundlePath := filepath.Join(directory, "source.bundle")
	git("bundle", "create", bundlePath, "HEAD", "^"+base)
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	diff := git("diff", "--raw", "--no-abbrev", "--no-renames", "--no-ext-diff", "--no-textconv", "-z", base, head, "--")
	input.BaseSHA, input.HeadSHA, input.MergeBaseSHA = base, head, base
	input.Source = &tracker.ChangeSource{
		Format: "git-bundle", BaseSHA: base, HeadSHA: head,
		BundleSHA256: tracker.ChangeSourceDigest(bundle),
		DiffSHA256:   tracker.ChangeSourceDigest([]byte(diff)),
		Bytes:        int64(len(bundle)),
	}
	return bundle, directory
}
