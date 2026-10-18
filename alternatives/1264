package diffbody

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

type Reference struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type File struct {
	Organization string    `json:"organization"`
	Project      string    `json:"project"`
	DiffID       string    `json:"diff_id"`
	Position     int       `json:"position"`
	Patch        string    `json:"patch,omitempty"`
	Body         Reference `json:"body"`
}

type Batch struct {
	Files         []File `json:"files"`
	VacuumPending bool   `json:"vacuum_pending"`
}

func Digest(patch string) string {
	sum := sha256.Sum256([]byte(patch))
	return hex.EncodeToString(sum[:])
}

func New(organization, project, diff string, position int, patch string) Reference {
	digest := Digest(patch)
	return Reference{Key: "orgs/" + organization + "/attempt-diffs/" + project + "/" + diff + "/" + strconv.Itoa(position) + "/" + digest, SHA256: digest, Bytes: int64(len(patch))}
}

func (r Reference) Validate(organization, project string, limit int64) error {
	parts := strings.Split(r.Key, "/")
	digest, err := hex.DecodeString(r.SHA256)
	if err != nil || len(digest) != sha256.Size || r.SHA256 != strings.ToLower(r.SHA256) || r.Bytes <= 0 || r.Bytes > limit || len(parts) != 7 || parts[0] != "orgs" || parts[1] != organization || parts[2] != "attempt-diffs" || parts[3] != project || parts[6] != r.SHA256 {
		return errors.New("invalid diff body reference")
	}
	if !strings.HasPrefix(parts[4], "diff_") || strings.Trim(parts[4], "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-") != "" {
		return errors.New("invalid diff body identity")
	}
	position, err := strconv.Atoi(parts[5])
	if err != nil || position < 0 || strconv.Itoa(position) != parts[5] {
		return errors.New("invalid diff body position")
	}
	return nil
}
