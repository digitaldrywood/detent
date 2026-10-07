import React from "react";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import { Empty, EmptyHeader, EmptyTitle } from "../../components/ui/empty.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../../components/ui/table.tsx";
import { AccountError } from "../account/api.ts";
import { usePageTitle } from "../pageTitle.ts";
import { type PlatformAccounts } from "./api.ts";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import { redirectPlatformSignIn } from "./usePlatformResource.ts";

function date(value: string, time = false): string {
  if (value === "") return "—";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return value;
  const iso = parsed.toISOString();
  return time ? `${iso.slice(0, 10)} ${iso.slice(11, 16)}Z` : iso.slice(0, 10);
}

export function PlatformAccountResults({ value }: { readonly value: PlatformAccounts }): React.ReactElement {
  return (
    <div className="flex flex-col gap-6" aria-live="polite">
      {value.accounts.length === 0 ? (
        <Empty><EmptyHeader><EmptyTitle>No accounts match</EmptyTitle></EmptyHeader></Empty>
      ) : value.accounts.map((account) => (
        <section key={account.email} aria-label={account.email} className="min-w-0 space-y-3 border-b border-border pb-6">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 className="break-all text-sm font-medium">{account.email}</h2>
            <div className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
              <span>Platform role: {account.platform_role === "" ? "—" : <Badge variant="outline">{account.platform_role}</Badge>}</span>
              <span>Last sign-in: <time dateTime={account.last_sign_in_at || undefined}>{date(account.last_sign_in_at, true)}</time></span>
            </div>
          </div>
          <p className="break-all font-mono text-xs text-muted-foreground">Subject: {account.subject || "—"}</p>
          <Table aria-label={`Organizations for ${account.email}`}>
            <TableHeader><TableRow>
              <TableHead>Organization</TableHead><TableHead>Role</TableHead><TableHead>Joined</TableHead><TableHead><span className="sr-only">Tenant</span></TableHead>
            </TableRow></TableHeader>
            <TableBody>
              {account.memberships.map((membership) => (
                <TableRow key={membership.organization_id}>
                  <TableCell>{membership.organization_name}</TableCell>
                  <TableCell><Badge variant="outline">{membership.role}</Badge></TableCell>
                  <TableCell><time dateTime={membership.joined_at}>{date(membership.joined_at)}</time></TableCell>
                  <TableCell><Button size="compact" variant="ghost" render={<a href={`/platform/tenants?tenant=${encodeURIComponent(membership.organization_id)}`} />}>Open tenant →</Button></TableCell>
                </TableRow>
              ))}
              {account.invitations.map((invitation, index) => (
                <TableRow key={`${invitation.organization_id}-${index}`}>
                  <TableCell>{invitation.organization_name}</TableCell>
                  <TableCell><Badge variant="warning">invited</Badge></TableCell>
                  <TableCell>expires <time dateTime={invitation.expires_at}>{date(invitation.expires_at)}</time></TableCell>
                  <TableCell><Button size="compact" variant="ghost" render={<a href={`/platform/tenants?tenant=${encodeURIComponent(invitation.organization_id)}`} />}>Open tenant →</Button></TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {account.memberships.length === 0 ? <p className="text-sm text-muted-foreground">No organization membership</p> : null}
        </section>
      ))}
      {value.accounts.length === 50 ? <p className="text-sm text-muted-foreground">Showing up to 50 accounts. Refine your search to find other accounts.</p> : null}
      {value.unsearched.length === 0 ? null : (
        <div role="status" className="space-y-1 text-sm text-warning-foreground">
          {value.unsearched.map((organization) => <p key={organization.organization_id}>Not searched: {organization.organization_name}</p>)}
        </div>
      )}
    </div>
  );
}

export function PlatformAccountsPage(): React.ReactElement {
  usePageTitle("Accounts", "Platform");
  const api = useEntryApi();
  const [query, setQuery] = React.useState("");
  const [value, setValue] = React.useState<PlatformAccounts>();
  const [loading, setLoading] = React.useState(false);
  const [error, setError] = React.useState<string | null>(null);
  const active = React.useRef(true);
  React.useEffect(() => {
    active.current = true;
    return () => { active.current = false; };
  }, []);
  const valid = Array.from(query.trim()).length >= 3;

  async function search(event: React.FormEvent): Promise<void> {
    event.preventDefault();
    if (!valid || loading) return;
    setLoading(true);
    setValue(undefined);
    setError(null);
    try {
      const result = await api.platformAccounts(query.trim());
      if (active.current) setValue(result);
    } catch (cause) {
      if (active.current) {
        redirectPlatformSignIn(cause);
        setError(cause instanceof AccountError ? cause.message : "Account search is temporarily unavailable");
      }
    } finally {
      if (active.current) setLoading(false);
    }
  }

  return (
    <>
      <h1 className="text-2xl font-semibold">Accounts</h1>
      <form onSubmit={(event) => { void search(event); }} className="space-y-2">
        <label htmlFor="account-email" className="text-sm font-medium">Search by email</label>
        <div className="flex items-center gap-2">
          <Input id="account-email" type="search" value={query} onChange={(event) => setQuery(event.target.value)} aria-describedby="account-search-help" className="min-w-0 flex-1" />
          <Button type="submit" disabled={!valid || loading}>{loading ? "Searching…" : "Search"}</Button>
        </div>
        <p id="account-search-help" className="text-xs text-muted-foreground">Enter at least three characters. Matches verified emails across all tenants.</p>
      </form>
      <Problem message={error} />
      {loading ? <div role="status" aria-label="Searching accounts" className="space-y-3"><Skeleton className="h-6 w-1/2" /><Skeleton className="h-24 w-full" /></div> : null}
      {value === undefined ? null : <PlatformAccountResults value={value} />}
    </>
  );
}
