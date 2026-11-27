-- +goose Up
ALTER TABLE runner_update_requests
 DROP CONSTRAINT runner_update_automatic_offer,
 ADD COLUMN automatic_offer_withdrawn boolean NOT NULL DEFAULT false,
 ADD CONSTRAINT runner_update_automatic_offer CHECK (
  (automatic_offer_slot IS NULL OR (automatic_offer_slot BETWEEN 0 AND 7 AND
   automatic_organization_revision >= 0 AND automatic_runner_revision >= 0 AND runner_id IS NOT NULL AND NOT urgent))
  AND (NOT automatic_offer_withdrawn OR (follow_hub AND automatic_offer_slot IS NOT NULL))
 );

-- +goose Down
ALTER TABLE runner_update_requests
 DROP CONSTRAINT runner_update_automatic_offer,
 DROP COLUMN automatic_offer_withdrawn;
UPDATE runner_update_requests SET automatic_offer_slot=NULL WHERE follow_hub AND automatic_offer_slot IS NOT NULL;
ALTER TABLE runner_update_requests ADD CONSTRAINT runner_update_automatic_offer CHECK (
 automatic_offer_slot IS NULL OR (automatic_offer_slot BETWEEN 0 AND 7 AND
  automatic_organization_revision >= 0 AND automatic_runner_revision >= 0 AND runner_id IS NOT NULL AND NOT urgent AND NOT follow_hub)
 );
