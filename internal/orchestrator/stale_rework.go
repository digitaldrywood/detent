package orchestrator

type staleReworkEvidence struct {
	ChangeID        string   `json:"change_id"`
	VersionID       string   `json:"version_id"`
	HeadSHA         string   `json:"head_sha"`
	BaseSHA         string   `json:"base_sha"`
	BaseFingerprint string   `json:"base_fingerprint,omitempty"`
	Findings        []string `json:"findings,omitempty"`
}

func staleReworkPark(park workflowLaneBlockedRecoveryMetadata) bool {
	return park.Cause == terminalAttemptRetryLimitCause && park.StaleRework != nil && park.Owner == blockedRecoveryOwnerOperator
}
