-- +goose Up
CREATE TABLE runner_host_metric_segments (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    hour timestamptz NOT NULL,
    segment_id timestamptz NOT NULL,
    partial boolean NOT NULL,
    sample_count bigint NOT NULL CHECK (sample_count > 0),
    logical_cores integer NOT NULL CHECK (logical_cores > 0),
    memory_total_bytes bigint NOT NULL CHECK (memory_total_bytes >= 0),
    memory_available_min_bytes bigint NOT NULL CHECK (memory_available_min_bytes >= 0),
    memory_available_sum_bytes bigint NOT NULL CHECK (memory_available_sum_bytes >= 0),
    memory_sample_count bigint NOT NULL CHECK (memory_sample_count >= 0),
    swap_used_max_bytes bigint NOT NULL CHECK (swap_used_max_bytes >= 0),
    swap_sample_count bigint NOT NULL CHECK (swap_sample_count >= 0),
    psi_some_avg10_sum double precision NOT NULL,
    psi_some_sample_count bigint NOT NULL CHECK (psi_some_sample_count >= 0),
    psi_full_avg10_sum double precision NOT NULL,
    psi_full_sample_count bigint NOT NULL CHECK (psi_full_sample_count >= 0),
    pressure_warn_count bigint NOT NULL CHECK (pressure_warn_count >= 0),
    pressure_critical_count bigint NOT NULL CHECK (pressure_critical_count >= 0),
    pressure_sample_count bigint NOT NULL CHECK (pressure_sample_count >= 0),
    cpu_busy_sum_percent double precision NOT NULL,
    cpu_busy_max_percent double precision NOT NULL,
    cpu_sample_count bigint NOT NULL CHECK (cpu_sample_count >= 0),
    load1_max double precision NOT NULL,
    load_sample_count bigint NOT NULL CHECK (load_sample_count >= 0),
    disk_free_min_bytes bigint NOT NULL CHECK (disk_free_min_bytes >= 0),
    disk_total_min_bytes bigint NOT NULL CHECK (disk_total_min_bytes >= 0),
    disk_sample_count bigint NOT NULL CHECK (disk_sample_count >= 0),
    PRIMARY KEY (organization_id, runner_id, hour, segment_id),
    CHECK (hour = date_trunc('hour', hour)),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE
);
CREATE INDEX runner_host_metric_segments_range_idx ON runner_host_metric_segments (organization_id, runner_id, hour DESC, segment_id);
CREATE INDEX runner_host_metric_segments_retention_idx ON runner_host_metric_segments (organization_id, hour, runner_id, segment_id);

CREATE TABLE runner_native_admission_observations (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    runner_revision bigint NOT NULL CHECK (runner_revision > 0),
    received_at timestamptz NOT NULL,
    observed_at timestamptz NOT NULL,
    policy_id text NOT NULL,
    last_refresh_at timestamptz NOT NULL,
    refresh_duration bigint NOT NULL CHECK (refresh_duration >= 0),
    PRIMARY KEY (organization_id, runner_id, project_id),
    FOREIGN KEY (organization_id, runner_id) REFERENCES runner_identities (organization_id, runner_id) ON DELETE CASCADE,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE runner_native_admission_selectors (
    organization_id text NOT NULL,
    runner_id text NOT NULL,
    project_id text NOT NULL,
    kind text NOT NULL CHECK (kind IN ('workflow','author','assignee','include','exclude')),
    position integer NOT NULL CHECK (position BETWEEN 0 AND 31),
    value text NOT NULL,
    PRIMARY KEY (organization_id, runner_id, project_id, kind, position),
    FOREIGN KEY (organization_id, runner_id, project_id) REFERENCES runner_native_admission_observations (organization_id, runner_id, project_id) ON DELETE CASCADE
);

ALTER TABLE runner_host_metric_segments OWNER TO detent_migration;
ALTER TABLE runner_native_admission_observations OWNER TO detent_migration;
ALTER TABLE runner_native_admission_selectors OWNER TO detent_migration;
REVOKE ALL ON runner_host_metric_segments, runner_native_admission_observations, runner_native_admission_selectors FROM PUBLIC, detent_platform;
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_host_metric_segments, runner_native_admission_observations, runner_native_admission_selectors TO detent_runtime;
ALTER TABLE runner_host_metric_segments ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_host_metric_segments FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_native_admission_observations ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_native_admission_observations FORCE ROW LEVEL SECURITY;
ALTER TABLE runner_native_admission_selectors ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_native_admission_selectors FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON runner_host_metric_segments TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON runner_native_admission_observations TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));
CREATE POLICY tenant_isolation ON runner_native_admission_selectors TO detent_runtime, detent_migration USING (organization_id = NULLIF(current_setting('detent.organization_id', true), '')) WITH CHECK (organization_id = NULLIF(current_setting('detent.organization_id', true), ''));

-- +goose Down
DROP TABLE runner_native_admission_selectors;
DROP TABLE runner_native_admission_observations;
DROP TABLE runner_host_metric_segments;
