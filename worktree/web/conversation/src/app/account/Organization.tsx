import { useProjectSettingsDirty } from "../settings/projectSettingsDirty.ts";
import React from "react";
import { MoreHorizontalIcon, PlusIcon, XIcon } from "lucide-react";

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
import { Badge } from "../../components/ui/badge.tsx";
import {
  InputGroup,
  InputGroupAddon,
  InputGroupInput,
} from "../../components/ui/input-group.tsx";
import {
  Menu,
  MenuItem,
  MenuPopup,
  MenuTrigger,
} from "../../components/ui/menu.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
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
  accessSummary,
  MemberAccessSheet,
  MemberIdentity,
  memberIdentity,
  ProjectAccessMatrix,
  ProjectMarks,
  RolePicker,
  type AccessProject,
} from "./MemberAccess.tsx";
import type {
  Invitation,
  Member,
  MemberRole,
  ProjectGrant,
} from "../../contracts/account.ts";
import {
  SettingsPageContainer,
  SettingsRow,
  SettingsSection,
} from "../settings/settingsLayout.tsx";
import { AccountError } from "./api.ts";
import { ControlError, NativeSelect } from "./controls.tsx";
import { useAccountApi, useAccountBootstrap } from "./context.ts";
import { newKey } from "./idempotency.ts";
import { behindSharedEntry } from "../entry/shared.ts";
import { useOrganizationMembers } from "./OrganizationMembers.tsx";
import { useMutation } from "./useResource.ts";
import { signInPath } from "../../runtime/basePath.ts";

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
    <AlertDialog
      open={member !== null}
      onOpenChange={(open) => {
        if (!pending) onOpenChange(open);
      }}
    >
      <AlertDialogPopup>
        <AlertDialogHeader>
          <AlertDialogTitle>
            Remove {member === null ? "this member" : memberIdentity(member)}?
          </AlertDialogTitle>
          <AlertDialogDescription>
            They lose access to every project in this organization immediately.
            Their work stays.
          </AlertDialogDescription>
          <ControlError message={error} />
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogClose
            render={
              <Button variant="outline" disabled={pending}>
                Keep them
              </Button>
            }
          />
          <Button
            variant="destructive"
            disabled={pending || member === null}
            aria-busy={pending}
            onClick={() => {
              if (member !== null) onConfirm(member);
            }}
          >
            Remove
          </Button>
        </AlertDialogFooter>
      </AlertDialogPopup>
    </AlertDialog>
  );
}

export function MembersTable({
  members,
  projects,
  roles,
  actorId,
  canManage,
  actorRole,
  onRoleChange,
  onEdit,
  onRemove,
  busyMember,
  errorFor,
}: {
  readonly members: readonly Member[];
  readonly projects: readonly AccessProject[];
  readonly roles: readonly MemberRole[];
  readonly actorId?: string;
  readonly canManage: boolean;
  readonly actorRole: string;
  readonly onRoleChange: (member: Member, role: string) => void;
  readonly onEdit: (member: Member) => void;
  readonly onRemove: (member: Member) => void;
  readonly busyMember: string | null;
  readonly errorFor: (member: Member) => string | null;
}): React.ReactElement {
  return (
    <div className="min-w-0 overflow-hidden rounded-lg border">
      <Table className="min-w-150">
        <TableHeader>
          <TableRow>
            <TableHead>Member</TableHead>
            <TableHead>Role</TableHead>
            <TableHead>Project access</TableHead>
            {canManage ? (
              <TableHead>
                <span className="sr-only">Actions</span>
              </TableHead>
            ) : null}
          </TableRow>
        </TableHeader>
        <TableBody>
          {members.map((member) => (
            <TableRow key={member.id}>
              <TableCell>
                <MemberIdentity
                  member={member}
                  isActor={member.user_id === actorId}
                />
                <ControlError message={errorFor(member)} />
              </TableCell>
              <TableCell>
                {canManage && member.role !== "owner" ? (
                  <RolePicker
                    roles={roles}
                    value={member.role}
                    actorRole={actorRole}
                    label={`Role for ${memberIdentity(member)}`}
                    disabled={busyMember !== null}
                    onChange={(role) => onRoleChange(member, role)}
                  />
                ) : (
                  <span className="text-sm capitalize text-muted-foreground">
                    {roles.find((role) => role.slug === member.role)?.name ??
                      member.role}
                  </span>
                )}
              </TableCell>
              <TableCell>
                {canManage ? (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={busyMember !== null}
                    aria-label={`Project access for ${memberIdentity(member)}: ${accessSummary(projects, member.grants)}`}
                    onClick={() => onEdit(member)}
                  >
                    <ProjectMarks projects={projects} grants={member.grants} />
                    {accessSummary(projects, member.grants)}
                  </Button>
                ) : (
                  <span className="flex items-center gap-2 text-sm">
                    <ProjectMarks projects={projects} grants={member.grants} />
                    {accessSummary(projects, member.grants)}
                  </span>
                )}
              </TableCell>
              {canManage ? (
                <TableCell>
                  <Menu>
                    <MenuTrigger
                      render={
                        <Button
                          variant="ghost"
                          size="icon-sm"
                          aria-label={`Actions for ${memberIdentity(member)}`}
                          disabled={busyMember !== null}
                        >
                          <MoreHorizontalIcon />
                        </Button>
                      }
                    />
                    <MenuPopup align="end">
                      <MenuItem onClick={() => onEdit(member)}>
                        Edit project access
                      </MenuItem>
                      <MenuItem
                        variant="destructive"
                        onClick={() => onRemove(member)}
                      >
                        Remove from organization
                      </MenuItem>
                    </MenuPopup>
                  </Menu>
                </TableCell>
              ) : null}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {members.length === 0 ? (
        <p role="status" className="px-4 py-8 text-sm text-muted-foreground">
          No matching members.
        </p>
      ) : null}
    </div>
  );
}

function splitEmails(value: string): string[] {
  return value
    .split(/[,\s]+/)
    .map((email) => email.trim().toLowerCase())
    .filter(Boolean);
}

export function InviteForm({
  onInvite,
  onDone,
  onCancel,
  projects = [],
  roles,
  actorRole = "owner",
  pending = false,
  error = null,
}: {
  readonly onInvite: (input: {
    email: string;
    role: string;
    grants: readonly ProjectGrant[];
  }) => Promise<boolean>;
  readonly onDone?: () => void;
  readonly onCancel?: () => void;
  readonly projects?: readonly AccessProject[];
  readonly roles: readonly MemberRole[];
  readonly actorRole?: string;
  readonly pending?: boolean;
  readonly error?: string | null;
}): React.ReactElement {
  const initialRole =
    roles.find((role) => role.slug === "member")?.slug ??
    roles.find((role) => role.slug !== "owner" || actorRole === "owner")
      ?.slug ??
    "";
  const [draft, setDraft] = React.useState("");
  const [emails, setEmails] = React.useState<string[]>([]);
  const [role, setRole] = React.useState(initialRole);
  const [grants, setGrants] = React.useState<ProjectGrant[]>([]);
  const [sending, setSending] = React.useState(false);
  const [feedback, setFeedback] = React.useState<string | null>(null);
  const [validation, setValidation] = React.useState<string | null>(null);
  const submitting = React.useRef(false);
  const busy = pending || sending;
  const addresses = [...new Set([...emails, ...splitEmails(draft)])];
  const inputId = React.useId();
  const commitEmails = (value: string) => {
    setEmails((emails) => [...new Set([...emails, ...splitEmails(value)])]);
    setDraft("");
    setValidation(null);
  };
  return (
    <form
      className="flex min-h-0 flex-col"
      onSubmit={async (event) => {
        event.preventDefault();
        if (busy || submitting.current || addresses.length === 0 || role === "")
          return;
        if (
          addresses.some((email) => !/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email))
        ) {
          setValidation("Enter a valid email address for each invitation.");
          return;
        }
        submitting.current = true;
        setSending(true);
        setValidation(null);
        setFeedback(null);
        setEmails(addresses);
        setDraft("");
        const failed: string[] = [];
        try {
          for (const email of addresses) {
            if (!(await onInvite({ email, role, grants }))) failed.push(email);
            else
              setEmails((current) =>
                current.filter((address) => address !== email),
              );
          }
          setEmails(failed);
          const sent = addresses.length - failed.length;
          setFeedback(
            `${sent} invitation${sent === 1 ? "" : "s"} sent.${failed.length > 0 ? ` ${failed.length} could not be sent. Retry the remaining addresses.` : ""}`,
          );
          if (failed.length === 0) {
            setRole(initialRole);
            setGrants([]);
            onDone?.();
          }
        } catch {
          setValidation(
            "Sending stopped. Check pending invitations before sending again.",
          );
        } finally {
          submitting.current = false;
          setSending(false);
        }
      }}
    >
      <DialogPanel className="flex flex-col gap-6 space-y-0">
        <div className="flex flex-col gap-2">
          <Label htmlFor={inputId}>Email addresses</Label>
          <InputGroup>
            {emails.length > 0 ? (
              <InputGroupAddon align="block-start" className="flex-wrap">
                {emails.map((email) => (
                  <Badge key={email} variant="secondary">
                    <span className="max-w-52 truncate" title={email}>
                      {email}
                    </span>
                    <Button
                      variant="ghost"
                      size="icon-xs"
                      aria-label={`Remove ${email}`}
                      disabled={busy}
                      onClick={() =>
                        setEmails((emails) =>
                          emails.filter((current) => current !== email),
                        )
                      }
                    >
                      <XIcon />
                    </Button>
                  </Badge>
                ))}
              </InputGroupAddon>
            ) : null}
            <InputGroupInput
              id={inputId}
              value={draft}
              disabled={busy}
              placeholder="Add an address, or paste a list"
              aria-describedby={`${inputId}-hint`}
              onChange={(event) => {
                const value = event.currentTarget.value;
                if (/[,\s]$/.test(value)) commitEmails(value);
                else {
                  setDraft(value);
                  setValidation(null);
                }
              }}
              onPaste={(event) => {
                const pasted = event.clipboardData.getData("text");
                if (/[,\s]/.test(pasted)) {
                  event.preventDefault();
                  commitEmails(draft + pasted);
                }
              }}
              onKeyDown={(event) => {
                if (event.key === "Enter" && draft.trim()) {
                  event.preventDefault();
                  commitEmails(draft);
                }
              }}
            />
          </InputGroup>
          <p id={`${inputId}-hint`} className="text-xs text-muted-foreground">
            Separate addresses with commas or spaces.
          </p>
        </div>
        <div className="flex flex-col gap-2">
          <Label>Role</Label>
          <div className="max-w-56">
            <RolePicker
              roles={roles}
              value={role}
              actorRole={actorRole}
              label="Role"
              disabled={busy}
              onChange={setRole}
            />
          </div>
          <p className="text-sm text-muted-foreground">
            {roles.find((option) => option.slug === role)?.description}
          </p>
        </div>
        <div className="flex flex-col gap-2">
          <h3 className="text-sm font-medium">Project access</h3>
          <ProjectAccessMatrix
            projects={projects}
            grants={grants}
            identity="invitation"
            disabled={busy}
            onChange={setGrants}
          />
        </div>
        <ControlError message={validation ?? error} />
        {feedback === null ? null : (
          <p role="status" className="text-sm text-muted-foreground">
            {feedback}
          </p>
        )}
      </DialogPanel>
      <DialogFooter>
        {onCancel === undefined ? null : (
          <Button variant="outline" disabled={busy} onClick={onCancel}>
            Cancel
          </Button>
        )}
        <Button
          type="submit"
          disabled={busy || addresses.length === 0 || role === ""}
          aria-busy={busy}
        >
          Send {addresses.length} invitation{addresses.length === 1 ? "" : "s"}
        </Button>
      </DialogFooter>
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
  const [editingId, setEditingId] = React.useState<string | null>(null);
  const [busyMember, setBusyMember] = React.useState<string | null>(null);
  const [memberErrors, setMemberErrors] = React.useState<
    Record<string, string>
  >({});
  const [revoking, setRevoking] = React.useState<Invitation | null>(null);
  const [activeInvitation, setActiveInvitation] = React.useState<string | null>(
    null,
  );
  const [resentInvitation, setResentInvitation] = React.useState<string | null>(
    null,
  );
  const [inviteOpen, setInviteOpen] = React.useState(false);
  const [search, setSearch] = React.useState("");
  const [roleFilter, setRoleFilter] = React.useState("all");
  const [editingInvitation, setEditingInvitation] =
    React.useState<Invitation | null>(null);
  const [invitationGrants, setInvitationGrants] = React.useState<
    readonly ProjectGrant[]
  >([]);
  const inviteKeys = React.useRef(new Map<string, string>());
  const mutationActive = React.useRef(false);
  const canManage = bootstrap?.actor.can_manage ?? false;
  const actorRole = bootstrap?.actor.role ?? "viewer";
  const projects = bootstrap?.projects ?? [];
  const roles = members.value?.roles ?? [];
  const editing = members.value?.members.find(
    (member) => member.id === editingId,
  );
  const filtered = (members.value?.members ?? []).filter(
    (member) =>
      (roleFilter === "all" || member.role === roleFilter) &&
      `${member.email} ${member.name ?? ""}`
        .toLowerCase()
        .includes(search.trim().toLowerCase()),
  );

  const runForMember = async (
    member: Member,
    work: () => Promise<unknown>,
  ): Promise<boolean> => {
    if (mutationActive.current) return false;
    mutationActive.current = true;
    setBusyMember(member.id);
    setMemberErrors((current) => {
      const next = { ...current };
      delete next[member.id];
      return next;
    });
    try {
      await work();
      await members.refresh();
      return true;
    } catch (cause) {
      const message =
        cause instanceof AccountError
          ? refusalMessage(cause)
          : "Could not save this member. Try again.";
      setMemberErrors((current) => ({
        ...current,
        [member.id]: message ?? "Could not save this member.",
      }));
      await members.refresh();
      return false;
    } finally {
      mutationActive.current = false;
      setBusyMember(null);
    }
  };

  const invite = useMutation(
    async (input: {
      email: string;
      role: string;
      grants: readonly ProjectGrant[];
    }) => {
      const identity = JSON.stringify(input);
      const key = inviteKeys.current.get(identity) ?? newKey();
      inviteKeys.current.set(identity, key);
      const created = await api.invite({ ...input, key });
      inviteKeys.current.delete(identity);
      await members.refresh();
      return created;
    },
  );
  const editInvitation = useMutation(async () => {
    if (editingInvitation === null) return;
    await api.setInvitationGrants({
      invitation: editingInvitation.id,
      grants: invitationGrants,
      key: newKey(),
    });
    await members.refresh();
    setEditingInvitation(null);
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
  const openInvite = () => {
    invite.clearError();
    setInviteOpen(true);
  };

  return (
    <SettingsPageContainer width="wide">
      <div className="flex flex-wrap items-center justify-between gap-3 px-3 sm:px-4">
        <div>
          <h2 className="text-xl font-semibold tracking-tight">Members</h2>
          <p className="mt-1 text-sm text-muted-foreground">
            Who belongs to {bootstrap?.organization.name ?? "this organization"}{" "}
            and what they can reach.
          </p>
        </div>
        {canManage ? (
          <Button
            onClick={openInvite}
            disabled={members.loading || roles.length === 0}
          >
            <PlusIcon />
            Invite member
          </Button>
        ) : null}
      </div>
      {bootstrap?.support == null ? null : (
        <SupportBanner
          actor={bootstrap.support.actor}
          reason={bootstrap.support.reason}
          expiresAt={bootstrap.support.expires_at}
          onExit={() => void exitSupport.call()}
          exiting={exitSupport.pending}
        />
      )}
      <SettingsSection
        id="settings-organization-members"
        title="Members"
        hideTitle
        variant="plain"
      >
        <div className="flex flex-wrap items-center gap-2 pb-3">
          <div className="w-full sm:w-72">
            <Input
              type="search"
              aria-label="Search members"
              placeholder="Search by name or email"
              value={search}
              onChange={(event) => setSearch(event.currentTarget.value)}
            />
          </div>
          <div className="w-full sm:w-40">
            <Select
              items={[
                { value: "all", label: "All roles" },
                ...roles.map((role) => ({
                  value: role.slug,
                  label: role.name,
                })),
              ]}
              value={roleFilter}
              onValueChange={(role) => {
                if (role !== null) setRoleFilter(role);
              }}
            >
              <SelectTrigger aria-label="Filter by role">
                <SelectValue />
              </SelectTrigger>
              <SelectPopup>
                <SelectItem value="all">All roles</SelectItem>
                {roles.map((role) => (
                  <SelectItem key={role.slug} value={role.slug}>
                    {role.name}
                  </SelectItem>
                ))}
              </SelectPopup>
            </Select>
          </div>
        </div>
        {members.loading && members.value === undefined ? (
          <p role="status" className="py-6 text-sm text-muted-foreground">
            Loading the members.
          </p>
        ) : members.error !== null ? (
          <div className="py-6">
            <ControlError message={refusalMessage(members.error)} />
            <Button
              size="sm"
              variant="outline"
              onClick={() => void members.refresh()}
            >
              Try again
            </Button>
          </div>
        ) : (
          <MembersTable
            members={filtered}
            projects={projects}
            roles={roles}
            actorId={bootstrap?.actor.subject}
            canManage={canManage}
            actorRole={actorRole}
            busyMember={busyMember}
            errorFor={(member) => memberErrors[member.id] ?? null}
            onEdit={(member) => setEditingId(member.id)}
            onRemove={setRemoving}
            onRoleChange={(member, role) =>
              void runForMember(member, () =>
                api.setMemberRole({ member: member.id, role, key: newKey() }),
              )
            }
          />
        )}
      </SettingsSection>
      <SettingsSection
        id="settings-organization-invitations"
        title="Pending invitations"
        aria-label="Pending invitations"
      >
        {(members.value?.invitations ?? []).map((invitation) => (
          <SettingsRow
            key={invitation.id}
            role="group"
            aria-label={`Invitation to ${invitation.email}`}
            title={invitation.email}
            description={`Invited as ${roles.find((role) => role.slug === invitation.role)?.name ?? invitation.role}. Expires ${new Date(invitation.expires_at).toLocaleDateString()}.`}
          >
            {canManage ? (
              <div className="flex flex-wrap items-center gap-2">
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => {
                    editInvitation.clearError();
                    setInvitationGrants(invitation.grants ?? []);
                    setEditingInvitation(invitation);
                  }}
                >
                  {accessSummary(projects, invitation.grants ?? [])}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={invitationAction.pending}
                  onClick={() =>
                    void invitationAction.call(invitation, "resend")
                  }
                >
                  Resend
                </Button>
                <Button
                  size="sm"
                  variant="ghost-destructive"
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
                  <ControlError
                    message={invitationAction.error?.message ?? null}
                  />
                ) : null}
                {resentInvitation === invitation.id ? (
                  <span role="status" className="text-sm text-muted-foreground">
                    Invitation resent.
                  </span>
                ) : null}
              </div>
            ) : null}
          </SettingsRow>
        ))}
        {(members.value?.invitations ?? []).length === 0 &&
        !members.loading &&
        members.error === null ? (
          <SettingsRow
            title="No pending invitations"
            description="Invitations you send wait here until they are accepted, with Resend and Revoke."
          >
            {canManage ? (
              <Button
                variant="outline"
                disabled={roles.length === 0}
                onClick={openInvite}
              >
                Invite member
              </Button>
            ) : null}
          </SettingsRow>
        ) : null}
      </SettingsSection>
      {editing === undefined ? null : (
        <MemberAccessSheet
          member={editing}
          projects={projects}
          roles={roles}
          actorRole={actorRole}
          pending={busyMember !== null}
          error={memberErrors[editing.id] ?? null}
          onClose={() => setEditingId(null)}
          onRemove={setRemoving}
          onSave={(member, role, grants) =>
            runForMember(member, async () => {
              if (role !== member.role)
                await api.setMemberRole({
                  member: member.id,
                  role,
                  key: newKey(),
                });
              for (const project of projects) {
                const before = member.grants.find(
                  (grant) => grant.project_id === project.id,
                );
                const after = grants.find(
                  (grant) => grant.project_id === project.id,
                );
                if (
                  before?.write === after?.write &&
                  before?.runner === after?.runner
                )
                  continue;
                await api.setMemberGrant({
                  member: member.id,
                  projectId: project.id,
                  write: after?.write ?? false,
                  runner: after?.runner ?? false,
                  revoke: after === undefined,
                  key: newKey(),
                });
              }
            })
          }
        />
      )}
      <Dialog
        open={inviteOpen}
        onOpenChange={(open) => {
          if (!invite.pending) setInviteOpen(open);
        }}
      >
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>Invite members</DialogTitle>
            <DialogDescription>
              They receive a link and join with their own account.
            </DialogDescription>
          </DialogHeader>
          {inviteOpen ? (
            <InviteForm
              roles={roles}
              projects={projects}
              actorRole={actorRole}
              pending={invite.pending}
              error={invite.error?.message ?? null}
              onInvite={async (input) => (await invite.call(input)) !== null}
              onDone={() => setInviteOpen(false)}
              onCancel={() => setInviteOpen(false)}
            />
          ) : null}
        </DialogPopup>
      </Dialog>
      <Dialog
        open={editingInvitation !== null}
        onOpenChange={(open) => {
          if (!open && !editInvitation.pending) setEditingInvitation(null);
        }}
      >
        <DialogPopup>
          <DialogHeader>
            <DialogTitle>Invitation project access</DialogTitle>
            <DialogDescription>{editingInvitation?.email}</DialogDescription>
          </DialogHeader>
          <DialogPanel className="flex flex-col gap-3">
            <ProjectAccessMatrix
              projects={projects}
              grants={invitationGrants}
              identity={editingInvitation?.email ?? "invitation"}
              disabled={editInvitation.pending}
              onChange={setInvitationGrants}
            />
            <ControlError message={editInvitation.error?.message ?? null} />
          </DialogPanel>
          <DialogFooter>
            <DialogClose
              render={
                <Button variant="outline" disabled={editInvitation.pending}>
                  Cancel
                </Button>
              }
            />
            <Button
              disabled={editInvitation.pending}
              onClick={() => void editInvitation.call()}
            >
              Save access
            </Button>
          </DialogFooter>
        </DialogPopup>
      </Dialog>
      <AlertDialog
        open={revoking !== null}
        onOpenChange={(open) => {
          if (!open && !invitationAction.pending) setRevoking(null);
        }}
      >
        <AlertDialogPopup>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Revoke invitation to {revoking?.email}?
            </AlertDialogTitle>
            <AlertDialogDescription>
              The invitation link will no longer let them join this
              organization.
            </AlertDialogDescription>
            <ControlError message={invitationAction.error?.message ?? null} />
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogClose
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
                if (revoking !== null)
                  void invitationAction.call(revoking, "revoke");
              }}
            >
              Revoke invitation
            </Button>
          </AlertDialogFooter>
        </AlertDialogPopup>
      </AlertDialog>
      <RemoveMemberDialog
        member={removing}
        pending={busyMember !== null}
        error={removing === null ? null : (memberErrors[removing.id] ?? null)}
        onOpenChange={(open) => {
          if (!open) setRemoving(null);
        }}
        onConfirm={(member) =>
          void runForMember(member, () =>
            api.removeMember({ member: member.id, key: newKey() }),
          ).then((success) => {
            if (success) {
              setRemoving(null);
              if (editingId === member.id) setEditingId(null);
            }
          })
        }
      />
    </SettingsPageContainer>
  );
}

export function SlugSettings({ projectId, canManage, embedded = false }: { readonly projectId?: string; readonly canManage: boolean; readonly embedded?: boolean }): React.ReactElement {
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
  useProjectSettingsDirty(current !== "" && draft !== current);
  async function save(): Promise<void> {
    setBusy(true); setError(null);
    try {
      await api.saveSlug(draft, current, projectId, shared);
      window.location.reload();
    } catch (error) { setError(error instanceof Error ? error.message : String(error)); }
    finally { setBusy(false); }
  }
  const editor = <div className="flex flex-col gap-2">
    <div className="flex min-w-0 items-center gap-2">
      <Input size={embedded ? "sm" : "default"} aria-label={projectId === undefined ? "Organization slug" : "Project slug"} value={draft} onChange={(event) => setDraft(event.target.value)} disabled={!canManage || busy || current === ""} maxLength={80} />
      {canManage ? <Button size="sm" disabled={busy || current === "" || draft === current} onClick={() => void save()}>{busy ? "Saving…" : "Save slug"}</Button> : null}
    </div>
    <ControlError message={error} />
  </div>;
  if (embedded) return <SettingsRow title="Project slug" help={{ label: "Project slug", text: "Earlier slugs keep redirecting to the current address." }} control={canManage ? editor : <span className="text-sm font-mono">{current}</span>} />;
  return <SettingsSection title="URL slug">
    <p className="text-sm text-muted-foreground">Earlier slugs keep redirecting to the current address.</p>
    <SettingsRow title={projectId === undefined ? "Organization slug" : "Project slug"}>{editor}</SettingsRow>
  </SettingsSection>;
}
