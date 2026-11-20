-- +goose Up
SET LOCAL lock_timeout = '5s';
DROP POLICY platform_access ON native_phase_receipts;
REVOKE SELECT, INSERT ON native_phase_receipts FROM detent_platform;
REVOKE USAGE ON SEQUENCE native_phase_receipts_position_seq FROM detent_platform;

-- +goose Down
SET LOCAL lock_timeout = '5s';
GRANT SELECT, INSERT ON native_phase_receipts TO detent_platform;
GRANT USAGE ON SEQUENCE native_phase_receipts_position_seq TO detent_platform;
CREATE POLICY platform_access ON native_phase_receipts TO detent_platform USING (true) WITH CHECK (true);
