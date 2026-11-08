package postgres

const costImportPageLimit = 100

var costImportColumns = []string{"organization_id", "provider", "provider_account", "source_id", "period_start", "period_end", "revision", "project_id", "resource_id", "bucket", "metric", "received_at", "voided", "observation_json"}

type costImportPageRow struct {
	key    [7]string
	record costImportRecord
}
