import React from "react";
import { Link, useNavigate, useParams, useRouterState } from "@tanstack/react-router";

import { Badge } from "../../components/ui/badge.tsx";
import { Empty, EmptyHeader, EmptyTitle } from "../../components/ui/empty.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { SettingsRow, SettingsSection } from "../settings/settingsLayout.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { type PlatformUsers } from "./api.ts";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import { PlatformRoleContext } from "./PlatformConsole.tsx";
import { usePlatformResource } from "./usePlatformResource.ts";

function date(value: string, time = false): string {
  if (value === "") return "—";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  const iso = parsed.toISOString();
  return time ? `${iso.slice(0, 10)} ${iso.slice(11, 16)}Z` : iso.slice(0, 10);
}

function PlatformRole({ role }: { readonly role: string }): React.ReactElement {
  return role === "" ? <span className="text-muted-foreground">None</span> : <Badge variant="info">{role}</Badge>;
}

function Unsearched({ value }: { readonly value: PlatformUsers }): React.ReactElement | null {
  return value.unsearched.length === 0 ? null : (
    <div role="status" className="space-y-1 text-sm text-warning-foreground">
      {value.unsearched.map((organization) => <p key={organization.organization_id}>Not searched: {organization.organization_name}</p>)}
    </div>
  );
}

export function PlatformUserResults({ value, query }: { readonly value: PlatformUsers; readonly query: string }): React.ReactElement {
  return (
    <div className="flex min-w-0 flex-col gap-6" aria-live="polite">
      <SettingsSection title="Users" headerAction={<Badge variant="outline">{value.users.length}</Badge>}>
        <Table aria-label="Users">
          <TableHeader><TableRow>
            <TableHead>User</TableHead><TableHead>Platform role</TableHead><TableHead className="text-right">Organizations</TableHead><TableHead>Last sign-in</TableHead>
          </TableRow></TableHeader>
          <TableBody>
            {value.users.map((user) => <TableRow key={user.email}>
              <TableCell><Link to={"/platform/users/$email" as never} params={{ email: user.email } as never} search={{ q: query } as never} className="font-medium underline-offset-4 hover:underline focus-visible:underline">{user.email}</Link></TableCell>
              <TableCell><PlatformRole role={user.platform_role} /></TableCell>
              <TableCell className="text-right"><Badge variant="outline">{user.memberships.length}</Badge></TableCell>
              <TableCell><time dateTime={user.last_sign_in_at || undefined}>{date(user.last_sign_in_at, true)}</time></TableCell>
            </TableRow>)}
            {value.users.length === 0 ? <TableRow><TableCell colSpan={4}><Empty size="compact"><EmptyHeader><EmptyTitle>No users match</EmptyTitle></EmptyHeader></Empty></TableCell></TableRow> : null}
          </TableBody>
        </Table>
      </SettingsSection>
      {value.users.length === 50 ? <p className="text-sm text-muted-foreground">Showing up to 50 users. Refine your search to find other users.</p> : null}
      <Unsearched value={value} />
    </div>
  );
}

export function PlatformUsersPage(): React.ReactElement {
  usePageTitle("Users", "Platform");
  const api = useEntryApi();
  const navigate = useNavigate();
  const routeQuery = useRouterState({ select: (state) => (state.location.search as { q?: string }).q ?? "" });
  const [query, setQuery] = React.useState(routeQuery);
  React.useEffect(() => { setQuery(routeQuery); }, [routeQuery]);
  React.useEffect(() => {
    if (query.trim() === routeQuery) return;
    const timer = setTimeout(() => {
      void navigate({ to: "/platform/users", search: { q: query.trim() || undefined }, resetScroll: false } as never);
    }, 300);
    return () => clearTimeout(timer);
  }, [query, routeQuery, navigate]);
  const submitted = routeQuery.trim();
  const valid = Array.from(submitted).length >= 3;
  const resource = usePlatformResource(() => valid ? api.platformUsers(submitted) : Promise.resolve(null), [api, submitted, valid]);
  const loading = valid && resource.loading;
  const value = loading || resource.error !== null ? null : resource.value;

  return (
    <>
      <form onSubmit={(event) => {
        event.preventDefault();
        if (query.trim() === submitted) {
          if (valid) void resource.refresh();
          return;
        }
        void navigate({ to: "/platform/users", search: { q: query.trim() || undefined }, resetScroll: false } as never);
      }} className="flex flex-col gap-2">
        <Label htmlFor="user-email">Search by email</Label>
        <div className="flex flex-col gap-2 sm:flex-row sm:items-center sm:gap-3">
          <Input id="user-email" type="search" placeholder="Search by email" value={query} onChange={(event) => setQuery(event.target.value)} aria-describedby="user-search-help" className="min-w-0 sm:max-w-sm" />
          <p id="user-search-help" className="text-xs text-muted-foreground">Enter at least three characters. Matches verified emails across all tenants. Search stays in the URL.</p>
        </div>
      </form>
      <Problem message={resource.error?.message ?? null} />
      {loading ? <div role="status" aria-label="Searching users" className="space-y-3"><Skeleton className="h-6 w-1/2" /><Skeleton className="h-24 w-full" /></div> : null}
      {value == null ? null : <PlatformUserResults value={value} query={submitted} />}
    </>
  );
}

export function PlatformUserPage(): React.ReactElement {
  const { email } = useParams({ strict: false }) as { email: string };
  usePageTitle(email, "Platform");
  const api = useEntryApi();
  const role = React.useContext(PlatformRoleContext);
  const resource = usePlatformResource(() => api.platformUsers(email), [api, email]);
  const value = resource.loading || resource.error !== null ? null : resource.value;
  const user = value?.users.find((candidate) => candidate.email.toLowerCase() === email.toLowerCase());

  return (
    <>
      <Problem message={resource.error?.message ?? null} />
      {resource.loading ? <Skeleton className="h-48" aria-label="Loading user" /> : null}
      {value == null ? null : user === undefined ? <><Empty><EmptyHeader><EmptyTitle>User not found</EmptyTitle></EmptyHeader></Empty><Unsearched value={value} /></> : <>
        <SettingsSection title="Account" aria-label="Account">
          <SettingsRow title="Email" control={<span className="min-w-0 break-all text-sm">{user.email}</span>} />
          <SettingsRow title="Platform role" control={<><PlatformRole role={user.platform_role} />{role === "admin" ? <Link to={"/platform/staff" as never} className="text-sm text-primary underline underline-offset-4">Manage in Staff</Link> : null}</>} />
          <SettingsRow title="Last sign-in" control={<time className="text-sm" dateTime={user.last_sign_in_at || undefined}>{date(user.last_sign_in_at, true)}</time>} />
          <SettingsRow title="Identity subject" control={<span className="min-w-0 break-all font-mono text-xs">{user.subject || "—"}</span>} />
        </SettingsSection>
        <SettingsSection title="Organizations" aria-label="Organizations" headerAction={<Badge variant="outline">{user.memberships.length}</Badge>}>
          {user.memberships.length === 0 ? <Empty size="compact"><EmptyHeader><EmptyTitle>No organization membership</EmptyTitle></EmptyHeader></Empty> : user.memberships.map((membership) => <SettingsRow key={membership.organization_id}
            title={<Link to={"/platform/tenants/$organization" as never} params={{ organization: membership.organization_id } as never} className="break-words underline-offset-4 hover:underline focus-visible:underline">{membership.organization_name}</Link>}
            description={<>Joined <time dateTime={membership.joined_at}>{date(membership.joined_at)}</time></>}
            control={<Badge variant="outline">{membership.role}</Badge>} />)}
        </SettingsSection>
        <SettingsSection title="Pending invitations" aria-label="Pending invitations" headerAction={<Badge variant="outline">{user.invitations.length}</Badge>}>
          {user.invitations.length === 0 ? <Empty size="compact"><EmptyHeader><EmptyTitle>No pending invitations</EmptyTitle></EmptyHeader></Empty> : user.invitations.map((invitation, index) => <SettingsRow key={`${invitation.organization_id}:${invitation.expires_at}:${index}`}
            title={<Link to={"/platform/tenants/$organization" as never} params={{ organization: invitation.organization_id } as never} className="break-words underline-offset-4 hover:underline focus-visible:underline">{invitation.organization_name}</Link>}
            description={<>Expires <time dateTime={invitation.expires_at}>{date(invitation.expires_at)}</time></>}
            control={<Badge variant="warning">invited</Badge>} />)}
        </SettingsSection>
        <Unsearched value={value} />
      </>}
    </>
  );
}
