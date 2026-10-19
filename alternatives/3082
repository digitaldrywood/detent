package update

import (
	"context"
	"encoding/hex"
	"errors"
)

func VerifyPublishedRelease(ctx context.Context, client ReleaseClient, release Release, platform, architecture string, verify ChecksumSignatureVerifier) error {
	assets, err := SelectReleaseAssets(release, platform, architecture)
	if err != nil {
		return err
	}
	if assets.Archive.BrowserDownloadURL == "" || assets.Provenance.BrowserDownloadURL == "" {
		return errors.New("release assets have no download URL")
	}
	checksums, err := client.Download(ctx, assets.Checksum.BrowserDownloadURL)
	if err != nil {
		return err
	}
	signature, err := client.Download(ctx, assets.ChecksumSignature.BrowserDownloadURL)
	if err != nil {
		return err
	}
	if err := verify(ctx, checksums, signature); err != nil {
		return err
	}
	hash, found := expectedChecksum(checksums, assets.Archive.Name)
	digest, err := hex.DecodeString(hash)
	if !found || err != nil || len(digest) != 32 {
		return errors.New("release archive is absent from the signed checksum manifest")
	}
	provenance, err := client.Download(ctx, assets.Provenance.BrowserDownloadURL)
	if err != nil {
		return err
	}
	return VerifyChecksum(checksums, assets.Provenance.Name, provenance)
}
