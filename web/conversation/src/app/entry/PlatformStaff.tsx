import React from "react";
import { Plus } from "lucide-react";

import { AlertDialog, AlertDialogPopup, AlertDialogHeader, AlertDialogTitle, AlertDialogDescription, AlertDialogFooter } from "../../components/ui/alert-dialog.tsx";
import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Dialog, DialogPopup, DialogHeader, DialogTitle, DialogDescription, DialogFooter } from "../../components/ui/dialog.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Select, SelectTrigger, SelectValue, SelectPopup, SelectItem } from "../../components/ui/select.tsx";
import { Spinner } from "../../components/ui/spinner.tsx";
import { Table, TableHeader, TableHead, TableBody, TableRow, TableCell } from "../../components/ui/table.tsx";
import { Textarea } from "../../components/ui/textarea.tsx";
import { ToastProvider, toastManager } from "../../components/ui/toast.tsx";
import { TooltipProvider, Tooltip, TooltipTrigger, TooltipPopup } from "../../components/ui/tooltip.tsx";
import { AccountError } from "../account/api.ts";
import { newKey } from "../account/idempotency.ts";
import { type PlatformMember, type PlatformMemberChange, type PlatformMembers } from "./api.ts";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import { redirectPlatformSignIn, usePlatformResource } from "./usePlatformResource.ts";

const ROLES = ["admin", "support", "billing", "viewer"] as const;
const roleLabel = (role: string) => role.charAt(0).toUpperCase() + role.slice(1);
export const STALE_STAFF_MESSAGE = "Someone changed the staff list; reload";

type Edit = { action: "add" } | { action: "change" | "remove"; member: PlatformMember; role: string };

function RoleSelect({ value, onChange, label, id, disabled = false }: {
  readonly value: string;
  readonly onChange: (role: string) => void;
  readonly label: string;
  readonly id?: string;
  readonly disabled?: boolean;
}): React.ReactElement {
  return (
    <Select value={value} disabled={disabled} onValueChange={(role) => { if (role) onChange(role); }}>
      <SelectTrigger id={id} aria-label={label}><SelectValue>{roleLabel(value)}</SelectValue></SelectTrigger>
      <SelectPopup>{ROLES.map((role) => <SelectItem key={role} value={role}>{roleLabel(role)}</SelectItem>)}</SelectPopup>
    </Select>
  );
}

function MemberDialog({ edit, value, csrf, close, saved, stale }: {
  readonly edit: Edit;
  readonly value: PlatformMembers;
  readonly csrf: string;
  readonly close: () => void;
  readonly saved: (message: string) => Promise<void>;
  readonly stale: () => void;
}): React.ReactElement {
  const api = useEntryApi();
  const [email, setEmail] = React.useState(edit.action === "add" ? "" : edit.member.email);
  const [role, setRole] = React.useState(edit.action === "add" ? "viewer" : edit.role);
  const [reason, setReason] = React.useState("");
  const [error, setError] = React.useState<string | null>(null);
  const [pending, setPending] = React.useState(false);
  const command = React.useRef<PlatformMemberChange | null>(null);
  const submitting = React.useRef(false);
  const title = edit.action === "add" ? "Add member" : edit.action === "remove" ? `Remove ${email}` :
    `Change role for ${email} from ${roleLabel(edit.member.role)} to ${roleLabel(role)}`;
  const actionLabel = edit.action === "add" ? "Add" : edit.action === "remove" ? "Remove member" : "Change role";

  async function submit(event: React.FormEvent) {
    event.preventDefault();
    if (submitting.current) return;
    const normalizedEmail = email.trim().toLowerCase();
    if (edit.action === "add" && (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(normalizedEmail) || normalizedEmail.length > 254)) {
      setError("Enter a valid email address.");
      return;
    }
    const normalizedReason = reason.trim();
    if (!normalizedReason || Array.from(normalizedReason).length > 500) {
      setError("Give a reason of 1–500 characters.");
      return;
    }
    command.current ??= {
      action: edit.action, email: normalizedEmail, ...(edit.action === "remove" ? {} : { role }),
      reason: normalizedReason, expected_revision: value.revision, idempotency_key: newKey(),
    };
    submitting.current = true;
    setPending(true);
    setError(null);
    try {
      await api.changePlatformMember({ csrf, change: command.current });
      close();
      await saved(edit.action === "add" ? `Added ${normalizedEmail} as ${roleLabel(role)}` :
        edit.action === "remove" ? `Removed ${normalizedEmail}` : `Changed ${normalizedEmail} to ${roleLabel(role)}`);
    } catch (cause) {
      redirectPlatformSignIn(cause);
      if (cause instanceof AccountError && cause.code === "revision_conflict") {
        close();
        stale();
      } else {
        if (cause instanceof AccountError && cause.status >= 400 && cause.status < 500 && cause.code !== "outcome_uncertain") {
          command.current = null;
        }
        setError(cause instanceof AccountError && cause.code === "already_member" ? "Already a member" :
          cause instanceof Error ? cause.message : "The membership change could not be confirmed. Retry to verify it.");
      }
    } finally {
      submitting.current = false;
      setPending(false);
    }
  }

  const locked = pending || command.current !== null;
  const fields = (
    <div className="grid gap-4 p-6">
      {edit.action === "add" ? <>
        <div className="grid gap-1.5">
          <Label htmlFor="staff-email">Email</Label>
          <Input id="staff-email" type="email" maxLength={254} autoComplete="email" value={email} disabled={locked}
            onChange={(event) => { setEmail(event.target.value); setError(null); }} />
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="staff-role">Role</Label>
          <RoleSelect id="staff-role" label="Role" value={role} onChange={setRole} disabled={locked} />
        </div>
      </> : null}
      <div className="grid gap-1.5">
        <Label htmlFor="staff-reason">Reason</Label>
        <Textarea id="staff-reason" value={reason} required maxLength={500} disabled={locked}
          onChange={(event) => { setReason(event.target.value); setError(null); }} />
      </div>
      <Problem message={error} />
    </div>
  );
  const buttons = <>
    <Button type="button" variant="outline" disabled={pending} onClick={close}>Cancel</Button>
    <Button type="submit" variant={edit.action === "remove" ? "destructive" : "default"} disabled={pending} aria-busy={pending}>
      {pending ? <Spinner /> : null}{actionLabel}
    </Button>
  </>;
  if (edit.action === "remove") return (
    <AlertDialog open onOpenChange={(open) => { if (!open && !pending) close(); }}>
      <AlertDialogPopup>
        <form onSubmit={(event) => { void submit(event); }} noValidate>
          <AlertDialogHeader><AlertDialogTitle>{title}</AlertDialogTitle>
            <AlertDialogDescription>This member will lose access to the platform console.</AlertDialogDescription></AlertDialogHeader>
          {fields}<AlertDialogFooter>{buttons}</AlertDialogFooter>
        </form>
      </AlertDialogPopup>
    </AlertDialog>
  );
  return (
    <Dialog open onOpenChange={(open) => { if (!open && !pending) close(); }}>
      <DialogPopup showCloseButton={!pending}>
        <form onSubmit={(event) => { void submit(event); }} noValidate>
          <DialogHeader><DialogTitle>{title}</DialogTitle>
            <DialogDescription>Give a reason for the platform membership change.</DialogDescription></DialogHeader>
          {fields}<DialogFooter>{buttons}</DialogFooter>
        </form>
      </DialogPopup>
    </Dialog>
  );
}

function Staff({ csrf }: { readonly csrf: string }): React.ReactElement {
  const api = useEntryApi();
  const resource = usePlatformResource(() => api.platformMembers(), [api]);
  const [edit, setEdit] = React.useState<Edit | null>(null);
  const [needsReload, setNeedsReload] = React.useState(false);
  const value = resource.value;
  const admins = value?.members.filter((member) => member.role === "admin").length ?? 0;
  const unavailable = needsReload || resource.loading || resource.error !== null;

  function stale() {
    setNeedsReload(true);
    const id = toastManager.add({ title: STALE_STAFF_MESSAGE, type: "warning", timeout: 0,
      actionProps: { children: "Reload", onClick: () => {
        void resource.refresh().then(() => { setNeedsReload(false); toastManager.close(id); });
      } },
    });
  }

  return <>
    <div className="flex flex-wrap items-start justify-between gap-3">
      <div className="grid gap-2">
        <h1 className="text-2xl font-semibold tracking-[-0.02em]">Staff</h1>
        <p className="text-sm text-muted-foreground">Platform roles are separate from organization membership.</p>
        <p className="max-w-3xl text-sm text-muted-foreground">Admin manages staff and everything below. Support may start support access. Billing may grant and revoke plans. Viewer reads the console.</p>
      </div>
      <Button onClick={() => setEdit({ action: "add" })} disabled={unavailable || value === undefined}><Plus />Add member</Button>
    </div>
    <Problem message={resource.error?.message ?? null} />
    {resource.error ? <Button variant="outline" disabled={resource.loading} onClick={() => { void resource.refresh(); }}>Reload</Button> : null}
    {value === undefined ? resource.loading ? <p className="text-sm text-muted-foreground">Loading staff…</p> : null :
      <Table aria-label="Platform members">
        <TableHeader><TableRow><TableHead>Member</TableHead><TableHead>Role</TableHead><TableHead>Added by</TableHead><TableHead>Added</TableHead><TableHead><span className="sr-only">Actions</span></TableHead></TableRow></TableHeader>
        <TableBody>{value.members.map((member) => {
          const own = member.email === value.self.email;
          const lastAdmin = member.role === "admin" && admins === 1;
          const protection = own ? "You cannot change your own role" : lastAdmin ? "At least one admin is required" :
            member.bootstrap ? "The bootstrap admin cannot be demoted" : null;
          const picker = <RoleSelect label={`Role for ${member.email}`} value={member.role} disabled={unavailable || protection !== null}
            onChange={(role) => { if (role !== member.role) setEdit({ action: "change", member, role }); }} />;
          return <TableRow key={member.email}>
            <TableCell><div className="flex flex-wrap items-center gap-2"><span>{member.email}</span>{own ? <Badge variant="outline">you</Badge> : null}{member.bootstrap ? <span className="text-muted-foreground">bootstrap</span> : null}</div></TableCell>
            <TableCell>{protection ? <Tooltip><TooltipTrigger render={<span tabIndex={0} aria-label={protection} />}>{picker}</TooltipTrigger><TooltipPopup>{protection}</TooltipPopup></Tooltip> : picker}</TableCell>
            <TableCell>{member.added_by}</TableCell><TableCell>{member.added_at.slice(0, 10)}</TableCell>
            <TableCell>{own || member.bootstrap || lastAdmin ? null : <Button variant="ghost-destructive" disabled={unavailable} onClick={() => setEdit({ action: "remove", member, role: member.role })}>Remove</Button>}</TableCell>
          </TableRow>;
        })}</TableBody>
      </Table>}
    {edit && value ? <MemberDialog edit={edit} value={value} csrf={csrf} close={() => setEdit(null)} stale={stale}
      saved={async (message) => { toastManager.add({ title: message, type: "success" }); await resource.refresh(); }} /> : null}
  </>;
}

export function PlatformStaff({ csrf }: { readonly csrf: string }): React.ReactElement {
  return <ToastProvider><TooltipProvider><Staff csrf={csrf} /></TooltipProvider></ToastProvider>;
}
