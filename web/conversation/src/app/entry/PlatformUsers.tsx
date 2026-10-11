import React from "react";
import { Link, useRouterState } from "@tanstack/react-router";
import { usePlatformResource } from "./usePlatformResource.ts";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Empty, EmptyHeader, EmptyTitle } from "../../components/ui/empty.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { usePageTitle } from "../pageTitle.ts";
import { type PlatformUsers } from "./api.ts";
import { Problem, useEntryApi } from "./EntryScreens.tsx";

function date(value: string, time = false): string {
  if (value === "") return "—";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  const iso = parsed.toISOString();
  return time ? `${iso.slice(0, 10)} ${iso.slice(11, 16)}Z` : iso.slice(0, 10);
}

export function PlatformUserResults({ value }: { readonly value: PlatformUsers }): React.ReactElement {
  return (
    <div className="flex flex-col gap-6" aria-live="polite">
      {value.users.length === 0 ? (
        <Empty><EmptyHeader><EmptyTitle>No users match</EmptyTitle></EmptyHeader></Empty>
      ) : value.users.map((user) => (
        <section key={user.email} aria-label={user.email} className="min-w-0 space-y-3 border-b border-border pb-6">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="break-all text-sm font-medium">{user.email}</h2>
            <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
              <span>Platform role: {user.platform_role === "" ? "—" : <Badge variant="outline">{user.platform_role}</Badge>}</span>
              <span>Last sign-in: <time dateTime={user.last_sign_in_at || undefined}>{date(user.last_sign_in_at, true)}</time></span>
            </div>
          </div>
          <p className="break-all font-mono text-xs text-muted-foreground">Subject: {user.subject || "—"}</p>
          <Table aria-label={`Organizations for ${user.email}`}>
            <TableHeader><TableRow>
              <TableHead>Organization</TableHead><TableHead>Role</TableHead><TableHead>Joined</TableHead><TableHead><span className="sr-only">Tenant</span></TableHead>
            </TableRow></TableHeader>
            <TableBody>
              {user.memberships.map((membership) => (
                <TableRow key={membership.organization_id}>
                  <TableCell>{membership.organization_name}</TableCell>
                  <TableCell><Badge variant="outline">{membership.role}</Badge></TableCell>
                  <TableCell><time dateTime={membership.joined_at}>{date(membership.joined_at)}</time></TableCell>
                  <TableCell><Button size="compact" variant="ghost" render={<Link to={"/platform/tenants" as never} search={{ tenant: membership.organization_id } as never} />}>Open tenant →</Button></TableCell>
                </TableRow>
              ))}
              {user.invitations.map((invitation, index) => (
                <TableRow key={`${invitation.organization_id}-${index}`}>
                  <TableCell>{invitation.organization_name}</TableCell>
                  <TableCell><Badge variant="warning">invited</Badge></TableCell>
                  <TableCell>expires <time dateTime={invitation.expires_at}>{date(invitation.expires_at)}</time></TableCell>
                  <TableCell><Button size="compact" variant="ghost" render={<Link to={"/platform/tenants" as never} search={{ tenant: invitation.organization_id } as never} />}>Open tenant →</Button></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {user.memberships.length === 0 ? <p className="text-sm text-muted-foreground">No organization membership</p> : null}
        </section>
      ))}
      {value.users.length === 50 ? <p className="text-sm text-muted-foreground">Showing up to 50 users. Refine your search to find other users.</p> : null}
      {value.unsearched.length === 0 ? null : (
        <div role="status" className="space-y-1 text-sm text-warning-foreground">
          {value.unsearched.map((organization) => <p key={organization.organization_id}>Not searched: {organization.organization_name}</p>)}
        </div>
      )}
    </div>
  );
}

export function PlatformUsersPage(): React.ReactElement {
  usePageTitle("Users", "Platform");
  const api = useEntryApi();
  const routeQuery = useRouterState({ select: (state) => (state.location.search as { q?: string }).q ?? "" });
  const [query, setQuery] = React.useState(routeQuery);
  const [submitted, setSubmitted] = React.useState(routeQuery);
  React.useEffect(() => { setQuery(routeQuery); setSubmitted(routeQuery); }, [routeQuery]);
  const resource = usePlatformResource(() => submitted === "" ? Promise.resolve(null) : api.platformUsers(submitted), [api, submitted]);
  const loading = submitted !== "" && resource.loading;
  const valid = Array.from(query.trim()).length >= 3;
  const value = loading || resource.error !== null ? null : resource.value;
  function search(event: React.FormEvent): void {
    event.preventDefault();
    if (!valid || loading) return;
    if (submitted === query.trim()) void resource.refresh();
    else setSubmitted(query.trim());
  }

  return (
    <>
      <form onSubmit={search} className="space-y-2">
        <label htmlFor="user-email" className="text-sm font-medium">Search by email</label>
        <div className="flex items-center gap-2">
          <Input id="user-email" type="search" value={query} onChange={(event) => setQuery(event.target.value)} aria-describedby="user-search-help" className="min-w-0 flex-1" />
          <Button type="submit" disabled={!valid || loading}>{loading ? "Searching…" : "Search"}</Button>
        </div>
        <p id="user-search-help" className="text-xs text-muted-foreground">Enter at least three characters. Matches verified emails across all tenants.</p>
      </form>
      <Problem message={resource.error?.message ?? null} />
      {loading ? <div role="status" aria-label="Searching users" className="space-y-3"><Skeleton className="h-6 w-1/2" /><Skeleton className="h-24 w-full" /></div> : null}
      {value == null ? null : <PlatformUserResults value={value} />}
    </>
  );
}
