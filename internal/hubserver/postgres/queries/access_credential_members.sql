-- name: AccessCredentialHasMember :one
SELECT EXISTS(SELECT 1 FROM hosted_members WHERE organization_id=$1 AND principal_id=$2);
