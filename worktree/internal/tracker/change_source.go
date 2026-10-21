package tracker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const (
	MaxChangeSourceBytes = 20 << 20
	MaxChangeDiffBytes   = 128 << 20
)

type ChangeSource struct {
	Format       string `json:"format"`
	BaseSHA      string `json:"base_sha"`
	HeadSHA      string `json:"head_sha"`
	BundleSHA256 string `json:"bundle_sha256"`
	DiffSHA256   string `json:"diff_sha256"`
	Bytes        int64  `json:"bytes"`
}

type ChangeSourceCapture struct {
	Patch  string       `json:"patch,omitempty"`
	Source ChangeSource `json:"source"`
	Bundle []byte       `json:"bundle"`
}

func ChangeSourceDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (s ChangeSource) Validate(base, head string, bundle []byte) error {
	if s.Format != "git-bundle" || s.BaseSHA != base || s.HeadSHA != head || base == head ||
		s.Bytes <= 0 || s.Bytes > MaxChangeSourceBytes || int64(len(bundle)) != s.Bytes ||
		s.BundleSHA256 != ChangeSourceDigest(bundle) || len(s.DiffSHA256) != 64 {
		return errors.New("retained source does not match the immutable version")
	}
	decoded, err := hex.DecodeString(s.DiffSHA256)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("retained source diff digest is invalid")
	}
	header, _, ok := bytes.Cut(bundle, []byte("\n\n"))
	lines := strings.Split(string(header), "\n")
	if !ok || (lines[0] != "# v2 git bundle" && lines[0] != "# v3 git bundle") {
		return errors.New("retained source requires a Git bundle; recreate it with git bundle create <path> HEAD ^<base>")
	}
	advertised := 0
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "-") {
			id, _, ok := strings.Cut(strings.TrimPrefix(line, "-"), " ")
			if !ok || id != base {
				return errors.New("source bundle prerequisites must be the authorized base; recreate it with git bundle create <path> HEAD ^<base>")
			}
			continue
		}
		if strings.HasPrefix(line, "@") {
			if lines[0] != "# v3 git bundle" || line != "@object-format=sha1" {
				return errors.New("source bundle capability is unsupported; recreate it with git bundle create <path> HEAD ^<base>")
			}
			continue
		}
		id, ref, ok := strings.Cut(line, " ")
		if !ok || id != head || ref == "" || strings.ContainsAny(ref, " \t\r\x00") {
			return errors.New("source bundle must advertise only the reviewed head; recreate it with git bundle create <path> HEAD ^<base>")
		}
		advertised++
	}
	if advertised != 1 {
		return errors.New("source bundle must advertise exactly one reviewed head ref; recreate it with git bundle create <path> HEAD ^<base>")
	}
	return nil
}
