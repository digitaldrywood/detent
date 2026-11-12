package runnerupdate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerupdate/updatetest"
	"github.com/digitaldrywood/detent/internal/update"
)

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
			client := &updatetest.ReleaseClient{Release: update.Release{TagName: "v1.2.4", Draft: test.draft}, Files: map[string][]byte{}}
			for _, name := range []string{"detent_1.2.4_linux_amd64.tar.gz", "checksums.txt", "checksums.txt.minisig", "detent_release_provenance.json"} {
				if test.missingArchive && name == "detent_1.2.4_linux_amd64.tar.gz" || test.missingSignature && name == "checksums.txt.minisig" || test.name == "unpublished" {
					continue
				}
				client.Release.Assets = append(client.Release.Assets, update.Asset{Name: name, BrowserDownloadURL: name})
			}
			provenance := []byte("provenance")
			client.Files["detent_release_provenance.json"] = provenance
			client.Files["checksums.txt"] = fmt.Appendf(nil, "%x  detent_release_provenance.json\n", sha256.Sum256(provenance))
			if !test.missingArchiveHash {
				client.Files["checksums.txt"] = fmt.Appendf(client.Files["checksums.txt"], "%x  detent_1.2.4_linux_amd64.tar.gz\n", sha256.Sum256([]byte("archive")))
			}
			client.Files["checksums.txt.minisig"] = []byte("fixture signature")
			if test.badChecksum {
				client.Files["detent_release_provenance.json"] = []byte("tampered")
			}
			service := New(Deps{ReleaseClient: client, Published: &sync.Map{}, VerifySignature: func(context.Context, []byte, []byte) error {
				if test.badSignature {
					return errors.New("invalid signature")
				}
				return nil
			}})
			if got := service.ReleasePublished(t.Context(), "1.2.4", "linux", "amd64"); got != test.want {
				t.Fatalf("published = %t, want %t", got, test.want)
			}
			if test.want {
				client.Release = update.Release{}
				if !service.ReleasePublished(t.Context(), "1.2.4", "linux", "amd64") || client.Reads != 1 {
					t.Fatal("published target was not cached")
				}
			}
		})
	}
}
