import { Link } from "@tanstack/react-router";
import { SearchIcon } from "lucide-react";
import React from "react";

import { Badge } from "../../../components/ui/badge.tsx";
import { Input } from "../../../components/ui/input.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import type { PlatformUser, PlatformUsers } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import { SettingsRow, SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage, StateLabel } from "../ui.tsx";

export function filterUsers(users: readonly PlatformUser[], query: string): readonly PlatformUser[] {
  const needle = query.trim().toLowerCase();
  if (needle === "") return users;
  return users.filter(
    (user) =>
      user.email.toLowerCase().includes(needle) ||
      user.organizations.some((membership) => membership.organization.name.toLowerCase().includes(needle)),
  );
}

function KindBadge({ kind }: { readonly kind: PlatformUser["kind"] }): React.ReactElement | null {
  if (kind === "customer") return null;
  return (
    <Badge variant={kind === "support" ? "warning" : "info"} size="sm">
      {kind}
    </Badge>
  );
}

function userPath(user: PlatformUser): string {
  return `/platform/users/${encodeURIComponent(user.subject)}`;
}

export function UsersPage(): React.ReactElement {
  const api = usePlatformApi();
  const users = useResource<PlatformUsers>(() => api.users(), [api]);
  const [query, setQuery] = React.useState("");
  return (
    <PlatformPage crumbs={[{ label: "Users" }]} proposed>
      <div className="relative">
        <SearchIcon className="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" />
        <Input
          type="search"
          aria-label="Search users"
          placeholder="Search by email or organization"
          className="ps-8"
          value={query}
          onChange={(event) => setQuery(event.currentTarget.value)}
        />
      </div>
      <SettingsSection title="People who have signed in">
        <Loadable resource={users} label="users" rows={6}>
          {(value) => {
            const rows = filterUsers(value.users, query);
            if (value.users.length === 0) return <EmptyNote>Nobody has signed in yet.</EmptyNote>;
            if (rows.length === 0) return <EmptyNote>Nobody matches “{query}”.</EmptyNote>;
            return (
              <Table aria-label="Users">
                <TableHeader>
                  <TableRow>
                    <TableHead>Email</TableHead>
                    <TableHead>Organizations</TableHead>
                    <TableHead>Last sign-in</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map((user) => (
                    <TableRow key={user.subject}>
                      <TableCell>
                        <span className="inline-flex items-center gap-1.5">
                          <Link to={userPath(user) as never} className="font-medium hover:underline">
                            {user.email}
                          </Link>
                          <KindBadge kind={user.kind} />
                        </span>
                      </TableCell>
                      <TableCell className="max-w-96 whitespace-normal">
                        {user.organizations.length === 0 ? (
                          <span className="text-muted-foreground">None</span>
                        ) : (
                          user.organizations.map((membership) => `${membership.organization.name} (${membership.role})`).join(", ")
                        )}
                      </TableCell>
                      <TableCell>
                        <Ago at={user.last_sign_in_at} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            );
          }}
        </Loadable>
      </SettingsSection>
    </PlatformPage>
  );
}

export function UserDetailPage({ subject }: { readonly subject: string }): React.ReactElement {
  const api = usePlatformApi();
  const users = useResource<PlatformUsers>(() => api.users(), [api]);
  const user = users.value?.users.find((candidate) => candidate.subject === subject);
  return (
    <PlatformPage crumbs={[{ label: "Users", to: "/platform/users" }, { label: user?.email ?? subject }]} proposed>
      <Loadable resource={users} label="the user">
        {() =>
          user === undefined ? (
            <EmptyNote>No user with subject {subject} has signed in.</EmptyNote>
          ) : (
            <>
              <div>
                <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-[-0.02em]">
                  {user.email}
                  <KindBadge kind={user.kind} />
                </h2>
                <p className="mt-1 font-mono text-xs text-muted-foreground">{user.subject}</p>
              </div>
              <SettingsSection title="Identity">
                <SettingsRow className="text-sm" title="Last sign-in" control={<Ago at={user.last_sign_in_at} />} />
                <SettingsRow className="text-sm"
                  title="Organizations created"
                  description="Counts against the per-identity limit in the allocation config."
                  control={<span className="tabular-nums">{user.created_organizations}</span>}
                />
                {user.kind === "customer" ? null : (
                  <SettingsRow className="text-sm"
                    title="Detent staff"
                    description="Staff never hold tenant membership; they reach an organization only through audited support access."
                    control={<KindBadge kind={user.kind} />}
                  />
                )}
              </SettingsSection>
              <SettingsSection title="Memberships">
                {user.organizations.length === 0 ? (
                  <EmptyNote>Not a member of any organization.</EmptyNote>
                ) : (
                  user.organizations.map((membership) => (
                    <SettingsRow className="text-sm"
                      key={membership.organization.id}
                      title={
                        <Link
                          to={`/platform/organizations/${encodeURIComponent(membership.organization.id)}/members` as never}
                          className="hover:underline"
                        >
                          {membership.organization.name}
                        </Link>
                      }
                      description={<span className="capitalize">{membership.role}</span>}
                      control={<StateLabel state={membership.status} />}
                    />
                  ))
                )}
              </SettingsSection>
            </>
          )
        }
      </Loadable>
    </PlatformPage>
  );
}
