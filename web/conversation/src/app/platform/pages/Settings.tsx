import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import type { PlatformSettings } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import type { PlatformAllowlist } from "../../entry/api.ts";
import { formatBytes } from "../../entry/PlatformConsole.tsx";
import { SettingsRow, SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Loadable, PlatformPage } from "../ui.tsx";

function List({ values, empty = "None" }: { readonly values: readonly string[]; readonly empty?: string }): React.ReactElement {
  if (values.length === 0) return <span className="text-muted-foreground">{empty}</span>;
  return (
    <span className="flex flex-wrap justify-end gap-1">
      {values.map((value) => (
        <Badge key={value} variant="outline" size="sm">
          {value}
        </Badge>
      ))}
    </span>
  );
}

export function SignupPolicy({ value }: { readonly value: PlatformAllowlist }): React.ReactElement {
  const source = value.source.file === "" ? "the entry configuration" : value.source.file;
  return (
    <SettingsSection title="Signup policy">
      <SettingsRow className="text-sm"
        title="Self-service organizations"
        description={`Read-only. Set in ${source} under ${value.source.keys.join(" and ")}.`}
        control={
          !value.self_service ? "Off" : value.open === true ? "Open to any verified account" : "Allowlist only"
        }
      />
      {value.self_service && value.open !== true ? (
        <>
          <SettingsRow className="text-sm" title="Allowed emails" control={<List values={value.allowed_emails} />} />
          <SettingsRow className="text-sm" title="Allowed domains" control={<List values={value.allowed_domains} />} />
        </>
      ) : null}
    </SettingsSection>
  );
}

export function PlatformSettingsPage(): React.ReactElement {
  const api = usePlatformApi();
  const allowlist = useResource<PlatformAllowlist>(() => api.live.allowlist(), [api]);
  const settings = useResource<PlatformSettings>(() => api.settings(), [api]);
  return (
    <PlatformPage crumbs={[{ label: "Settings" }]} proposed width="readable">
      <p className="text-[13px] text-muted-foreground">
        Everything here comes from the entry's config file and is read-only in v1. Change the file and restart the entry.
      </p>
      <Loadable resource={allowlist} label="the signup policy">
        {(value) => <SignupPolicy value={value} />}
      </Loadable>
      <Loadable resource={settings} label="the entry configuration">
        {(value) => (
          <>
            <SettingsSection title="People">
              <SettingsRow className="text-sm" title="Platform staff" description="Sign in to this console." control={<List values={value.staff_emails} />} />
              <SettingsRow className="text-sm"
                title="Support actors"
                description="Staff who may start support access."
                control={<List values={value.support_actors} />}
              />
              <SettingsRow className="text-sm"
                title="Entitlement administrators"
                description="Staff who may grant and revoke plans."
                control={<List values={value.entitlement_administrators} />}
              />
            </SettingsSection>
            <SettingsSection title="Allocation limits">
              <SettingsRow className="text-sm" title="Tenant Hubs" control={<span className="tabular-nums">{value.allocation.max_tenants}</span>} />
              <SettingsRow className="text-sm" title="Concurrent provisions" control={<span className="tabular-nums">{value.allocation.max_concurrent_provisions}</span>} />
              <SettingsRow className="text-sm"
                title="Organizations per person"
                control={<span className="tabular-nums">{value.allocation.max_organizations_per_identity}</span>}
              />
              <SettingsRow className="text-sm" title="Provisioning retries" control={<span className="tabular-nums">{value.allocation.retry_limit}</span>} />
              <SettingsRow className="text-sm" title="Free disk floor" control={formatBytes(value.allocation.min_free_disk_bytes)} />
              <SettingsRow className="text-sm" title="Available memory floor" control={formatBytes(value.allocation.min_available_memory_bytes)} />
            </SettingsSection>
            <SettingsSection title="Billing">
              <SettingsRow className="text-sm"
                title="Stripe mode"
                control={value.billing.mode === null ? "Off" : <Badge variant={value.billing.mode === "live" ? "success" : "warning"}>{value.billing.mode}</Badge>}
              />
              <SettingsRow className="text-sm" title="Account" control={<span className="font-mono text-xs">{value.billing.account_id ?? "—"}</span>} />
            </SettingsSection>
            <p className="text-xs text-muted-foreground">Source: {value.source.file}</p>
          </>
        )}
      </Loadable>
    </PlatformPage>
  );
}
