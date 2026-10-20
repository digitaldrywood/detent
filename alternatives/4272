import { Building2Icon, CheckIcon, ChevronUpIcon, PlusIcon } from "lucide-react";
import { useContext, useEffect, useRef, useState } from "react";

import { ClientContext } from "../../app/client";
import { useAccountApi } from "../../app/account/context";
import { newKey } from "../../app/account/idempotency";
import { useMutation } from "../../app/account/useResource";
import { PLATFORM, SIGN_IN_ORGANIZATIONS } from "../../app/entry/api";
import {
  behindSharedEntry,
  ENTRY_CREATE_ORGANIZATION,
  useSharedOrganizations,
} from "../../app/entry/shared";
import type { AccountBootstrap } from "../../contracts/account";
import { signInPath } from "../../runtime/basePath";
import { cn } from "../../lib/utils";
import { Badge } from "../ui/badge";
import { Input } from "../ui/input";
import { Menu, MenuItem, MenuPopup, MenuSeparator, MenuTrigger } from "../ui/menu";
import { SidebarMenuButton, SidebarMenuItem } from "../ui/sidebar";

type Workspace = {
  readonly id: string;
  readonly name: string;
  readonly role?: string;
};

function WorkspaceBadge({ name }: { name: string }) {
  const initials = name.trim().split(/\s+/).slice(0, 2)
    .map((word) => Array.from(word)[0]).join("").toLocaleUpperCase();
  return (
    <Badge
      aria-hidden
      className="size-5.5 shrink-0 justify-center rounded-sm p-0 text-xs sm:size-5.5"
      variant="secondary"
    >
      {initials}
    </Badge>
  );
}

export function WorkspacePicker({
  current,
  organizations,
  onSelect,
  addHref,
  platformHref,
  pending = false,
  loading = false,
  error = null,
  signInHref,
  onSignOut,
}: {
  readonly current: Workspace;
  readonly organizations: readonly Workspace[];
  readonly onSelect: (id: string) => void;
  readonly addHref: string;
  readonly platformHref?: string;
  readonly pending?: boolean;
  readonly loading?: boolean;
  readonly error?: string | null;
  readonly signInHref?: string;
  readonly onSignOut?: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const search = useRef<HTMLInputElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const popup = useRef<HTMLDivElement>(null);
  const workspaces = organizations.some((workspace) => workspace.id === current.id)
    ? organizations.map((workspace) => workspace.id === current.id ? { ...workspace, role: workspace.role ?? current.role } : workspace)
    : [current, ...organizations];
  const filtered = workspaces.filter((workspace) =>
    workspace.name.toLocaleLowerCase().includes(query.trim().toLocaleLowerCase()),
  );
  useEffect(() => {
    if (error) setOpen(true);
  }, [error]);

  return (
    <SidebarMenuItem className="shrink-0">
      <Menu
        open={open}
        onOpenChange={(next) => {
          setOpen(next);
          if (next) setQuery("");
        }}
        onOpenChangeComplete={(next) => {
          if (next) search.current?.focus();
        }}
      >
        <MenuTrigger
          ref={trigger}
          render={
            <SidebarMenuButton
              aria-label={`Switch workspace: ${current.name}`}
              title={current.name}
              className="w-auto gap-1 px-1"
            >
              <WorkspaceBadge name={current.name} />
              <ChevronUpIcon className="size-3" />
              {current.role === "reporter" ? <span className="truncate text-xs text-muted-foreground">{current.name}</span> : null}
            </SidebarMenuButton>
          }
        />
        <MenuPopup
          ref={popup}
          side="top"
          align="start"
          className="w-56 max-w-(--available-width)"
          finalFocus={trigger}
          aria-label="Workspaces"
        >
          <div className="p-1 pb-2">
            <Input
              ref={search}
              aria-label="Search workspaces"
              placeholder="Search workspaces…"
              type="search"
              size="compact"
              className="[&_input]:text-base sm:[&_input]:text-xs"
              value={query}
              onChange={(event) => setQuery(event.currentTarget.value)}
              onKeyDown={(event) => {
                if (event.key === "ArrowDown" || event.key === "ArrowUp") {
                  event.preventDefault();
                  event.stopPropagation();
                  const rows = popup.current?.querySelectorAll<HTMLElement>(
                    '[role="menuitem"]:not([aria-disabled="true"])',
                  );
                  const row = event.key === "ArrowDown" ? rows?.[0] : rows?.[rows.length - 1];
                  row?.focus();
                } else if (event.key !== "Escape" && event.key !== "Tab") {
                  event.stopPropagation();
                }
              }}
            />
          </div>
          {filtered.map((workspace) => (
            <MenuItem
              key={workspace.id}
              aria-label={workspace.name}
              aria-description={workspace.role}
              aria-current={workspace.id === current.id ? true : undefined}
              title={workspace.name}
              className={cn(
                "min-h-8 py-1.25 text-xs sm:min-h-8 sm:text-xs",
                workspace.id === current.id && "bg-foreground/[0.08] text-foreground",
              )}
              disabled={pending}
              onClick={() => {
                setOpen(false);
                if (workspace.id !== current.id) onSelect(workspace.id);
              }}
            >
              <WorkspaceBadge name={workspace.name} />
              <span className="min-w-0 flex-1 truncate">{workspace.name}</span>
              {workspace.role ? <span className="text-xs capitalize text-muted-foreground">{workspace.role}</span> : null}
              {workspace.id === current.id ? <CheckIcon aria-hidden className="size-3.5" /> : null}
            </MenuItem>
          ))}
          {loading ? (
            <p role="status" className="px-2 py-1.5 text-xs text-muted-foreground">
              Loading workspaces…
            </p>
          ) : null}
          {filtered.length === 0 && !loading && !error ? (
            <p role="status" className="px-2 py-1.5 text-xs text-muted-foreground">
              No workspaces found.
            </p>
          ) : null}
          {error ? (
            <p role="alert" className="px-2 py-1.5 text-xs text-destructive-foreground">
              {error}
            </p>
          ) : null}
          {signInHref ? (
            <MenuItem className="min-h-8 text-xs sm:min-h-8 sm:text-xs" render={<a href={signInHref} />}>
              Sign in again
            </MenuItem>
          ) : null}
          <MenuSeparator />
          <MenuItem className="min-h-8 text-xs sm:min-h-8 sm:text-xs" render={<a href={addHref} />}>
            <PlusIcon aria-hidden className="size-3.5" />
            Add a workspace
          </MenuItem>
          {platformHref ? (
            <MenuItem className="min-h-8 text-xs sm:min-h-8 sm:text-xs" render={<a href={platformHref} />}>
              <Building2Icon aria-hidden className="size-3.5" />
              Platform console
            </MenuItem>
          ) : null}
          {onSignOut ? <><MenuSeparator /><MenuItem disabled={pending} onClick={onSignOut}>Sign out</MenuItem></> : null}
        </MenuPopup>
      </Menu>
    </SidebarMenuItem>
  );
}

function AccountWorkspacePicker({ account }: { readonly account: AccountBootstrap }) {
  const api = useAccountApi();
  const shared = useSharedOrganizations();
  const isShared = behindSharedEntry();
  const sessionEnded = isShared && shared.error?.status === 401;
  const listingError = isShared && shared.error
    ? sessionEnded
      ? "Your Cloud session has ended."
      : "Could not load workspaces. Try again shortly."
    : null;
  const switchTo = useMutation(async (organization: string) => {
    const target = shared.value?.find((candidate) => candidate.id === organization);
    if (target !== undefined) {
      globalThis.location.assign(target.url);
      return null;
    }
    const result = await api.switchOrganization({ organization, key: newKey() });
    globalThis.location.assign(result.next);
    return result;
  });
  const signOut = useMutation(async () => {
    await api.logout();
    globalThis.location.assign(signInPath());
  });

  return (
    <WorkspacePicker
      current={{ ...account.organization, role: account.actor.role }}
      organizations={isShared ? shared.value ?? [] : account.organizations}
      onSelect={(id) => void switchTo.call(id)}
      pending={switchTo.pending || signOut.pending}
      loading={isShared && shared.loading}
      error={listingError ?? switchTo.error?.message ?? signOut.error?.message ?? null}
      onSignOut={account.actor.role === "reporter" ? () => void signOut.call() : undefined}
      signInHref={sessionEnded ? SIGN_IN_ORGANIZATIONS : undefined}
      addHref={isShared ? ENTRY_CREATE_ORGANIZATION : "/organization"}
      platformHref={account.actor.platform_role ? PLATFORM : undefined}
    />
  );
}

export function SidebarWorkspacePicker() {
  const client = useContext(ClientContext);
  return client?.account ? <AccountWorkspacePicker account={client.account} /> : null;
}
