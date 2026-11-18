-- +goose Up
ALTER TABLE health_findings ADD COLUMN revision bigint NOT NULL DEFAULT 1 CHECK (revision>0);
CREATE INDEX health_fingerprint_recency ON health_findings(organization_id,fingerprint,resolved_at DESC,last_seen_at DESC,id);
GRANT INSERT,UPDATE,DELETE ON health_findings,health_finding_projects,health_finding_issues,health_detector_ticks,health_baseline_unavailable TO detent_runtime;
GRANT USAGE,SELECT ON SEQUENCE health_findings_sequence_seq TO detent_runtime;
-- +goose Down
REVOKE INSERT,DELETE ON health_findings,health_finding_projects,health_finding_issues,health_detector_ticks,health_baseline_unavailable FROM detent_runtime;
DROP INDEX health_fingerprint_recency;
ALTER TABLE health_findings DROP COLUMN revision;
