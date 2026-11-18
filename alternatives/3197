-- +goose Up
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_project_configurations
 ADD COLUMN request_id text NOT NULL DEFAULT '',
 ADD COLUMN request_operation text NOT NULL DEFAULT '',
 ADD COLUMN request_actor_id text NOT NULL DEFAULT '',
 ADD COLUMN request_hash text NOT NULL DEFAULT '',
 ADD COLUMN request_runner_revision bigint NOT NULL DEFAULT 0,
 ADD COLUMN request_status text NOT NULL DEFAULT '',
 ADD COLUMN request_payload_object_key text NOT NULL DEFAULT '',
 ADD COLUMN request_payload_sha256 text NOT NULL DEFAULT '',
 ADD COLUMN request_payload_bytes bigint NOT NULL DEFAULT 0,
 ADD COLUMN request_payload_content_bytes bigint NOT NULL DEFAULT 0,
 ADD COLUMN observation_payload_observed_at timestamptz,
 ADD COLUMN observation_payload_object_key text NOT NULL DEFAULT '',
 ADD COLUMN observation_payload_sha256 text NOT NULL DEFAULT '',
 ADD COLUMN observation_payload_bytes bigint NOT NULL DEFAULT 0,
 ADD COLUMN observation_payload_content_bytes bigint NOT NULL DEFAULT 0,
 ADD CONSTRAINT runner_configuration_request_status CHECK(request_status IN ('','pending','completed')) NOT VALID,
 ADD CONSTRAINT runner_configuration_request_payload CHECK ((request_id='' AND request_status='' AND request_payload_object_key='' AND request_runner_revision=0) OR (request_id<>'' AND request_operation<>'' AND request_actor_id<>'' AND length(request_hash)=64 AND request_runner_revision>0 AND request_status<>'' AND request_payload_object_key<>'' AND length(request_payload_sha256)=64 AND request_payload_bytes>0 AND request_payload_content_bytes>0)) NOT VALID,
 ADD CONSTRAINT runner_configuration_observation_payload CHECK ((observation_payload_object_key='' AND observation_payload_sha256='' AND observation_payload_bytes=0 AND observation_payload_content_bytes=0) OR (observation_payload_object_key<>'' AND length(observation_payload_sha256)=64 AND observation_payload_bytes>0 AND observation_payload_content_bytes>0)) NOT VALID;

-- +goose Down
SET LOCAL lock_timeout = '5s';
ALTER TABLE runner_project_configurations
 DROP CONSTRAINT runner_configuration_request_status,
 DROP CONSTRAINT runner_configuration_observation_payload,
 DROP CONSTRAINT runner_configuration_request_payload,
 DROP COLUMN observation_payload_observed_at,
 DROP COLUMN observation_payload_content_bytes,
 DROP COLUMN observation_payload_bytes,
 DROP COLUMN observation_payload_sha256,
 DROP COLUMN observation_payload_object_key,
 DROP COLUMN request_payload_content_bytes,
 DROP COLUMN request_payload_bytes,
 DROP COLUMN request_payload_sha256,
 DROP COLUMN request_payload_object_key,
 DROP COLUMN request_status,
 DROP COLUMN request_runner_revision,
 DROP COLUMN request_hash,
 DROP COLUMN request_actor_id,
 DROP COLUMN request_operation,
 DROP COLUMN request_id;
