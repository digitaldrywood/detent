-- name: LockNotificationProject :many
SELECT c.*, (SELECT p.profile='native' FROM projects p WHERE p.organization_id=c.organization_id AND p.id=c.project_id)::boolean AS native
FROM project_event_cursors c
WHERE c.organization_id=$1 AND c.notification_sequence<c.sequence
ORDER BY c.notification_checked_at,c.project_id LIMIT 1 FOR UPDATE OF c;

-- name: NotificationEvents :many
SELECT sqlc.embed(e), coalesce((SELECT c.sequence FROM issue_notification_cursors c WHERE c.organization_id=e.organization_id AND c.work_item_id=e.work_item_id),0)::bigint AS consumed_sequence
FROM collaboration_events e
WHERE e.organization_id=$1 AND e.project_id=$2 AND e.project_sequence>$3
ORDER BY e.project_sequence,e.work_item_id LIMIT 256;

-- name: AdvanceNotificationProject :exec
UPDATE project_event_cursors SET notification_sequence=$3,notification_checked_at=clock_timestamp() WHERE organization_id=$1 AND project_id=$2;

-- name: EnsureNotificationCursor :exec
INSERT INTO issue_notification_cursors(organization_id,work_item_id) VALUES($1,$2) ON CONFLICT DO NOTHING;

-- name: LockNotificationCursor :one
SELECT * FROM issue_notification_cursors WHERE organization_id=$1 AND work_item_id=$2 FOR UPDATE;

-- name: AdvanceNotificationCursor :exec
UPDATE issue_notification_cursors SET sequence=$3,recipient_item=$4,recipient_principal=$5 WHERE organization_id=$1 AND work_item_id=$2;

-- name: NotificationRecipients :many
SELECT m.principal_id AS principal,coalesce(i.actor_kind='human' AND i.actor_principal_id=m.principal_id,false)::boolean AS filer,m.user_id,
(m.active AND m.notify_issues AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>sqlc.arg(now_at)::timestamptz) AND EXISTS(SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=m.organization_id AND g.user_id=m.user_id AND g.project_id=i.project_id))::boolean AS eligible
FROM hosted_members m JOIN api_tokens t ON t.organization_id=m.organization_id AND t.id=m.principal_id JOIN issues i ON i.organization_id=m.organization_id AND i.native_id=sqlc.arg(work_item_id)
WHERE m.organization_id=sqlc.arg(organization_id) AND m.principal_id>sqlc.arg(after_principal)
AND ((i.actor_kind='human' AND i.actor_principal_id=m.principal_id) OR EXISTS(SELECT 1 FROM collaboration_events e WHERE e.organization_id=i.organization_id AND e.work_item_id=i.native_id AND e.type='comment.created' AND e.actor_kind='human' AND e.actor_principal_id=m.principal_id AND e.recorded_at<=sqlc.arg(recorded_at) AND (e.work_item_id<>sqlc.arg(event_item) OR e.sequence<=sqlc.arg(sequence))))
ORDER BY m.principal_id LIMIT 257;

-- name: NotificationAuthor :one
SELECT email FROM hosted_members WHERE organization_id=$1 AND principal_id=$2;

-- name: EnsureIssueSubscription :exec
INSERT INTO issue_subscriptions(organization_id,work_item_id,user_id,token) VALUES($1,$2,$3,$4) ON CONFLICT(organization_id,work_item_id,user_id) DO NOTHING;

-- name: AppendIssueNotification :exec
INSERT INTO issue_notification_notices(organization_id,work_item_id,user_id,event_id,due_at,state_from,state_to,author,comment_id,related,verify,source_item)
SELECT s.organization_id,s.work_item_id,s.user_id,$4,$5,$6,$7,$8,$9,$10,$11,$12 FROM issue_subscriptions s WHERE s.organization_id=$1 AND s.work_item_id=$2 AND s.user_id=$3 AND NOT s.unsubscribed
ON CONFLICT DO NOTHING;

-- name: LockDueNotification :one
SELECT s.* FROM issue_subscriptions s WHERE s.organization_id=$1 AND EXISTS(SELECT 1 FROM issue_notification_notices n WHERE n.organization_id=s.organization_id AND n.work_item_id=s.work_item_id AND n.user_id=s.user_id AND n.due_at<=$2)
ORDER BY s.work_item_id,s.user_id LIMIT 1 FOR UPDATE SKIP LOCKED;

-- name: TakeIssueNotices :many
DELETE FROM issue_notification_notices n WHERE n.organization_id=$1 AND n.work_item_id=$2 AND n.user_id=$3 AND (n.source_item,n.event_id) IN (SELECT q.source_item,q.event_id FROM issue_notification_notices q WHERE q.organization_id=$1 AND q.work_item_id=$2 AND q.user_id=$3 ORDER BY q.due_at,q.source_item,q.event_id LIMIT 1000) RETURNING n.*;

-- name: NotificationRecipient :one
SELECT m.email,i.title,i.number,s.token,o.slug AS organization_slug,p.slug AS project_slug,coalesce(b.verification_state,'')::text AS verification
FROM issue_subscriptions s JOIN hosted_members m ON m.organization_id=s.organization_id AND m.user_id=s.user_id
JOIN api_tokens t ON t.organization_id=m.organization_id AND t.id=m.principal_id
JOIN issues i ON i.organization_id=s.organization_id AND i.native_id=s.work_item_id
JOIN projects p ON p.organization_id=i.organization_id AND p.id=i.project_id
JOIN public.organizations o ON o.organization_id=i.organization_id
LEFT JOIN native_board_items b ON b.organization_id=i.organization_id AND b.project_id=i.project_id AND b.work_item_id=i.native_id
WHERE s.organization_id=$1 AND s.work_item_id=$2 AND s.user_id=$3 AND NOT s.unsubscribed AND m.notify_issues AND m.active AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>$4)
AND EXISTS(SELECT 1 FROM hosted_project_grants g WHERE g.organization_id=m.organization_id AND g.user_id=m.user_id AND g.project_id=i.project_id);

-- name: UnsubscribeIssueNotification :execrows
UPDATE issue_subscriptions SET unsubscribed=true WHERE organization_id=$1 AND token=$2;
