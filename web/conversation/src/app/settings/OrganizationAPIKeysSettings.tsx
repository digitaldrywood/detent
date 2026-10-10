import React from "react";
import type {
  AccessKey,
  PersonalKeyPolicy,
} from "../../contracts/accessKeys.ts";
import { Label } from "../../components/ui/label.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import {
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
} from "../../components/ui/alert-dialog.tsx";
import {
  useAccountApi,
  useAccountBootstrap,
  useCanManage,
} from "../account/context.ts";
import { ControlError, NativeSelect } from "../account/controls.tsx";
import {
  AccessKeyManager,
  KeyScope,
  lastKeyUse,
} from "./PersonalAPIKeysSettings.tsx";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
  useRelativeTimeTick,
} from "./settingsLayout.tsx";

export function OrganizationAPIKeysSettings() {
  const canManage = useCanManage();
  return (
    <SettingsPageContainer>
      <SettingsSection title="Org settings · API & MCP" variant="plain">
        <SettingsRow
          title="Organization access"
          description="A key never grants more than its owner has. Manage your personal keys in Account settings → API keys."
        />
      </SettingsSection>
      {canManage ? (
        <>
          <AccessKeyManager service />
          <OrganizationKeyControls />
        </>
      ) : (
        <SettingsSection title="Personal API keys">
          <SettingsRow
            title="Manage your keys in account settings"
            description="Organization key controls are available to owners and admins."
            control={
              <Button
                size="sm"
                variant="outline"
                render={<a href="api-keys" />}
              >
                Open my API keys
              </Button>
            }
          />
        </SettingsSection>
      )}
    </SettingsPageContainer>
  );
}

function OrganizationKeyControls() {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  useRelativeTimeTick(60_000);
  const [keys, setKeys] = React.useState<readonly AccessKey[]>([]);
  const [policy, setPolicy] = React.useState<PersonalKeyPolicy>("allowed");
  const [draft, setDraft] = React.useState<PersonalKeyPolicy>("allowed");
  const [loaded, setLoaded] = React.useState(false);
  const [busy, setBusy] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const [revoke, setRevoke] = React.useState<AccessKey | null>(null);
  const currentApi = React.useRef(api);
  currentApi.current = api;
  const load = React.useCallback(async () => {
    const [listed, value] = await Promise.all([
      api.memberKeys(),
      api.keyPolicy(),
    ]);
    if (currentApi.current !== api) return;
    setKeys(listed.keys);
    setPolicy(value.personal_keys);
    setDraft(value.personal_keys);
    setLoaded(true);
  }, [api]);
  React.useEffect(() => {
    let current = true;
    setLoaded(false);
    setKeys([]);
    setRevoke(null);
    setError(null);
    load().catch(() => {
      if (current)
        setError(
          "Could not load organization key controls. Reload to try again.",
        );
    });
    return () => {
      current = false;
    };
  }, [load]);
  async function mutate(action: () => Promise<unknown>) {
    setBusy(true);
    setError(null);
    try {
      await action();
      if (currentApi.current !== api) return;
      setRevoke(null);
      await load();
    } catch {
      if (currentApi.current === api)
        setError(
          "Could not save the change or refresh the list. Reload to check the current organization access.",
        );
    } finally {
      setBusy(false);
    }
  }
  const pending = keys.filter((key) =>
    key.effective_reach?.some((org) => org.status === "pending"),
  );
  return (
    <>
      <SettingsSection title="Personal key policy">
        {error && (
          <div className="p-3">
            <ControlError message={error} />
          </div>
        )}
        {!loaded && !error && (
          <SettingsRow
            title="Loading organization key controls…"
            role="status"
          />
        )}
        {loaded && (
          <SettingsRow
            title={
              <Label htmlFor="org-personal-key-policy">Personal keys</Label>
            }
            description="Applies to personal keys reaching this organization. Service keys are managed separately."
            control={
              <div className="flex flex-wrap items-center gap-2">
                <NativeSelect
                  id="org-personal-key-policy"
                  value={draft}
                  disabled={busy}
                  onValueChange={(value) =>
                    setDraft(value as PersonalKeyPolicy)
                  }
                  options={[
                    { value: "allowed", label: "Allowed" },
                    { value: "approval", label: "Require approval" },
                    { value: "blocked", label: "Blocked" },
                  ]}
                />
                <Button
                  size="sm"
                  disabled={busy || draft === policy}
                  onClick={() => void mutate(() => api.saveKeyPolicy(draft))}
                >
                  Save policy
                </Button>
              </div>
            }
          />
        )}
      </SettingsSection>
      {loaded && (
        <>
          <SettingsSection title="Approval queue">
            {pending.length === 0 && (
              <SettingsRow title="No keys awaiting approval" />
            )}
            {pending.map((key) => (
              <SettingsRow
                key={key.id}
                title={key.name}
                description={key.owner_email}
                status={<KeyScope apiKey={key} />}
                control={
                  <Button
                    size="xs"
                    disabled={busy}
                    aria-label={`Approve ${key.name}`}
                    onClick={() =>
                      void mutate(() => api.approveMemberKey(key.id))
                    }
                  >
                    Approve for this org
                  </Button>
                }
              />
            ))}
          </SettingsSection>
          <SettingsSection title="Member keys">
            {keys.length === 0 && (
              <SettingsRow
                title="No member keys reach this org"
                description="Personal keys with current membership and matching scope appear here, including unused keys."
              />
            )}
            {keys.map((key) => (
              <SettingsRow
                key={key.id}
                title={
                  <span className="flex flex-wrap items-center gap-2 break-words">
                    {key.name}
                    <Badge variant="outline">{key.permission}</Badge>
                  </span>
                }
                description={
                  <span>
                    {key.owner_email} ·{" "}
                    {lastKeyUse(key.effective_reach?.[0]?.last_used_at)}
                  </span>
                }
                status={<KeyScope apiKey={key} />}
                control={
                  !key.effective_reach?.[0]?.blocked && (
                    <Button
                      size="xs"
                      variant="destructive-outline"
                      disabled={busy}
                      aria-label={`Revoke ${key.name} for this org`}
                      onClick={() => {
                        setError(null);
                        setRevoke(key);
                      }}
                    >
                      Revoke for this org
                    </Button>
                  )
                }
              />
            ))}
          </SettingsSection>
        </>
      )}
      <AlertDialog
        open={revoke !== null}
        onOpenChange={(value) => {
          if (!value && !busy) setRevoke(null);
        }}
      >
        <AlertDialogPopup>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Revoke {revoke?.name} for {account?.organization.name}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              Access to this organization stops immediately. The key keeps its
              access to other organizations.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="px-6 pb-4">
            <ControlError message={error} />
          </div>
          <AlertDialogFooter>
            <AlertDialogClose
              render={
                <Button variant="outline" disabled={busy}>
                  Keep access
                </Button>
              }
            />
            <Button
              variant="destructive"
              disabled={busy}
              onClick={() => {
                if (revoke) void mutate(() => api.blockMemberKey(revoke.id));
              }}
            >
              Revoke for this org
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>
    </>
  );
}
