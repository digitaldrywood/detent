package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
)

const releaseEvidenceHeaderLimit = 1024 * 1024

type releaseEvidence struct {
	Schema      int    `json:"schema"`
	ArchiveName string `json:"archive_name"`
	Checksums   []byte `json:"checksums"`
	Signature   []byte `json:"signature"`
	Provenance  []byte `json:"provenance"`
}

func writeReleaseEvidence(lockPath string, evidence releaseEvidence, archive []byte) (string, error) {
	header, err := json.Marshal(evidence)
	if err != nil {
		return "", err
	}
	if len(header) > releaseEvidenceHeaderLimit || len(archive) > defaultMaxDownloadBytes {
		return "", errors.New("release verification evidence exceeds artifact limits")
	}
	sum := sha256.Sum256(archive)
	key := hex.EncodeToString(sum[:])
	dir := lockPath + ".releases"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(dir, ".evidence-*")
	if err != nil {
		return "", err
	}
	defer removeFile(file.Name())
	writeErr := binary.Write(file, binary.BigEndian, uint32(len(header)))
	if writeErr == nil {
		_, writeErr = file.Write(header)
	}
	if writeErr == nil {
		_, writeErr = file.Write(archive)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return "", err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, key)); err != nil {
		return "", err
	}
	return key, nil
}

func verifyReleaseEvidence(ctx context.Context, lockPath string, key string, opts DetectionOptions, commit string, digest string) bool {
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	file, err := os.Open(filepath.Join(lockPath+".releases", hex.EncodeToString(decoded)))
	if err != nil {
		return false
	}
	verified := releaseEvidenceMatches(ctx, file, key, opts, commit, digest)
	return file.Close() == nil && verified
}

func releaseEvidenceMatches(ctx context.Context, file *os.File, key string, opts DetectionOptions, commit string, digest string) bool {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > defaultMaxDownloadBytes+releaseEvidenceHeaderLimit+4 {
		return false
	}
	var size uint32
	if binary.Read(file, binary.BigEndian, &size) != nil || size == 0 || size > releaseEvidenceHeaderLimit {
		return false
	}
	header := make([]byte, size)
	if _, err := io.ReadFull(file, header); err != nil {
		return false
	}
	var evidence releaseEvidence
	if json.Unmarshal(header, &evidence) != nil || evidence.Schema != 1 || VerifyChecksumSignature(ctx, evidence.Checksums, evidence.Signature) != nil {
		return false
	}
	tag := "v" + strings.TrimPrefix(opts.CurrentVersion, "v")
	goos := firstNonEmpty(opts.GOOS, runtime.GOOS)
	goarch := firstNonEmpty(opts.GOARCH, runtime.GOARCH)
	if !slices.Contains(archiveAssetNames(tag, goos, goarch), evidence.ArchiveName) || VerifyChecksum(evidence.Checksums, provenanceAssetName, evidence.Provenance) != nil {
		return false
	}
	if _, err := provenance.Parse(evidence.Provenance, releaseRepository, tag, commit); err != nil {
		return false
	}
	offset := int64(size) + 4
	archiveSize := info.Size() - offset
	if archiveSize <= 0 || archiveSize > defaultMaxDownloadBytes {
		return false
	}
	archive, err := io.ReadAll(io.NewSectionReader(file, offset, archiveSize))
	if err != nil {
		return false
	}
	sum := sha256.Sum256(archive)
	actual := hex.EncodeToString(sum[:])
	expected, found := expectedChecksum(evidence.Checksums, evidence.ArchiveName)
	if !found || !strings.EqualFold(actual, expected) || !strings.EqualFold(actual, key) {
		return false
	}
	installed, err := archiveBinaryDigest(bytes.NewReader(archive), int64(len(archive)), goos)
	return err == nil && installed == digest
}

func readerDigest(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func archiveBinaryDigest(archive io.ReaderAt, size int64, goos string) (_ string, err error) {
	if goos == "windows" {
		reader, err := zip.NewReader(archive, size)
		if err != nil {
			return "", err
		}
		for _, file := range reader.File {
			if filepath.Base(file.Name) != windowsExecutableName {
				continue
			}
			binary, err := file.Open()
			if err != nil {
				return "", err
			}
			digest, readErr := readerDigest(binary)
			return digest, errors.Join(readErr, binary.Close())
		}
	} else {
		gz, err := gzip.NewReader(io.NewSectionReader(archive, 0, size))
		if err != nil {
			return "", err
		}
		defer func() { err = errors.Join(err, gz.Close()) }()
		reader := tar.NewReader(gz)
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
			if (header.Typeflag == tar.TypeReg || header.Typeflag == 0) && filepath.Base(header.Name) == projectName {
				return readerDigest(reader)
			}
		}
	}
	return "", fmt.Errorf("verified release archive has no %s binary", projectName)
}
