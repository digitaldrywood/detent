-- name: RecordCurrentChangeReviewSequences :many
WITH selected AS MATERIALIZED (
 SELECT current.evidence_sequence FROM (VALUES ('approved'),('changes_requested')) decisions(decision)
 CROSS JOIN LATERAL (
  SELECT h.evidence_sequence FROM current_change_reviews h
  WHERE h.organization_id=sqlc.arg(organization_id) AND h.change_id=sqlc.arg(change_id) AND h.version_id=sqlc.arg(version_id)
  AND h.review_kind='human' AND h.decision=decisions.decision AND h.actor_key<>sqlc.arg(reviewing_actor)
  ORDER BY h.evidence_sequence DESC LIMIT 1
 ) current
 UNION ALL
 SELECT latest.sequence FROM (VALUES ('validator'),('implementation'),('security_audit')) roles(review_kind)
 CROSS JOIN LATERAL (
  SELECT e.sequence FROM change_evidence e
  WHERE e.organization_id=sqlc.arg(organization_id) AND e.change_id=sqlc.arg(change_id) AND e.version_id=sqlc.arg(version_id)
  AND e.kind='review' AND NOT e.review_landing AND e.review_kind=roles.review_kind AND e.sequence<=sqlc.arg(through)
  ORDER BY e.sequence DESC LIMIT 1
 ) latest
 UNION ALL
 SELECT h.evidence_sequence FROM current_change_reviews h
 WHERE h.organization_id=sqlc.arg(organization_id) AND h.change_id=sqlc.arg(change_id) AND h.version_id=sqlc.arg(version_id)
 AND h.review_kind='security_audit_failure' AND h.actor_key=''
)
SELECT DISTINCT evidence_sequence FROM selected ORDER BY evidence_sequence;

-- name: RecordCurrentChangeEvidenceBySequences :many
SELECT * FROM change_evidence WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id)
AND sequence=ANY(sqlc.arg(sequences)::bigint[]) ORDER BY sequence;

-- name: RecordCurrentChangeReviewChecks :many
SELECT * FROM change_evidence
WHERE organization_id=sqlc.arg(organization_id) AND change_id=sqlc.arg(change_id) AND version_id=sqlc.arg(version_id)
AND kind='check' AND source_key=ANY(sqlc.arg(check_ids)::text[]) ORDER BY sequence;

-- name: RecordCurrentChangePriorApproval :one
SELECT EXISTS (SELECT 1 WHERE EXISTS (SELECT 1 FROM current_change_reviews h
WHERE h.organization_id=sqlc.arg(organization_id) AND h.change_id=sqlc.arg(change_id) AND h.version_id<sqlc.arg(version_id)
AND h.review_kind='human_prior_approval') OR EXISTS (SELECT 1 FROM current_change_reviews h
WHERE h.organization_id=sqlc.arg(organization_id) AND h.change_id=sqlc.arg(change_id) AND h.version_id>sqlc.arg(version_id)
AND h.review_kind='human_prior_approval')) AS approved;

-- name: RecordPendingChangeReviewSummary :one
SELECT * FROM change_requests WHERE organization_id=sqlc.arg(organization_id) AND review_summary_sequence<evidence_sequence AND id>sqlc.arg(after_id)
ORDER BY id LIMIT 1;
