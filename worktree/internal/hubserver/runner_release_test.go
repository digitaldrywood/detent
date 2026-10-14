package hubserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"

	"github.com/digitaldrywood/detent/internal/update"
)

type runnerReleaseFixture struct {
	release update.Release
	files   map[string][]byte
	reads   int
}

func (f *runnerReleaseFixture) ListReleases(context.Context) ([]update.Release, error) {
	return nil, errors.New("heartbeat must select an exact release")
}

func (f *runnerReleaseFixture) GetRelease(context.Context, string) (update.Release, error) {
	f.reads++
	return f.release, nil
}

func (f *runnerReleaseFixture) Download(_ context.Context, name string) ([]byte, error) {
	if data, ok := f.files[name]; ok {
		return data, nil
	}
	return nil, errors.New("asset unavailable")
}

func TestHeartbeatReleasePublication(t *testing.T) {
	for _, test := range []struct {
		name                                                                                   string
		draft, missingArchive, missingSignature, badSignature, badChecksum, missingArchiveHash bool
		want                                                                                   bool
	}{
		{name: "signed published release", want: true},
		{name: "unpublished"},
		{name: "draft", draft: true},
		{name: "missing platform archive", missingArchive: true},
		{name: "missing signature", missingSignature: true},
		{name: "invalid signature", badSignature: true},
		{name: "invalid provenance checksum", badChecksum: true},
		{name: "archive missing from signed manifest", missingArchiveHash: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &runnerReleaseFixture{release: update.Release{TagName: "v1.2.4", Draft: test.draft}, files: map[string][]byte{}}
			for _, name := range []string{"detent_1.2.4_linux_amd64.tar.gz", "checksums.txt", "checksums.txt.minisig", "detent_release_provenance.json"} {
				if test.missingArchive && name == "detent_1.2.4_linux_amd64.tar.gz" || test.missingSignature && name == "checksums.txt.minisig" || test.name == "unpublished" {
					continue
				}
				client.release.Assets = append(client.release.Assets, update.Asset{Name: name, BrowserDownloadURL: name})
			}
			provenance := []byte("provenance")
			client.files["detent_release_provenance.json"] = provenance
			client.files["checksums.txt"] = fmt.Appendf(nil, "%x  detent_release_provenance.json\n", sha256.Sum256(provenance))
			if !test.missingArchiveHash {
				client.files["checksums.txt"] = fmt.Appendf(client.files["checksums.txt"], "%x  detent_1.2.4_linux_amd64.tar.gz\n", sha256.Sum256([]byte("archive")))
			}
			client.files["checksums.txt.minisig"] = []byte("fixture signature")
			if test.badChecksum {
				client.files["detent_release_provenance.json"] = []byte("tampered")
			}
			service := &Service{config: Config{RunnerReleaseClient: client, runnerReleaseSignatureVerifier: func(context.Context, []byte, []byte) error {
				if test.badSignature {
					return errors.New("invalid signature")
				}
				return nil
			}}.normalized()}
			if got := service.runnerReleasePublished(t.Context(), "1.2.4", "linux", "amd64"); got != test.want {
				t.Fatalf("published = %t, want %t", got, test.want)
			}
			if test.want {
				client.release = update.Release{}
				if !service.runnerReleasePublished(t.Context(), "1.2.4", "linux", "amd64") || client.reads != 1 {
					t.Fatal("published target was not cached")
				}
			}
		})
	}
}
