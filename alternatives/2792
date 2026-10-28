package securityaudit

import "github.com/digitaldrywood/detent/internal/failureattribution"

type CheckComparison struct {
	Head failureattribution.Observation `json:"head"`
	Base failureattribution.Observation `json:"base"`
}

func AttributeFindings(base, head string, findings []Finding, checks []CheckComparison) string {
	values := make([]string, 0, len(findings))
	for i := range findings {
		finding := &findings[i]
		finding.Attribution, finding.AttributionEvidence = failureattribution.Unknown, nil
		for j := len(checks) - 1; j >= 0; j-- {
			check := checks[j]
			if check.Head.Command != finding.CheckCommand {
				continue
			}
			finding.Attribution, finding.AttributionEvidence = failureattribution.Classify(base, head, finding.CheckDiagnostic, check.Head, check.Base)
			break
		}
		values = append(values, finding.Attribution)
	}
	return failureattribution.Aggregate(values)
}

func NormalizeAttribution(run *Run) {
	run.Attribution = AttributeFindings(run.BaseSHA, run.HeadSHA, run.Findings, run.Checks)
}
