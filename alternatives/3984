package searchstore

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type FingerprintRow struct {
	Digest        string
	Origin        bool
	OriginKind    string
	ReleaseSeries string
}

type FingerprintProbe struct {
	Origin        string
	Defects       []string
	Worker        bool
	ReleaseSeries string
}

func FingerprintDigest(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}

func FingerprintRows(project tracker.ProjectID, body string) []FingerprintRow {
	origin, stamped := issueorigin.Parse(body)
	if !stamped {
		return nil
	}
	series := releaseSeries(project, body)
	rows := []FingerprintRow{{Digest: FingerprintDigest(origin.Fingerprint), Origin: true, OriginKind: origin.Kind, ReleaseSeries: series}}
	for _, key := range issueorigin.DefectKeys(body) {
		digest := FingerprintDigest(key)
		if !slices.ContainsFunc(rows, func(row FingerprintRow) bool { return row.Digest == digest }) {
			rows = append(rows, FingerprintRow{Digest: digest, OriginKind: origin.Kind, ReleaseSeries: series})
		}
	}
	slices.SortFunc(rows, func(a, b FingerprintRow) int { return strings.Compare(a.Digest, b.Digest) })
	return rows
}

func DuplicateFingerprintProbe(project tracker.ProjectID, body string) (FingerprintProbe, bool) {
	origin, stamped := issueorigin.Parse(body)
	if !stamped {
		return FingerprintProbe{}, false
	}
	probe := FingerprintProbe{Origin: FingerprintDigest(origin.Fingerprint), Worker: origin.Kind == "worker", ReleaseSeries: releaseSeries(project, body)}
	probe.Defects = []string{probe.Origin}
	for _, key := range issueorigin.DefectKeys(body) {
		if digest := FingerprintDigest(key); !slices.Contains(probe.Defects, digest) {
			probe.Defects = append(probe.Defects, digest)
		}
	}
	slices.Sort(probe.Defects)
	return probe, true
}

func releaseSeries(project tracker.ProjectID, body string) string {
	if string(project) != issueorigin.DetentCloudProjectID {
		return ""
	}
	return issueorigin.ReleaseStabilizationSeries(body)
}
