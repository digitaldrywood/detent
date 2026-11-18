package schemafingerprint

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

func Versions(versions []int64) string {
	parts := make([]string, len(versions))
	for i, version := range versions {
		parts[i] = strconv.FormatInt(version, 10)
	}
	digest := sha256.Sum256([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(digest[:])
}
