package hubserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubserver/runnerfleet"
)

func capabilitiesDigest(capabilities nativeCapabilitiesResponse) (string, error) {
	raw, err := json.Marshal(capabilities)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

const sqliteHeartbeatCursorVersion = 1

type heartbeatPosition = runnerfleet.HeartbeatPosition
