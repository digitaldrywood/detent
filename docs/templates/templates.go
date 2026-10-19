package templates

import _ "embed"

// BlockedHandoff is the canonical worker status contract. The runner replaces
// completion_fields with the current attempt's completion identity.
//
//go:embed blocked-handoff.md
var BlockedHandoff string
