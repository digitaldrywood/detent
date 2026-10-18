-- +goose Up
CREATE TABLE organization_sprite_pool_cutover_bounds (
 organization_id TEXT PRIMARY KEY,
 min_runners INTEGER NOT NULL,
 max_runners INTEGER NOT NULL,
 revision INTEGER NOT NULL
);

-- +goose StatementBegin
CREATE TRIGGER preserve_sprite_pool_cutover_bounds AFTER INSERT ON organization_sprite_pools
BEGIN
 INSERT INTO organization_sprite_pool_cutover_bounds
 SELECT NEW.organization_id,sum(min_runners),sum(max_runners),NEW.revision
 FROM project_sprite_pools WHERE organization_id=NEW.organization_id
 HAVING count(*)>0;
END;
-- +goose StatementEnd
