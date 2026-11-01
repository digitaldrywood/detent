-- +goose Up
DROP TRIGGER preserve_sprite_pool_cutover_bounds;
UPDATE organization_sprite_pools AS p
SET min_runners=b.min_runners,max_runners=b.max_runners,revision=p.revision+1
FROM organization_sprite_pool_cutover_bounds AS b
WHERE p.organization_id=b.organization_id AND p.revision=b.revision
 AND (p.min_runners<>b.min_runners OR p.max_runners<>b.max_runners);
DROP TABLE organization_sprite_pool_cutover_bounds;
