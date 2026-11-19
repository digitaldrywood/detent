package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func capabilitiesDigest(capabilities nativeCapabilitiesResponse) (string, error) {
	raw, err := json.Marshal(capabilities)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
