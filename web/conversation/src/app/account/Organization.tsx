import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
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
import { Switch } from "../../components/ui/switch.tsx";
import type { Invitation, Member, ProjectGrant } from "../../contracts/account.ts";
import { SettingsPageContainer, SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { behindSharedEntry } from "../entry/shared.ts";
import { useOrganizationMembers } from "./OrganizationMembers.tsx";
import { useMutation } from "./useResource.ts";
import { signInPath } from "../../runtime/basePath.ts";

const ROLES = ["owner", "admin", "member", "reporter", "viewer"] as const;
const ROLE_OPTIONS = ROLES.map((role) => ({
  value: role,
  label: role.charAt(0).toUpperCase() + role.slice(1),
}));

type AccessProject = {
  readonly id: string;
  readonly name: string;
  readonly can_write: boolean;
  readonly can_manage_runners: boolean;
};

function replaceGrant(grants: readonly ProjectGrant[], projectId: string, grant: ProjectGrant | null): ProjectGrant[] {
  const next = grants.filter((current) => current.project_id !== projectId);
  if (grant !== null) next.push(grant);
  return next;
}

function ProjectAccess({ projects, grants, identity, canManage, busy, onChange }: {
  readonly projects: readonly AccessProject[];
  readonly grants: readonly ProjectGrant[];
  readonly identity: string;
  readonly canManage: boolean;
  readonly busy: boolean;
  readonly onChange: (projectId: string, grant: ProjectGrant | null) => void;
}): React.ReactElement {
  return (
    <ul className="flex flex-col gap-1.5">
      {projects.map((project) => {
        const grant = grants.find((current) => current.project_id === project.id);
        const level = grant === undefined ? "none" : grant.write ? "write" : "read";
        return (
          <li key={project.id} className="flex flex-wrap items-center gap-3 text-xs">
            <span className="min-w-24 text-foreground">{project.name}</span>
            {canManage ? (
              <>
                <NativeSelect
                  aria-label={`Access to ${project.name} for ${identity}`}
                  value={level}
                  disabled={busy}
                  options={[
                    { value: "none", label: "No access" },
                    { value: "read", label: "Read" },
                    { value: "write", label: "Write", disabled: !project.can_write },
                  ]}
                  onValueChange={(access) => onChange(project.id, access === "none" ? null : {
                    project_id: project.id, write: access === "write", runner: grant?.runner ?? false,
                  })}
                />
                <label className="flex items-center gap-1.5 text-muted-foreground">
                  <Switch
                    size="sm"
                    aria-label={`Runner management on ${project.name} for ${identity}`}
                    checked={grant?.runner ?? false}
                    disabled={busy || grant === undefined || !project.can_manage_runners}
                    onCheckedChange={(runner) => {
                      if (grant !== undefined) onChange(project.id, { ...grant, runner });
                    }}
                  />
                  runners
                </label>
              </>
            ) : (
              <span className="text-muted-foreground">
                {level === "none" ? "no access" : level}{grant?.runner ? " · runners" : ""}
              </span>
            )}
          </li>
        );
      })}
    </ul>
  );
}

/**
 * The message for a refused mutation. `last_owner` is the one the screen has
 * to say in its own words: the hub's message is about a database rule, and the
 * reader's question is "why can I not remove this person".
 */
export function refusalMessage(error: AccountError | null): string | null {
  if (error === null) return null;
  if (error.code === "last_owner") {
    return "An organization keeps at least one owner. Make somebody else an owner first.";
  }
  // The hub writes its 403s for readers ("This member cannot be removed; the
  // organization must retain an owner"), and throwing that away for a generic
  // sentence tells the reader less than the server already said. The fallback
  // is for a refusal that carried no message at all.
  if (error.status === 403) {
    return error.message.trim().length > 0
      ? error.message
      : "You do not have permission to change this organization.";
  }
  if (error.status === 404) return "That member is no longer in this organization.";
  return error.message;
}

export function SupportBanner({
  actor,
  reason,
  expiresAt,
  onExit,
  exiting = false,
}: {
  readonly actor: string;
  readonly reason: string;
  readonly expiresAt: string;
  readonly onExit: () => void;
  readonly exiting?: boolean;
}): React.ReactElement {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-start gap-3 rounded-[10px] border border-warning/30 bg-warning-surface px-3.5 py-3 text-[13px] text-warning-foreground"
    >
      <div className="min-w-0 flex-1">
        <b className="font-semibold">You are in a Detent support session.</b>{" "}
        {actor} is acting as this organization until {expiresAt}. Reason: {reason}. Everything done
        here is recorded against that session.
      </div>
      <Button size="sm" variant="warning-outline" onClick={onExit} disabled={exiting}>
        {exiting ? "Exiting…" : "Exit support session"}
      </Button>
    </div>
  );
}

function RemoveMemberDialog({
  member,
  onConfirm,
  onOpenChange,
  pending,
  error,
}: {
  readonly member: Member | null;
  readonly onConfirm: (member: Member) => void;
  readonly onOpenChange: (open: boolean) => void;
  readonly pending: boolean;
  readonly error: string | null;
}): React.ReactElement {
  return (
    <Dialog open={member !== null} onOpenChange={onOpenChange}>
      <DialogPopup>
        <DialogHeader>
          <DialogTitle>Remove {member?.email ?? "this member"}?</DialogTitle>
          <DialogDescription>
            They lose access to every project in this organization immediately. Their work stays.
          </DialogDescription>
        </DialogHeader>
        <DialogPanel>
          <ControlError message={error} />
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline">Keep them</Button>} />
          <Button
            variant="destructive"
            disabled={pending || member === null}
            onClick={() => {
              if (member !== null) onConfirm(member);
            }}
          >
            {pending ? "Removing…" : "Remove"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

export function MembersTable({
  members,
  projects,
  canManage,
  actorRole,
  onRoleChange,
  onGrantChange,
  onRemove,
  busyMember,
  errorFor,
}: {
  readonly members: readonly Member[];
  readonly projects: readonly AccessProject[];
  readonly canManage: boolean;
  readonly actorRole: string;
  readonly onRoleChange: (member: Member, role: string) => void;
  readonly onGrantChange: (member: Member, projectId: string, grant: ProjectGrant | null) => void;
  readonly onRemove: (member: Member) => void;
  readonly busyMember: string | null;
  readonly errorFor: (member: Member) => string | null;
}): React.ReactElement {
  return (
    <Table>
      <TableHeader>
        <TableRow>
          <TableHead>Member</TableHead>
          <TableHead>Role</TableHead>
          <TableHead>Project access</TableHead>
          {canManage ? <TableHead className="text-right">Actions</TableHead> : null}
        </TableRow>
      </TableHeader>
      <TableBody>
        {members.map((member) => {
          const identity = member.email.trim() || member.name?.trim() || member.user_id;
          const busy = busyMember === member.id;
          const failure = errorFor(member);
          return (
            <TableRow key={member.id}>
              <TableCell>
                <div className="font-medium text-foreground">{identity}</div>
                {member.email.trim() && member.name?.trim() ? (
                  <div className="text-xs text-muted-foreground">{member.name}</div>
                ) : null}
                <div className="text-xs text-muted-foreground">
                  {member.status === "active"
                    ? member.never_signed_in ? "Hasn't signed in yet" : "Active"
                    : "Disabled"}
                </div>
                {failure === null ? null : (
                  <div className="pt-1">
                    <ControlError message={failure} />
                  </div>
                )}
              </TableCell>
              <TableCell>
                {canManage ? (
                  <div className="flex flex-col gap-1.5">
                    <NativeSelect
                      aria-label={`Role for ${identity}`}
                      value={member.role}
                      disabled={busy}
                      options={ROLE_OPTIONS.map((option) => ({
                        ...option,
                        // Only an owner may make another owner: the hub refuses
                        // it, so the option is not offered to an admin either.
                        disabled: option.value === "owner" && actorRole !== "owner",
                      }))}
                      onValueChange={(role) => onRoleChange(member, role)}
                      className="min-w-[130px]"
                    />
                    {member.role === "reporter" ? <p className="text-sm text-muted-foreground">Reads issues and files new ones into Triage.</p> : null}
                  </div>
                ) : (
                  <span className="text-sm capitalize text-muted-foreground">{member.role}</span>
                )}
              </TableCell>
              <TableCell>
                <ProjectAccess
                  projects={projects}
                  grants={member.grants}
                  identity={identity}
                  canManage={canManage}
                  busy={busy}
                  onChange={(projectId, grant) => onGrantChange(member, projectId, grant)}
                />
              </TableCell>
              {canManage ? (
                <TableCell className="text-right">
                  <Button
                    size="xs"
                    variant="destructive-outline"
                    disabled={busy}
                    onClick={() => onRemove(member)}
                  >
                    Remove
                  </Button>
                </TableCell>
              ) : null}
            </TableRow>
          );
        })}
      </TableBody>
    </Table>
  );
}

export function InviteForm({
  onInvite,
  projects = [],
  actorRole = "owner",
  pending = false,
  error = null,
}: {
  readonly onInvite: (input: { email: string; role: string; grants: readonly ProjectGrant[] }) => Promise<boolean>;
  readonly projects?: readonly AccessProject[];
  readonly actorRole?: string;
  readonly pending?: boolean;
  readonly error?: string | null;
}): React.ReactElement {
  const [email, setEmail] = React.useState("");
  const [role, setRole] = React.useState<string>("member");
  const [grants, setGrants] = React.useState<ProjectGrant[]>([]);
  const [invitedEmail, setInvitedEmail] = React.useState<string | null>(null);
  return (
    <form
      className="flex flex-col gap-2 py-3"
      onSubmit={async (event) => {
        event.preventDefault();
        const value = email.trim();
        if (pending || value.length === 0) return;
        setInvitedEmail(null);
        if (await onInvite({ email: value, role, grants })) {
          setEmail("");
          setRole("member");
          setGrants([]);
          setInvitedEmail(value);
        }
      }}
    >
      <div className="flex flex-1 flex-col gap-1.5">
        <Label htmlFor="invite-email">Email</Label>
        <Input
          id="invite-email"
          type="email"
          value={email}
          disabled={pending}
          placeholder="colleague@example.com"
          onChange={(event) => setEmail(event.currentTarget.value)}
        />
      </div>
      <div className="flex flex-col gap-1.5">
        <Label htmlFor="invite-role">Role</Label>
        <NativeSelect
          id="invite-role"
          value={role}
          disabled={pending}
          options={ROLE_OPTIONS.map((option) => ({ ...option, disabled: option.value === "owner" && actorRole !== "owner" }))}
          onValueChange={setRole}
          className="min-w-[130px]"
        />
        {role === "reporter" ? <p className="text-sm text-muted-foreground">Reads issues and files new ones into Triage.</p> : null}
      </div>
      <ProjectAccess
        projects={projects}
        grants={grants}
        identity="invitation"
        canManage
        busy={pending}
        onChange={(projectId, grant) => setGrants((current) => replaceGrant(current, projectId, grant))}
      />
      <Button type="submit" disabled={pending || email.trim().length === 0}>
        {pending ? "Inviting…" : "Send invitation"}
      </Button>
      <div className="w-full sm:w-auto">
        <ControlError message={error} />
      </div>
      {invitedEmail === null ? null : (
        <p role="status" className="w-full text-sm text-muted-foreground">
          Invitation sent to {invitedEmail}.
        </p>
      )}
    </form>
  );
}

export function OrganizationSwitcher({
  organizations,
  onSwitch,
  pending = false,
}: {
  readonly organizations: readonly {
    readonly id: string;
    readonly name: string;
    readonly current: boolean;
  }[];
  readonly onSwitch: (organization: string) => void;
  readonly pending?: boolean;
}): React.ReactElement | null {
  if (organizations.length < 2) return null;
  const current = organizations.find((organization) => organization.current)?.id ?? "";
  return (
    <NativeSelect
      aria-label="Switch organization"
      value={current}
      disabled={pending}
      options={organizations.map((organization) => ({
        value: organization.id,
        label: organization.name,
      }))}
      onValueChange={(id) => {
        if (id !== current) onSwitch(id);
      }}
    />
  );
}

export function OrganizationMembersRoute(): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const members = useOrganizationMembers();
  const [removing, setRemoving] = React.useState<Member | null>(null);
  const [busyMember, setBusyMember] = React.useState<string | null>(null);
  const [memberErrors, setMemberErrors] = React.useState<Record<string, string>>({});
  const [revoking, setRevoking] = React.useState<Invitation | null>(null);
  const [activeInvitation, setActiveInvitation] = React.useState<string | null>(null);
  const [resentInvitation, setResentInvitation] = React.useState<string | null>(null);

  const canManage = bootstrap?.actor.can_manage ?? false;
  const actorRole = bootstrap?.actor.role ?? "viewer";
  const projects = React.useMemo(
    () => bootstrap?.projects ?? [],
    [bootstrap],
  );

  const noteFailure = React.useCallback((member: Member, error: AccountError | null) => {
    setMemberErrors((current) => {
      const next = { ...current };
      const message = refusalMessage(error);
      if (message === null) delete next[member.id];
      else next[member.id] = message;
      return next;
    });
  }, []);

  /** Every member mutation is the same shape: run it, report it, re-read. */
  const runForMember = React.useCallback(
    async (member: Member, work: () => Promise<unknown>) => {
      setBusyMember(member.id);
      noteFailure(member, null);
      try {
        await work();
        await members.refresh();
      } catch (cause) {
        noteFailure(member, cause instanceof AccountError ? cause : null);
        if (!(cause instanceof AccountError)) throw cause;
        // The hub may have changed something before it refused; re-reading is
        // the only honest way back to what is actually true.
        await members.refresh();
      } finally {
        setBusyMember(null);
      }
    },
    [members, noteFailure],
  );

  const invite = useMutation(async (input: { email: string; role: string; grants: readonly ProjectGrant[] }) => {
    const created = await api.invite({ ...input, key: newKey() });
    await members.refresh();
    return created;
  });

  const editInvitation = useMutation(async (input: { invitation: string; grants: readonly ProjectGrant[] }) => {
    await api.setInvitationGrants({ ...input, key: newKey() });
    await members.refresh();
  });

  const invitationAction = useMutation(
    async (invitation: Invitation, action: "revoke" | "resend") => {
      setActiveInvitation(invitation.id);
      setResentInvitation(null);
      const input = { invitation: invitation.id, key: newKey() };
      if (action === "revoke") {
        await api.revokeInvitation(input);
        setRevoking(null);
      } else {
        await api.resendInvitation(input);
        setResentInvitation(invitation.id);
      }
      await members.refresh();
      return true;
    },
  );

  const exitSupport = useMutation(async () => {
    await api.logout();
    globalThis.location?.assign(signInPath());
    return null;
  });

  const removeError = removing === null ? null : (memberErrors[removing.id] ?? null);

  return (
    <SettingsPageContainer>
      <h2 className="px-3 text-xl font-semibold tracking-tight sm:px-4">Members</h2>

      {bootstrap?.support == null ? null : (
        <SupportBanner
          actor={bootstrap.support.actor}
          reason={bootstrap.support.reason}
          expiresAt={bootstrap.support.expires_at}
          onExit={() => void exitSupport.call()}
          exiting={exitSupport.pending}
        />
      )}

      <SettingsSection id="settings-organization-members" title="Members" variant="plain">
        {members.loading && members.value === undefined ? (
          <p role="status" className="px-3 py-6 text-sm text-muted-foreground sm:px-4">
            Loading the members.
          </p>
        ) : members.error !== null ? (
          <div className="px-3 py-6 sm:px-4">
            <ControlError message={refusalMessage(members.error)} />
            <Button size="sm" variant="outline" className="mt-2" onClick={() => void members.refresh()}>
              Try again
            </Button>
          </div>
        ) : (
          <MembersTable
            members={members.value?.members ?? []}
            projects={projects}
            canManage={canManage}
            actorRole={actorRole}
            busyMember={busyMember}
            errorFor={(member) => memberErrors[member.id] ?? null}
            onRoleChange={(member, role) =>
              void runForMember(member, () =>
                api.setMemberRole({ member: member.id, role, key: newKey() }),
              )
            }
            onGrantChange={(member, projectId, grant) =>
              void runForMember(member, () =>
                api.setMemberGrant({
                  member: member.id,
                  projectId,
                  write: grant?.write ?? false,
                  runner: grant?.runner ?? false,
                  revoke: grant === null,
                  key: newKey(),
                }),
              )
            }
            onRemove={(member) => setRemoving(member)}
          />
        )}
      </SettingsSection>

      <SettingsSection id="settings-organization-invitations" title="Invitations">
        <ControlError message={editInvitation.error?.message ?? null} />
        {(members.value?.invitations ?? []).map((invitation) => (
          <SettingsRow
            key={invitation.id}
            title={invitation.email}
            description={`Invited as ${invitation.role}. Expires ${invitation.expires_at}.`}
          >
            <ProjectAccess
              projects={projects}
              grants={invitation.grants ?? []}
              identity={invitation.email}
              canManage={canManage}
              busy={editInvitation.pending || invitationAction.pending}
              onChange={(projectId, grant) => void editInvitation.call({
                invitation: invitation.id,
                grants: replaceGrant(invitation.grants ?? [], projectId, grant),
              })}
            />
            {canManage ? (
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  size="sm"
                  variant="outline"
                  disabled={invitationAction.pending}
                  onClick={() => void invitationAction.call(invitation, "resend")}
                >
                  Resend
                </Button>
                <Button
                  size="sm"
                  variant="destructive-outline"
                  disabled={invitationAction.pending}
                  onClick={() => {
                    invitationAction.clearError();
                    setActiveInvitation(invitation.id);
                    setRevoking(invitation);
                  }}
                >
                  Revoke
                </Button>
                {activeInvitation === invitation.id ? (
                  <ControlError message={invitationAction.error?.message ?? null} />
                ) : null}
                {resentInvitation === invitation.id ? <span role="status">Invitation resent.</span> : null}
              </div>
            ) : null}
          </SettingsRow>
        ))}
        {(members.value?.invitations ?? []).length === 0 ? (
          <SettingsRow title="No pending invitations" description="Nobody is waiting to join." />
        ) : null}
        {canManage ? (
          <SettingsRow title="Invite somebody" description="They receive a link and join with their own account.">
            <InviteForm
              projects={projects}
              actorRole={actorRole}
              onInvite={async (input) => (await invite.call(input)) !== null}
              pending={invite.pending}
              error={invite.error?.message ?? null}
            />
          </SettingsRow>
        ) : null}
      </SettingsSection>

      <Dialog
        open={revoking !== null}
        onOpenChange={(open) => {
          if (!open) setRevoking(null);
        }}
      >
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>Revoke invitation to {revoking?.email}?</DialogTitle>
            <DialogDescription>
              The invitation link will no longer let them join this organization.
            </DialogDescription>
          </DialogHeader>
          <DialogPanel>
            <ControlError message={invitationAction.error?.message ?? null} />
          </DialogPanel>
          <DialogFooter>
            <DialogClose
              render={
                <Button variant="outline" disabled={invitationAction.pending}>
                  Keep invitation
                </Button>
              }
            />
            <Button
              variant="destructive"
              disabled={invitationAction.pending}
              onClick={() => {
                if (revoking !== null) void invitationAction.call(revoking, "revoke");
              }}
            >
              {invitationAction.pending ? "Revoking…" : "Revoke invitation"}
            </Button>
          </DialogFooter>
        </DialogPopup>
      </Dialog>

      <RemoveMemberDialog
        member={removing}
        pending={busyMember !== null}
        error={removeError}
        onOpenChange={(open) => {
          if (!open) setRemoving(null);
        }}
        onConfirm={(member) => {
          void runForMember(member, () =>
            api.removeMember({ member: member.id, key: newKey() }),
          ).then(() => {
            // A refusal keeps the dialog open so the reason is read where the
            // decision was made; a success closes it.
            setMemberErrors((current) => {
              if (current[member.id] === undefined) setRemoving(null);
              return current;
            });
          });
        }}
      />
    </SettingsPageContainer>
  );
}

export function SlugSettings({ projectId, canManage }: { readonly projectId?: string; readonly canManage: boolean }): React.ReactElement {
  const api = useAccountApi();
  const shared = behindSharedEntry();
  const [current, setCurrent] = React.useState("");
  const [draft, setDraft] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [busy, setBusy] = React.useState(false);
  React.useEffect(() => {
    let active = true;
    void api.slug(projectId, shared).then(({ slug }) => { if (active) { setCurrent(slug); setDraft(slug); } }).catch((error: unknown) => { if (active) setError(String(error)); });
    return () => { active = false; };
  }, [api, projectId, shared]);
  async function save(): Promise<void> {
    setBusy(true); setError(null);
    try {
      await api.saveSlug(draft, current, projectId, shared);
      window.location.reload();
    } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
    finally { setBusy(false); }
  }
  return <SettingsSection title="URL slug">
    <p className="text-sm text-muted-foreground">Earlier slugs keep redirecting to the current address.</p>
    <SettingsRow title={projectId === undefined ? "Organization slug" : "Project slug"}>
      <div className="flex flex-col gap-2">
        <div className="flex items-center gap-2">
          <Input aria-label={projectId === undefined ? "Organization slug" : "Project slug"} value={draft} onChange={(event) => setDraft(event.target.value)} disabled={!canManage || busy || current === ""} maxLength={80} />
          {canManage ? <Button size="sm" disabled={busy || current === "" || draft === current} onClick={() => void save()}>{busy ? "Saving…" : "Save slug"}</Button> : null}
        </div>
        <ControlError message={error} />
      </div>
    </SettingsRow>
  </SettingsSection>;
}
