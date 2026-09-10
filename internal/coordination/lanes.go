package coordination

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type LaneWrite struct {
	InstanceIdentity string    `json:"instance_identity"`
	Issue            string    `json:"issue"`
	From             string    `json:"from"`
	To               string    `json:"to"`
	Reason           string    `json:"reason"`
	FenceToken       uint64    `json:"fence_token"`
	WrittenAt        time.Time `json:"written_at"`
}

type laneWrites struct {
	Writers map[string]LaneWrite `json:"writers"`
}

func PublishLaneWrite(ctx context.Context, backend Store, project string, write LaneWrite) error {
	if backend == nil {
		return nil
	}
	if strings.TrimSpace(project) == "" || strings.TrimSpace(write.InstanceIdentity) == "" ||
		strings.TrimSpace(write.Issue) == "" || strings.TrimSpace(write.To) == "" ||
		strings.TrimSpace(write.Reason) == "" || write.FenceToken == 0 || write.WrittenAt.IsZero() {
		return errors.New("lane write identity, transition, fence token and timestamp are required")
	}
	write.WrittenAt = write.WrittenAt.UTC()
	key := laneWriteKey(project, write.Issue)
	for range 16 {
		record, found, err := backend.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("read coordinated lane writes: %w", err)
		}
		writes, err := decodeLaneWrites(record, found)
		if err != nil {
			return err
		}
		previous, exists := writes.Writers[write.InstanceIdentity]
		previous.WrittenAt = previous.WrittenAt.UTC()
		if exists && previous.FenceToken >= write.FenceToken {
			if previous == write {
				return nil
			}
			return errors.New("lane write fence token is stale")
		}
		writes.Writers[write.InstanceIdentity] = write
		value, err := json.Marshal(writes)
		if err != nil {
			return fmt.Errorf("encode coordinated lane write: %w", err)
		}
		if _, swapped, err := backend.CompareAndSwap(ctx, key, record.Version, value); err != nil {
			return fmt.Errorf("publish coordinated lane write: %w", err)
		} else if swapped {
			return nil
		}
	}
	return errors.New("coordinated lane write compare-and-swap limit exceeded")
}

func MatchPeerLaneWrite(ctx context.Context, backend Store, project, instance, issue, to string, observedAt time.Time) (bool, error) {
	if backend == nil {
		return false, nil
	}
	record, found, err := backend.Get(ctx, laneWriteKey(project, issue))
	if err != nil {
		return false, fmt.Errorf("read peer lane writes: %w", err)
	}
	writes, err := decodeLaneWrites(record, found)
	if err != nil {
		return false, err
	}
	for identity, write := range writes.Writers {
		if identity != instance && identity != "" && write.InstanceIdentity == identity && write.Issue == issue &&
			write.FenceToken > 0 && !write.WrittenAt.IsZero() && write.WrittenAt.Before(observedAt) &&
			strings.EqualFold(strings.TrimSpace(write.To), strings.TrimSpace(to)) {
			return true, nil
		}
	}
	return false, nil
}

func decodeLaneWrites(record Record, found bool) (laneWrites, error) {
	writes := laneWrites{Writers: make(map[string]LaneWrite)}
	if found {
		if err := json.Unmarshal(record.Value, &writes); err != nil {
			return laneWrites{}, fmt.Errorf("decode coordinated lane writes: %w", err)
		}
	}
	if writes.Writers == nil {
		writes.Writers = make(map[string]LaneWrite)
	}
	return writes, nil
}

func laneWriteKey(project, issue string) string {
	digest := sha256.Sum256([]byte(project + "\x00" + issue))
	return "lanes/" + hex.EncodeToString(digest[:]) + ".json"
}
