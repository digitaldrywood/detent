package credits

import (
	"context"
	"encoding/csv"
	"math"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
)

func (s *Service) Export(ctx context.Context, from, to int64, start func() (*csv.Writer, error)) (bool, error) {
	const pageSize = 100
	var snapshot int64
	beforeAt, beforeID := time.UnixMicro(math.MaxInt64), int64(math.MaxInt64)
	var writer *csv.Writer
	for {
		var rows []billingstore.CreditExportEntry
		err := s.read(ctx, func(reader tenantstore.Reader) error {
			var err error
			snapshot, rows, err = reader.BillingCreditExport(ctx, s.deps.Mode, snapshot, from, to, beforeAt, beforeID, pageSize)
			return err
		})
		if err != nil {
			if writer == nil {
				return false, err
			}
			return true, err
		}
		if writer == nil {
			writer, err = start()
			if err != nil {
				return true, err
			}
		}
		for _, row := range rows {
			if err := WriteCSVEntry(writer, row); err != nil {
				return true, err
			}
			beforeAt, beforeID = row.RecordedAt, row.ID
		}
		if len(rows) < pageSize {
			break
		}
	}
	writer.Flush()
	return true, writer.Error()
}
