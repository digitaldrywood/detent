// Package cloudorigin names the shared hosted product's environment-specific
// origins. These aliases never apply to customer-operated Hubs.
package cloudorigin

const (
	Production = "https://cloud.detent.build"
	Staging    = "https://staging.cloud.detent.build"
)

func Legacy(canonical string) string {
	switch canonical {
	case Production:
		return "https://hub.detent.build"
	case Staging:
		return "https://staging.hub.detent.build"
	default:
		return ""
	}
}

// Aliases reports whether two different origins name the same hosted service,
// including rollback. It never equates production with staging.
func Aliases(from, to string) bool {
	return from != "" && to != "" && (Legacy(from) == to || Legacy(to) == from)
}
