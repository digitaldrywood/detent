package releaseaudit

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
)

func validateResume(request Request) error {
	if request.ResumeKey != "" {
		key, err := hex.DecodeString(request.ResumeKey)
		if err != nil || len(key) != 32 || strings.ToLower(request.ResumeKey) != request.ResumeKey {
			return errors.New("release audit resume requires a SHA-256 contract key")
		}
	}
	if request.Resume == nil {
		return nil
	}
	prior := request.Resume
	if request.ResumeKey == "" || prior.ResumeKey != request.ResumeKey || prior.BaseCommit != request.BaseCommit || prior.Identity != request.Identity || prior.BundleCommit != BundleCommit || prior.BundleDigest != BundleDigest || !slices.Contains([]string{"failed", "incomplete"}, prior.Status) {
		return errors.New("release audit resume does not match its interrupted execution contract")
	}
	if prior.Sequence < 1 || prior.Invocations < 0 || prior.Invocations > request.Allowance.Invocations || !validUsage(prior.Usage) || prior.Usage.Input > request.Allowance.Usage.Input || prior.Usage.Output > request.Allowance.Usage.Output || prior.Usage.Cached > request.Allowance.Usage.Cached {
		return errors.New("release audit resume has invalid accounting")
	}
	var ledger []json.RawMessage
	if json.Unmarshal(prior.Ledger, &ledger) != nil || len(ledger) == 0 {
		return errors.New("release audit resume requires an initialized coverage ledger")
	}
	return nil
}

func (e *execution) restore(ctx context.Context, prior Snapshot) error {
	raw, err := json.Marshal(prior)
	if err != nil || len(raw) > maxDocumentBytes {
		return errors.New("release audit resume exceeds its allowance")
	}
	var restored Snapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		return err
	}
	if err := validateDocument(ctx, e.directory, "ledger", restored.Ledger); err != nil {
		return err
	}
	restored.Status, restored.Reason = "incomplete", ""
	restored.Allowance = e.request.Allowance
	e.state = restored
	for _, evidence := range restored.Evidence {
		if evidence.SessionID != "" {
			e.sessions[evidence.SessionID] = true
		}
	}
	return nil
}
