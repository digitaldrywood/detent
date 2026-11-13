package reports

import (
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func ReportsTrackingFingerprints(failures []NativeFailure) map[string]string {
	out := map[string]string{}
	for _, f := range failures {
		class := ClassifyFailure(f.Example).Name
		signature := NormalizeFailureSignature(f.Example)
		for _, fingerprint := range []string{signature, issueorigin.Fingerprint(signature), class, issueorigin.Fingerprint(class)} {
			if _, taken := out[fingerprint]; !taken && fingerprint != "" {
				out[fingerprint] = class
			}
		}
	}
	return out
}
