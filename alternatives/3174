package hubserver

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"

	"modernc.org/sqlite"

	"github.com/digitaldrywood/detent/internal/attachment"
)

var registerStorageConversionDigest = sync.OnceValue(func() error {
	return sqlite.RegisterDeterministicScalarFunction("storage_conversion_digest", -1, func(_ *sqlite.FunctionContext, values []driver.Value) (driver.Value, error) {
		return storageConversionDigest(values...), nil
	})
})

func storageConversionDigest(values ...driver.Value) string {
	var encoded []byte
	for _, value := range values {
		var data []byte
		switch value := value.(type) {
		case string:
			data = []byte(value)
		case []byte:
			data = value
		case int64:
			data = strconv.AppendInt(nil, value, 10)
		default:
			return ""
		}
		encoded = strconv.AppendInt(encoded, int64(len(data)), 10)
		encoded = append(encoded, ':')
		encoded = append(encoded, data...)
	}
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func authorizeStorageConversion(ctx context.Context, tx *sql.Tx, scope nativeScope, family storageConversionFamily, aggregate string, row int64, raw, converted []byte, ref attachment.ArchiveReference) error {
	switch family.name {
	case "runs", "versions", "evidence", "sources":
	default:
		return nil
	}
	before, after := storageConversionDigest(raw), storageConversionDigest(converted)
	if family.name == "runs" {
		before = storageConversionDigest(raw, int64(0), "", "", "", "", int64(0), int64(0))
		after = storageConversionDigest(converted, int64(1), "", "", ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize)
	}
	if family.name == "versions" || family.name == "evidence" {
		before, after = storageConversionDigest(raw, ""), storageConversionDigest(converted, "")
	}
	if family.name == "sources" {
		before = storageConversionDigest(raw, "", "", int64(0), int64(0))
		after = storageConversionDigest([]byte{}, ref.Key, ref.SHA256, ref.CompressedSize, ref.UncompressedSize)
	}
	var policyID string
	if family.name == "versions" {
		var version struct {
			PolicyID string `json:"policy_id"`
		}
		if err := json.Unmarshal(raw, &version); err != nil {
			return err
		}
		policyID = version.PolicyID
	}
	_, err := tx.ExecContext(ctx, `UPDATE storage_conversion_progress SET conversion_rowid=?,conversion_before_sha256=?,conversion_after_sha256=?,conversion_policy_revision_id=? WHERE organization_id=? AND project_id=? AND family=? AND aggregate_id=? AND format_version=?`, row, before, after, policyID, scope.organization, scope.project, family.name, aggregate, storageConversionVersion)
	return err
}
