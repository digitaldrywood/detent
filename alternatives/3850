import React from "react";
import { CopyIcon, PlusIcon } from "lucide-react";
import { useNavigate } from "@tanstack/react-router";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Dialog,
  DialogClose,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogPanel,
  DialogPopup,
  DialogTitle,
} from "../../components/ui/dialog.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
  SettingsSearchTarget,
} from "../settings/settingsLayout.tsx";
import { behindSharedEntry, useSharedOrganizations } from "../entry/shared.ts";
import { makeEntryApi, type EntrySession } from "../entry/api.ts";
import { useOrganizationMembers } from "./OrganizationMembers.tsx";
import { signInPath } from "../../runtime/basePath.ts";
import { AccountError } from "./api.ts";
import { ControlError } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { ModelSelectionSettings } from "./ModelSelectionSettings.tsx";
import { SlugSettings, SupportBanner } from "./Organization.tsx";
import { useMutation, useResource } from "./useResource.ts";

function OrganizationMark({
  name,
}: {
  readonly name: string;
}): React.ReactElement {
  const initials = name
    .trim()
    .split(/\s+/)
    .slice(0, 2)
    .map((word) => word[0])
    .join("")
    .toUpperCase();
  return (
    <span
      aria-hidden="true"
      className="flex size-10 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-sm font-semibold text-primary"
    >
      {initials}
    </span>
  );
}

function CreateOrganizationDialog({
  onClose,
}: {
  readonly onClose: () => void;
}): React.ReactElement {
  const api = useAccountApi();
  const shared = behindSharedEntry();
  const entry = React.useMemo(() => makeEntryApi(), []);
  const session = useResource<EntrySession | null>(
    () => (shared ? entry.session() : Promise.resolve(null)),
    [entry, shared],
  );
  const [name, setName] = React.useState("");
  const [price, setPrice] = React.useState("");
  const key = React.useMemo(() => newKey(), []);
  const used = session.value?.free_slot_used === true;
  const canCreate = shared && session.value?.can_create === true;
  const create = useMutation(async () => {
    try {
      const result = shared
        ? await entry.createOrganization({
            name: name.trim(),
            key,
            csrf: session.value?.csrf ?? "",
            price: used ? price : "",
          })
        : await api.createOrganization({ name: name.trim(), key });
      globalThis.location.assign(result.next);
      return result;
    } catch (error) {
      if (error instanceof AccountError && error.code === "paid_plan_required")
        await session.refresh();
      throw error;
    }
  });
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !create.pending) onClose();
      }}
    >
      <DialogPopup
        render={
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (
                canCreate &&
                !create.pending &&
                name.trim() &&
                (!used || price) &&
                !session.loading &&
                !session.error
              )
                void create.call();
            }}
          />
        }
      >
        <DialogHeader>
          <DialogTitle>Create organization</DialogTitle>
          <DialogDescription>
            {used
              ? "Your Free organization slot is used. Choose a paid plan for this organization. Each organization is billed separately."
              : "You become its owner. It has its own projects, runners and plan. You sign in again once it is ready."}
          </DialogDescription>
        </DialogHeader>
        <DialogPanel className="flex flex-col gap-3">
          <Label htmlFor="org-create-name">Organization name</Label>
          <Input
            id="org-create-name"
            autoComplete="organization"
            maxLength={120}
            value={name}
            disabled={create.pending}
            onChange={(event) => setName(event.currentTarget.value)}
          />
          {used ? (
            <>
              <Label htmlFor="org-create-price">Paid plan</Label>
              <Select
                value={price}
                onValueChange={(value) => setPrice(value ?? "")}
                disabled={create.pending}
              >
                <SelectTrigger id="org-create-price">
                  <SelectValue placeholder="Choose a paid plan">
                    {
                      session.value?.creation_prices?.find(
                        (option) => option.id === price,
                      )?.label
                    }
                  </SelectValue>
                </SelectTrigger>
                <SelectPopup>
                  {session.value?.creation_prices?.map((option) => (
                    <SelectItem key={option.id} value={option.id}>
                      {option.label}
                    </SelectItem>
                  ))}
                </SelectPopup>
              </Select>
              {session.value?.creation_prices?.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  Paid organization creation is temporarily unavailable. Upgrade
                  your existing Free organization to free the slot.
                </p>
              ) : null}
            </>
          ) : null}
          <ControlError
            message={create.error?.message ?? session.error?.message ?? null}
          />
          {session.error ? (
            <Button
              variant="outline"
              size="sm"
              onClick={() => void session.refresh()}
            >
              Try again
            </Button>
          ) : null}
        </DialogPanel>
        <DialogFooter>
          <DialogClose
            render={
              <Button variant="outline" disabled={create.pending}>
                Cancel
              </Button>
            }
          />
          <Button
            type="submit"
            disabled={
              create.pending ||
              session.loading ||
              session.error !== null ||
              !canCreate ||
              !name.trim() ||
              (used && !price)
            }
          >
            {create.pending ? "Creating…" : "Create organization"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

export function OrganizationOverviewRoute(): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const navigate = useNavigate();
  const members = useOrganizationMembers();
  const entry = React.useMemo(() => makeEntryApi(), []);
  const sharedEntry = behindSharedEntry();
  const creation = useResource<EntrySession | null>(
    () => (sharedEntry ? entry.session() : Promise.resolve(null)),
    [entry, sharedEntry],
  );
  const canCreate = sharedEntry && creation.value?.can_create === true;
  const sharedListing = useSharedOrganizations();
  const shared = sharedListing.error ? [] : sharedListing.value;
  const organizations = shared ?? bootstrap?.organizations ?? [];
  const organization = bootstrap?.organization;
  const [name, setName] = React.useState(organization?.name ?? "");
  const [creating, setCreating] = React.useState(false);
  const [copied, setCopied] = React.useState(false);
  const [copyError, setCopyError] = React.useState<string | null>(null);
  const renameKey = React.useMemo(() => newKey(), [name]);
  const rename = useMutation(async () => {
    await api.renameOrganization({ name: name.trim(), key: renameKey });
    globalThis.location.reload();
  });
  const switchTo = useMutation(async (id: string) => {
    const target = shared?.find((candidate) => candidate.id === id);
    if (target !== undefined) globalThis.location.assign(target.url);
    else
      globalThis.location.assign(
        (await api.switchOrganization({ organization: id, key: newKey() }))
          .next,
      );
  });
  const exitSupport = useMutation(async () => {
    await api.logout();
    globalThis.location.assign(signInPath());
  });
  const count = members.value?.member_count ?? members.value?.members.length;
  const projectCount = bootstrap?.projects.length ?? 0;
  const choices =
    organization && !organizations.some((item) => item.id === organization.id)
      ? [
          { ...organization, current: true, role: bootstrap?.actor.role },
          ...organizations,
        ]
      : organizations;
  return (
    <SettingsPageContainer>
      <div className="flex items-center gap-3 px-3 sm:px-4">
        <OrganizationMark name={organization?.name ?? "Organization"} />
        <div className="min-w-0">
          <h2 className="break-words text-xl font-semibold tracking-tight">
            {organization?.name ?? "Organization"}
          </h2>
          <p className="text-sm text-muted-foreground">
            You are the {bootstrap?.actor.role ?? "viewer"}
            {count === undefined
              ? ""
              : ` · ${count} ${count === 1 ? "member" : "members"}`}{" "}
            · {projectCount} {projectCount === 1 ? "project" : "projects"}
          </p>
          <ControlError message={members.error?.message ?? null} />
        </div>
      </div>
      {bootstrap?.support ? (
        <SupportBanner
          {...{
            actor: bootstrap.support.actor,
            reason: bootstrap.support.reason,
            expiresAt: bootstrap.support.expires_at,
          }}
          onExit={() => void exitSupport.call()}
          exiting={exitSupport.pending}
        />
      ) : null}
      {organization ? (
        <SettingsSection title="Organization">
          <SettingsRow
            title="Name"
            description="Shown in the sidebar, invitations and emails."
            control={
              bootstrap?.actor.can_manage ? (
                <form
                  className="flex min-w-0 flex-wrap items-center gap-2"
                  onSubmit={(event) => {
                    event.preventDefault();
                    if (
                      name.trim() &&
                      name.trim() !== organization.name &&
                      !rename.pending
                    )
                      void rename.call();
                  }}
                >
                  <Input
                    aria-label="Organization name"
                    size="sm"
                    maxLength={120}
                    value={name}
                    disabled={rename.pending}
                    onChange={(event) => setName(event.currentTarget.value)}
                    className="min-w-0 flex-1"
                  />
                  <Button
                    size="sm"
                    type="submit"
                    variant="outline"
                    disabled={
                      rename.pending ||
                      !name.trim() ||
                      name.trim() === organization.name
                    }
                  >
                    {rename.pending ? "Saving…" : "Save"}
                  </Button>
                </form>
              ) : (
                <span className="break-words text-sm">{organization.name}</span>
              )
            }
          >
            <ControlError message={rename.error?.message ?? null} />
          </SettingsRow>
          <SettingsRow
            title="Organization ID"
            description="Use it in API calls and support requests."
            control={
              <div className="flex min-w-0 items-center gap-2">
                <code className="break-all text-xs">{organization.id}</code>
                <Button
                  size="icon-sm"
                  variant="ghost"
                  aria-label="Copy organization ID"
                  onClick={async () => {
                    try {
                      await navigator.clipboard.writeText(organization.id);
                      setCopied(true);
                      setCopyError(null);
                    } catch {
                      setCopyError("Could not copy the organization ID.");
                    }
                  }}
                >
                  <CopyIcon />
                </Button>
              </div>
            }
          >
            {copied ? (
              <span role="status" className="text-xs text-muted-foreground">
                Organization ID copied.
              </span>
            ) : null}
            <ControlError message={copyError} />
          </SettingsRow>
          <SettingsRow
            title="Plan"
            description="Capacity, AI credits and limits live on Plan & usage."
            control={
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant="outline">
                  {bootstrap?.plan?.name ?? "Unavailable"}
                </Badge>
                {bootstrap?.actor.can_manage ? (
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() =>
                      void navigate({
                        to: "/settings/$section",
                        params: { section: "plan" },
                      })
                    }
                  >
                    Plan & usage
                  </Button>
                ) : null}
              </div>
            }
          />
        </SettingsSection>
      ) : null}
      <SlugSettings canManage={bootstrap?.actor.can_manage ?? false} />
      <SettingsSection title="Your organizations">
        {choices.map((item) => (
          <SettingsRow
            key={item.id}
            title={
              <span className="flex items-center gap-3">
                <OrganizationMark
                  name={
                    item.id === organization?.id ? organization.name : item.name
                  }
                />
                <span className="min-w-0 break-words">
                  {item.id === organization?.id ? organization.name : item.name}
                </span>
              </span>
            }
            description={
              <span className="capitalize">
                {item.role ?? (item.current ? bootstrap?.actor.role : "")}
              </span>
            }
            control={
              item.current ? (
                <Badge variant="outline">Current</Badge>
              ) : (
                <Button
                  size="sm"
                  variant="outline"
                  aria-label={`Switch to ${item.name}`}
                  disabled={switchTo.pending}
                  onClick={() => void switchTo.call(item.id)}
                >
                  Switch
                </Button>
              )
            }
          />
        ))}
        {canCreate ? (
          <SettingsRow
            title="New organization"
            description="Its own projects, runners and plan. You sign in again to switch into it."
            control={
              <Button
                size="sm"
                variant="outline"
                onClick={() => setCreating(true)}
              >
                <PlusIcon />
                Create organization
              </Button>
            }
          />
        ) : null}
        <ControlError
          message={creation.error?.message ?? sharedListing.error?.message ?? switchTo.error?.message ?? null}
        />
      </SettingsSection>
      {creating && canCreate ? (
        <CreateOrganizationDialog onClose={() => setCreating(false)} />
      ) : null}
    </SettingsPageContainer>
  );
}

export function OrganizationModelsRoute(): React.ReactElement {
  const bootstrap = useAccountBootstrap();
  return (
    <SettingsPageContainer>
      <h2 className="px-3 text-xl font-semibold tracking-tight sm:px-4">
        Models
      </h2>
      <SettingsSearchTarget id="settings-organization-models">
        <ModelSelectionSettings
          canManage={bootstrap?.actor.can_manage ?? false}
        />
      </SettingsSearchTarget>
    </SettingsPageContainer>
  );
}
