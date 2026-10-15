package tracker

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

const MaxChangeSourceBytes = 20 << 20

type ChangeSource struct {
	Format       string `json:"format"`
	BaseSHA      string `json:"base_sha"`
	HeadSHA      string `json:"head_sha"`
	BundleSHA256 string `json:"bundle_sha256"`
	DiffSHA256   string `json:"diff_sha256"`
	Bytes        int64  `json:"bytes"`
}

type ChangeSourceCapture struct {
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
	return nil
}
