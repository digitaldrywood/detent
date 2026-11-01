-- +goose Up
CREATE TABLE platform.administration_receipts (
    subject TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    retry_identity TEXT NOT NULL,
    input_hash TEXT NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('pending','succeeded','failed','uncertain')),
    resource_id TEXT NOT NULL,
    url TEXT NOT NULL,
    reconnect BOOLEAN NOT NULL,
    sign_out BOOLEAN NOT NULL,
    signed_out BOOLEAN NOT NULL,
    provider_confirmed BOOLEAN NOT NULL,
    audit_recorded BOOLEAN NOT NULL,
    output_object_key TEXT NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY(subject, organization_id, retry_identity)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON platform.administration_receipts TO detent_platform;
CREATE INDEX archive_binding_origins ON platform.archive_storage_bindings(organization_id,lower(endpoint));
CREATE UNIQUE INDEX audit_retry_identity ON platform.audit(subject,organization_id,event,retry_identity) WHERE retry_identity IS NOT NULL;
CREATE TABLE platform.organization_slug_counters (
    base TEXT PRIMARY KEY,
    suffix BIGINT NOT NULL CHECK(suffix > 0)
);
GRANT SELECT, INSERT, UPDATE, DELETE ON platform.organization_slug_counters TO detent_platform;

-- +goose StatementBegin
CREATE FUNCTION platform.assign_organization_slug() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    base TEXT;
    candidate TEXT;
    allocated BIGINT;
BEGIN
    IF NEW.slug <> '' THEN
        RETURN NULL;
    END IF;
    base := trim(both '-' from left(regexp_replace(lower(NEW.name), '[^a-z0-9]+', '-', 'g'), 60));
    IF base = '' THEN
        base := 'organization-organization';
    ELSIF base = ANY(ARRAY['work','settings','login','projects','organizations','organization','api','app','auth','chat','reports','activity','diagnostics','fleet','support','invite','logout','health','metrics','static','webhooks','design-system','setup','usage','billing','account','platform']) THEN
        base := base || '-organization';
    END IF;
    INSERT INTO platform.organization_slug_counters(base,suffix) VALUES(base,1)
    ON CONFLICT ON CONSTRAINT organization_slug_counters_pkey DO UPDATE SET suffix=platform.organization_slug_counters.suffix+1
    RETURNING suffix INTO allocated;
    candidate := base || CASE WHEN allocated=1 THEN '' ELSE '-' || allocated::text END;
    INSERT INTO platform.organization_slugs(slug,organization_id) VALUES(candidate,NEW.id) ON CONFLICT DO NOTHING;
    IF NOT FOUND THEN
        candidate := left(base,14) || '-' || replace(lower(NEW.id),'_','-');
        INSERT INTO platform.organization_slugs(slug,organization_id) VALUES(candidate,NEW.id);
    END IF;
    UPDATE platform.organizations SET slug=candidate WHERE id=NEW.id;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER organizations_generate_slug AFTER INSERT ON platform.organizations FOR EACH ROW EXECUTE FUNCTION platform.assign_organization_slug();
REVOKE ALL ON FUNCTION platform.assign_organization_slug() FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION platform.preserve_organization_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'organization records are retained' USING ERRCODE = '23514';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR (OLD.provider_id <> '' AND NEW.provider_id IS DISTINCT FROM OLD.provider_id)
       OR NEW.generation < OLD.generation
       OR NEW.creator_subject IS DISTINCT FROM OLD.creator_subject
       OR (OLD.state = 'deleted' AND NEW.state <> 'deleted') THEN
        RAISE EXCEPTION 'organization identity is immutable' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER organization_identity BEFORE UPDATE OR DELETE ON platform.organizations FOR EACH ROW EXECUTE FUNCTION platform.preserve_organization_identity();

-- +goose StatementBegin
CREATE FUNCTION platform.preserve_entry_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'entry history is immutable' USING ERRCODE = '23514';
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER platform_member_history BEFORE UPDATE OR DELETE ON platform.platform_member_changes FOR EACH ROW EXECUTE FUNCTION platform.preserve_entry_history();
CREATE TRIGGER entitlement_history BEFORE UPDATE OR DELETE ON platform.entitlement_changes FOR EACH ROW EXECUTE FUNCTION platform.preserve_entry_history();
CREATE TRIGGER billing_customer_history BEFORE UPDATE OR DELETE ON platform.billing_customers FOR EACH ROW EXECUTE FUNCTION platform.preserve_entry_history();
CREATE TRIGGER archive_binding_identity BEFORE UPDATE ON platform.archive_storage_bindings FOR EACH ROW EXECUTE FUNCTION platform.preserve_entry_history();
REVOKE ALL ON FUNCTION platform.preserve_organization_identity() FROM PUBLIC;
REVOKE ALL ON FUNCTION platform.preserve_entry_history() FROM PUBLIC;

-- +goose Down
DROP TRIGGER organizations_generate_slug ON platform.organizations;
DROP FUNCTION platform.assign_organization_slug();
DROP TABLE platform.organization_slug_counters;
DROP TRIGGER archive_binding_identity ON platform.archive_storage_bindings;
DROP TRIGGER billing_customer_history ON platform.billing_customers;
DROP TRIGGER entitlement_history ON platform.entitlement_changes;
DROP TRIGGER platform_member_history ON platform.platform_member_changes;
DROP TRIGGER organization_identity ON platform.organizations;
DROP FUNCTION platform.preserve_entry_history();
DROP FUNCTION platform.preserve_organization_identity();
DROP INDEX platform.archive_binding_origins;
DROP INDEX platform.audit_retry_identity;
DROP TABLE platform.administration_receipts;
