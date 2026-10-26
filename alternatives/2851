package releaseaudit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Part struct {
	AttachmentID string `json:"attachment_id"`
	Index        int    `json:"index"`
	Offset       int    `json:"offset"`
	Bytes        int    `json:"bytes"`
	Digest       string `json:"digest"`
}

type Checkpoint struct {
	ID           string    `json:"id"`
	Identity     Identity  `json:"identity"`
	Sequence     int       `json:"sequence"`
	Status       string    `json:"status"`
	BundleCommit string    `json:"bundle_commit"`
	BundleDigest string    `json:"bundle_digest"`
	Allowance    Allowance `json:"allowance"`
	Invocations  int       `json:"invocations"`
	Usage        Usage     `json:"usage"`
	Digest       string    `json:"digest"`
	Bytes        int       `json:"bytes"`
	Parts        []Part    `json:"parts"`
}

type CheckpointPage struct {
	Items []Checkpoint `json:"items"`
	Next  string       `json:"next_cursor,omitempty"`
}

func SnapshotBytes(snapshot Snapshot) ([]byte, Checkpoint, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil || len(raw) > maxDocumentBytes {
		return nil, Checkpoint{}, errors.New("release audit snapshot exceeds its allowance")
	}
	id := sha256.Sum256([]byte(strings.Join([]string{snapshot.Identity.ProjectID, snapshot.Identity.RunID, snapshot.Identity.Attempt, snapshot.Identity.JobID, strconv.Itoa(snapshot.Sequence)}, "\x00")))
	digest := sha256.Sum256(raw)
	return raw, Checkpoint{ID: hex.EncodeToString(id[:]), Identity: snapshot.Identity, Sequence: snapshot.Sequence, Status: snapshot.Status, BundleCommit: snapshot.BundleCommit, BundleDigest: snapshot.BundleDigest, Allowance: snapshot.Allowance, Invocations: snapshot.Invocations, Usage: snapshot.Usage, Digest: hex.EncodeToString(digest[:]), Bytes: len(raw)}, nil
}

func SplitDocument(content []byte) ([][]byte, error) {
	if !utf8.Valid(content) || len(content) > maxDocumentBytes {
		return nil, errors.New("release audit document is invalid or exceeds its allowance")
	}
	var parts [][]byte
	for len(content) > 0 {
		end := min(len(content), 32768)
		for !utf8.Valid(content[:end]) {
			end--
		}
		parts = append(parts, content[:end])
		content = content[end:]
	}
	return parts, nil
}

func (c Checkpoint) Valid() bool {
	if c.ID == "" || len(c.ID) != 64 || c.Identity.ProjectID == "" || c.Identity.Repository == "" || len(c.Identity.Commit) != 40 || c.Identity.RunID == "" || c.Identity.Attempt == "" || c.Identity.JobID == "" || !strings.HasPrefix(c.Identity.ReleaseSeries, "release/") || c.Sequence < 1 || !slices.Contains([]string{"incomplete", "failed", "clean", "findings", "needs_validation"}, c.Status) || len(c.BundleCommit) != 40 || len(c.BundleDigest) != 64 || c.Bytes <= 0 || c.Bytes > maxDocumentBytes || len(c.Digest) != 64 || len(c.Parts) < 1 || len(c.Parts) > 161 || c.Allowance.Invocations <= 0 || c.Invocations < 0 || c.Invocations > c.Allowance.Invocations || !validUsage(c.Allowance.Usage) || !validUsage(c.Usage) {
		return false
	}
	id := sha256.Sum256([]byte(strings.Join([]string{c.Identity.ProjectID, c.Identity.RunID, c.Identity.Attempt, c.Identity.JobID, strconv.Itoa(c.Sequence)}, "\x00")))
	if hex.EncodeToString(id[:]) != c.ID {
		return false
	}
	offset := 0
	for index, part := range c.Parts {
		if part.Index != index || part.Offset != offset || part.Bytes < 1 || part.Bytes > 32768 || len(part.Digest) != 64 || !strings.HasPrefix(part.AttachmentID, "att_") {
			return false
		}
		offset += part.Bytes
	}
	return offset == c.Bytes
}
