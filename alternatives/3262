-- +goose Up
-- +goose StatementBegin
CREATE FUNCTION reject_unrecorded_hosted_binding_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.organization_id IS DISTINCT FROM OLD.organization_id
        OR NEW.bootstrap_subject IS DISTINCT FROM OLD.bootstrap_subject
        OR (OLD.provider_id <> '' AND NEW.provider_id IS DISTINCT FROM OLD.provider_id)
        OR ((NEW.public_url IS DISTINCT FROM OLD.public_url OR NEW.deployment IS DISTINCT FROM OLD.deployment OR NEW.allocation_generation IS DISTINCT FROM OLD.allocation_generation)
            AND NOT EXISTS (
                SELECT 1 FROM hosted_binding_migrations m
                WHERE m.organization_id = OLD.organization_id AND NOT m.applied AND NEW.deployment = 'shared'
                    AND m.from_deployment = OLD.deployment AND m.from_public_url = OLD.public_url AND m.from_generation = OLD.allocation_generation
                    AND m.to_public_url = NEW.public_url AND m.to_generation = NEW.allocation_generation
            )) THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'hosted binding changes require a migration record';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE FUNCTION reject_hosted_binding_migration_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE'
        OR NEW.organization_id IS DISTINCT FROM OLD.organization_id OR NEW.id IS DISTINCT FROM OLD.id
        OR NEW.from_public_url IS DISTINCT FROM OLD.from_public_url OR NEW.to_public_url IS DISTINCT FROM OLD.to_public_url
        OR NEW.from_deployment IS DISTINCT FROM OLD.from_deployment
        OR NEW.from_generation IS DISTINCT FROM OLD.from_generation OR NEW.to_generation IS DISTINCT FROM OLD.to_generation
        OR NEW.migrated_at IS DISTINCT FROM OLD.migrated_at OR OLD.applied OR NOT NEW.applied THEN
        RAISE EXCEPTION USING ERRCODE = '23514', MESSAGE = 'hosted binding migration history is immutable';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER hosted_tenant_binding_update BEFORE UPDATE ON hosted_tenant FOR EACH ROW EXECUTE FUNCTION reject_unrecorded_hosted_binding_change();
CREATE TRIGGER hosted_binding_migration_update BEFORE UPDATE OR DELETE ON hosted_binding_migrations FOR EACH ROW EXECUTE FUNCTION reject_hosted_binding_migration_change();

-- +goose Down
DROP TRIGGER hosted_binding_migration_update ON hosted_binding_migrations;
DROP TRIGGER hosted_tenant_binding_update ON hosted_tenant;
DROP FUNCTION reject_hosted_binding_migration_change();
DROP FUNCTION reject_unrecorded_hosted_binding_change();
