import React from "react";
import type {
  AccessKey,
  PersonalKeyPolicy,
  KeyContext,
} from "../../contracts/accessKeys.ts";
import { Label } from "../../components/ui/label.tsx";
import { Button } from "../../components/ui/button.tsx";
import { MoreHorizontal } from "lucide-react";
import {
  Menu,
  MenuTrigger,
  MenuPopup,
  MenuItem,
} from "../../components/ui/menu.tsx";
import {
  AlertDialog,
  AlertDialogClose,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogPopup,
  AlertDialogTitle,
} from "../../components/ui/alert-dialog.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { ControlError, NativeSelect } from "../account/controls.tsx";
import { APIKeyTable, keyIsInactive } from "./APIKeyTable.tsx";
import { useRelativeTimeTick } from "./settingsLayout.tsx";

export function useOrganizationKeyControls(enabled: boolean) {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  const [keys, setKeys] = React.useState<readonly AccessKey[]>([]);
  const [nextCursor, setNextCursor] = React.useState("");
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
    setNextCursor(listed.next_cursor ?? "");
    setPolicy(value.personal_keys);
    setDraft(value.personal_keys);
    setLoaded(true);
  }, [api]);
  React.useEffect(() => {
    let current = true;
    setLoaded(false);
    setKeys([]);
    setNextCursor("");
    setRevoke(null);
    setError(null);
    if (enabled)
      load().catch(() => {
        if (current)
          setError(
            "Could not load organization key controls. Reload to try again.",
          );
      });
    return () => {
      current = false;
    };
  }, [load, enabled]);
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
  async function loadMore() {
    setBusy(true);
    setError(null);
    try {
      const listed = await api.memberKeys(nextCursor);
      if (currentApi.current !== api) return;
      setKeys((current) => [
        ...current,
        ...listed.keys.filter(
          (key) => !current.some((item) => item.id === key.id),
        ),
      ]);
      setNextCursor(listed.next_cursor ?? "");
    } catch {
      if (currentApi.current === api)
        setError("Could not load more member keys. Try again.");
    } finally {
      setBusy(false);
    }
  }
  const pending = keys.filter((key) =>
    key.effective_reach?.some(
      (org) =>
        org.organization_id === account?.organization.id &&
        org.status === "pending",
    ),
  );
  return {
    keys,
    pending,
    nextCursor,
    policy,
    draft,
    loaded,
    busy,
    error,
    revoke,
    setDraft,
    setError,
    setRevoke,
    mutate,
    loadMore,
  };
}

export function OrganizationKeyControls({
  controls,
  context,
  showHistory,
}: {
  readonly controls: ReturnType<typeof useOrganizationKeyControls>;
  readonly context: KeyContext | null;
  readonly showHistory: boolean;
}) {
  const api = useAccountApi();
  const account = useAccountBootstrap();
  const now = useRelativeTimeTick(60_000);
  const {
    keys,
    pending,
    nextCursor,
    policy,
    draft,
    loaded,
    busy,
    error,
    revoke,
    setDraft,
    setError,
    setRevoke,
    mutate,
    loadMore,
  } = controls;
  function confirmBlock(key: AccessKey) {
    setError(null);
    setRevoke(key);
  }
  return (
    <>
      <p className="text-xs text-muted-foreground">
        Personal keys from members that reach this organization. Blocking one
        cuts its access here only.
      </p>
      {error && <ControlError message={error} />}
      {loaded && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-card p-3">
          <div className="space-y-0.5">
            <Label htmlFor="org-personal-key-policy">
              Personal keys in this organization
            </Label>
            <p className="text-xs text-muted-foreground">
              Service keys are not affected.
            </p>
          </div>
          <div className="flex items-center gap-2">
            <NativeSelect
              id="org-personal-key-policy"
              value={draft}
              disabled={busy}
              onValueChange={(value) => setDraft(value as PersonalKeyPolicy)}
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
              Save
            </Button>
          </div>
        </div>
      )}
      {pending.map((key) => (
        <div
          key={key.id}
          role="group"
          aria-label={`${key.name} awaiting approval`}
          className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-warning/30 bg-warning-surface px-3 py-2"
        >
          <div className="flex min-w-0 flex-wrap items-center gap-x-1 text-xs">
            <span
              aria-hidden="true"
              className="mr-1 size-1.5 shrink-0 rounded-full bg-warning-foreground"
            />
            <span className="break-words font-medium">{key.name}</span>
            <span className="break-all text-muted-foreground">
              · {key.owner_email} · {key.permission} · awaiting approval
            </span>
          </div>
          <div className="flex items-center gap-2">
            <Button
              size="xs"
              variant="outline"
              disabled={busy}
              aria-label={`Block ${key.name} for this org`}
              onClick={() => confirmBlock(key)}
            >
              Block
            </Button>
            <Button
              size="xs"
              disabled={busy}
              aria-label={`Approve ${key.name}`}
              onClick={() => void mutate(() => api.approveMemberKey(key.id))}
            >
              Approve
            </Button>
          </div>
        </div>
      ))}
      <APIKeyTable
        label="Member keys"
        keys={keys.filter((key) => showHistory || !keyIsInactive(key, now))}
        context={context}
        organizationId={account?.organization.id}
        loaded={loaded}
        error={Boolean(error)}
        emptyMessage="No member keys reach this org"
        renderActions={(key, inactive) => {
          const reach = key.effective_reach?.find(
            (org) => org.organization_id === account?.organization.id,
          );
          return (
            !inactive &&
            reach?.status !== "blocked" &&
            !reach?.blocked && (
              <div className="flex justify-end">
                <Menu>
                  <MenuTrigger
                    render={<Button size="icon-xs" variant="ghost-muted" />}
                    disabled={busy}
                    aria-label={`More actions for ${key.name}`}
                  >
                    <MoreHorizontal aria-hidden="true" />
                  </MenuTrigger>
                  <MenuPopup align="end">
                    <MenuItem
                      variant="destructive"
                      onClick={() => confirmBlock(key)}
                    >
                      Block for this org
                    </MenuItem>
                  </MenuPopup>
                </Menu>
              </div>
            )
          );
        }}
      />
      {nextCursor && (
        <div className="flex justify-end">
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={() => void loadMore()}
          >
            Load more member keys
          </Button>
        </div>
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
              Block {revoke?.name} for {account?.organization.name}?
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
              Block for this org
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>
    </>
  );
}
