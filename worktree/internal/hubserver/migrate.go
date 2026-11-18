package hubserver

import "embed"

//go:embed postgres/migrations/*.sql
var migrationFiles embed.FS
