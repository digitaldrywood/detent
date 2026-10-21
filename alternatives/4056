import React from "react";
import { Link, useNavigate, useParams, useRouterState } from "@tanstack/react-router";

import { Badge } from "../../components/ui/badge.tsx";
import { Empty, EmptyHeader, EmptyTitle } from "../../components/ui/empty.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Select, SelectItem, SelectPopup, SelectTrigger, SelectValue } from "../../components/ui/select.tsx";
import { ToggleGroup, ToggleGroupItem } from "../../components/ui/toggle-group.tsx";
import { ArrowDown, ArrowUp } from "lucide-react";
import { type UserSearch, type UserSort } from "./userList.ts";
import { formatCount } from "../usage/usageFormat.ts";
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

function UserFilter({ label, value, items, onChange }: {
  readonly label: string;
  readonly value: string;
  readonly items: readonly { value: string; label: string }[];
  readonly onChange: (value: string) => void;
}): React.ReactElement {
  return <Select value={value} items={items} onValueChange={(next) => onChange(next ?? "all")}>
    <SelectTrigger size="compact" aria-label={label} className="w-full sm:w-40"><SelectValue /></SelectTrigger>
    <SelectPopup>{items.map((item) => <SelectItem key={item.value} value={item.value}>{item.label}</SelectItem>)}</SelectPopup>
  </Select>;
}

export function PlatformUsersPage(): React.ReactElement {
  usePageTitle("Users", "Platform");
  const api = useEntryApi();
  const navigate = useNavigate();
  const search = useRouterState({ select: (state) => state.location.search as UserSearch });
  const routeQuery = search.q ?? "";
  const [query, setQuery] = React.useState(routeQuery);
  React.useEffect(() => { setQuery(routeQuery); }, [routeQuery]);
  const change = React.useCallback((next: Partial<UserSearch>) => {
    void navigate({ to: "/platform/users", search: { ...search, page: undefined, ...next }, resetScroll: false } as never);
  }, [navigate, search]);
  React.useEffect(() => {
    if (query.trim() === routeQuery) return;
    const timer = setTimeout(() => change({ q: query.trim() || undefined }), 300);
    return () => clearTimeout(timer);
  }, [query, routeQuery, change]);
  const resource = usePlatformResource(() => api.platformUsers(routeQuery, search), [api, routeQuery, search.segment, search.organization, search.role, search.sort, search.direction, search.page]);
  const value = resource.value;
  const loading = resource.loading;
  const sort = search.sort ?? "last_sign_in";
  const descending = (search.direction ?? (sort === "last_sign_in" ? "desc" : "asc")) === "desc";
  const sortHead = (field: UserSort, title: string) => <TableHead aria-sort={sort === field ? descending ? "descending" : "ascending" : "none"}>
    <Button variant="ghost" size="compact" onClick={() => change({ sort: field, direction: sort === field ? descending ? "asc" : "desc" : field === "last_sign_in" ? "desc" : "asc" })}>
      {title}{sort === field ? descending ? <ArrowDown /> : <ArrowUp /> : null}
    </Button>
  </TableHead>;
  const segments = [
    { value: "all", label: "All", count: value?.counts.all },
    { value: "staff", label: "Staff", count: value?.counts.staff },
    { value: "no_organization", label: "No organization", count: value?.counts.no_organization },
    { value: "invited", label: "Invited", count: value?.counts.invited },
  ] as const;
  const filtered = Boolean(routeQuery || search.segment && search.segment !== "all" || search.organization || search.role);
  const page = value?.page ?? 1;
  const sortLabel = sort === "last_sign_in" ? `Last sign-in · ${descending ? "newest first" : "oldest first"}` : `${sort === "email" ? "User" : sort === "role" ? "Platform role" : "Organizations"} · ${sort === "organizations" ? descending ? "highest first" : "lowest first" : descending ? "Z–A" : "A–Z"}`;
  return <div className="flex min-w-0 flex-col gap-5">
    <form className="flex flex-wrap items-center gap-3" onSubmit={(event) => {
      event.preventDefault();
      if (query.trim() === routeQuery) void resource.refresh();
      else change({ q: query.trim() || undefined });
    }}>
      <Input size="compact" type="search" aria-label="Search by email" placeholder="Filter by email" value={query} onChange={(event) => setQuery(event.target.value)} className="min-w-0 sm:w-40 sm:flex-1" />
      <ToggleGroup aria-label="User segment" className="max-w-full flex-wrap" value={[search.segment ?? "all"]} onValueChange={(values) => {
        if (values[0]) change({ segment: values[0] === "all" ? undefined : values[0] as UserSearch["segment"] });
      }}>
        {segments.map((segment) => <ToggleGroupItem key={segment.value} value={segment.value}>
          {segment.label}<Badge variant={segment.value === "no_organization" && (segment.count ?? 0) > 0 ? "warning" : "outline"}>{segment.count === undefined ? "…" : formatCount(segment.count)}</Badge>
        </ToggleGroupItem>)}
      </ToggleGroup>
      <div className="flex w-full gap-2 sm:ml-auto sm:w-auto">
        <UserFilter label="Organization" value={search.organization ?? "all"} items={[{ value: "all", label: "All organizations" }, ...(value?.organizations ?? []).map((org) => ({ value: org.organization_id, label: org.organization_name }))]} onChange={(organization) => change({ organization: organization === "all" ? undefined : organization })} />
        <UserFilter label="Platform role" value={search.role ?? "all"} items={[{ value: "all", label: "All roles" }, { value: "none", label: "None" }, ...["admin", "support", "billing", "viewer"].map((role) => ({ value: role, label: role }))]} onChange={(role) => change({ role: role === "all" ? undefined : role })} />
      </div>
    </form>
    <Problem message={resource.error?.message ?? null} />
    <SettingsSection title={value ? `${formatCount(value.total)} ${value.total === 1 ? "user" : "users"}` : "Users"} className="min-w-0" headerAction={<span className="text-xs text-muted-foreground">{sortLabel}</span>}>
      {loading ? <div role="status" aria-label="Loading users"><Skeleton className="h-24 w-full" /></div> : resource.error ? <Empty><EmptyHeader><EmptyTitle>Users could not be loaded</EmptyTitle></EmptyHeader><Button variant="outline" size="compact" onClick={() => { void resource.refresh(); }}>Try again</Button></Empty> : !value?.users.length ? <Empty><EmptyHeader><EmptyTitle>{filtered ? "No users match" : "No users yet"}</EmptyTitle></EmptyHeader></Empty> : <Table aria-label="Users" className="min-w-[720px]">
        <TableHeader><TableRow>{sortHead("email", "User")}{sortHead("role", "Platform role")}{sortHead("organizations", "Organizations")}{sortHead("last_sign_in", "Last sign-in")}</TableRow></TableHeader>
        <TableBody>{value.users.map((user) => <TableRow key={user.email}>
          <TableCell><Link to={"/platform/users/$email" as never} params={{ email: user.email } as never} search={search as never} className="font-medium underline-offset-4 hover:underline focus-visible:underline">{user.email}</Link><div className="text-xs text-muted-foreground">{user.memberships.length ? user.memberships.map((org) => org.organization_name).join(", ") : user.invitations.length ? `Invited to ${user.invitations.map((org) => org.organization_name).join(", ")}` : "No organization"}</div></TableCell>
          <TableCell><PlatformRole role={user.platform_role} /></TableCell>
          <TableCell><Badge variant={user.memberships.length === 0 ? "warning" : "outline"}>{formatCount(user.memberships.length)}</Badge></TableCell>
          <TableCell><time dateTime={user.last_sign_in_at || undefined}>{user.last_sign_in_at ? date(user.last_sign_in_at, true) : "Never"}</time></TableCell>
        </TableRow>)}</TableBody>
      </Table>}
    </SettingsSection>
    {value && !resource.error ? <nav aria-label="User pages" className="flex items-center justify-between gap-3">
      <span className="text-xs text-muted-foreground">{value.total === 0 ? "0" : `${formatCount((page - 1) * value.page_size + 1)}–${formatCount(Math.min(page * value.page_size, value.total))}`} of {formatCount(value.total)}</span>
      <div className="flex gap-2"><Button variant="outline" size="compact" disabled={loading || page <= 1} onClick={() => change({ page: page - 1 })}>Previous</Button><Button variant="outline" size="compact" disabled={loading || page * value.page_size >= value.total} onClick={() => change({ page: page + 1 })}>Next</Button></div>
    </nav> : null}
    {value ? <Unsearched value={value} /> : null}
  </div>;
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
