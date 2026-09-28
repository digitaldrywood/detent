import { Link } from "@tanstack/react-router";
import React from "react";

import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../../components/ui/table.tsx";
import type { SupportSession, SupportSessions } from "../../../contracts/platform.ts";
import { useResource } from "../../account/useResource.ts";
import { SettingsRow, SettingsSection } from "../../settings/settingsLayout.tsx";
import { usePlatformAccess } from "../access.tsx";
import { usePlatformApi } from "../PlatformLayout.tsx";
import { Ago, EmptyNote, Loadable, PlatformPage, ReasonDialog, StateLabel } from "../ui.tsx";

function SessionsTable({ sessions }: { readonly sessions: readonly SupportSession[] }): React.ReactElement {
  return (
    <Table aria-label="Past support sessions">
      <TableHeader>
        <TableRow>
          <TableHead>Organization</TableHead>
          <TableHead>Support actor</TableHead>
          <TableHead>Signed in as</TableHead>
          <TableHead>Reason</TableHead>
          <TableHead>Started</TableHead>
          <TableHead>Outcome</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {sessions.map((session) => (
          <TableRow key={session.id}>
            <TableCell>
              <Link
                to={`/platform/organizations/${encodeURIComponent(session.organization.id)}/audit` as never}
                className="font-medium hover:underline"
              >
                {session.organization.name}
              </Link>
            </TableCell>
            <TableCell>{session.actor}</TableCell>
            <TableCell>{session.effective_email}</TableCell>
            <TableCell>{session.reason.replaceAll("-", " ")}</TableCell>
            <TableCell>
              <Ago at={session.started_at} />
            </TableCell>
            <TableCell>
              <StateLabel state={session.state === "ended" ? "idle" : "stale"} label={session.state} />
            </TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

export function SupportSessionsPage(): React.ReactElement {
  const api = usePlatformApi();
  const access = usePlatformAccess();
  const sessions = useResource<SupportSessions>(() => api.supportSessions(), [api]);
  return (
    <PlatformPage crumbs={[{ label: "Support sessions" }]} proposed>
      <p className="max-w-2xl text-[13px] text-muted-foreground">
        Support access signs a support actor in as a member for at most one hour, with a stated reason. Start it from an
        organization's page. {access.canSupport ? "" : "Your account is not a support actor, so you can read sessions but not start or end them."}
      </p>
      <Loadable resource={sessions} label="support sessions">
        {(value) => {
          const active = value.sessions.filter((session) => session.state === "active" || session.state === "pending");
          const past = value.sessions.filter((session) => session.state !== "active" && session.state !== "pending");
          return (
            <>
              <SettingsSection title={`Active now (${active.length})`}>
                {active.length === 0 ? (
                  <EmptyNote>No one is in a customer organization right now.</EmptyNote>
                ) : (
                  active.map((session) => (
                    <SettingsRow className="text-sm"
                      key={session.id}
                      title={
                        <span>
                          {session.actor} in{" "}
                          <Link
                            to={`/platform/organizations/${encodeURIComponent(session.organization.id)}` as never}
                            className="hover:underline"
                          >
                            {session.organization.name}
                          </Link>
                        </span>
                      }
                      description={`As ${session.effective_email} for ${session.reason.replaceAll("-", " ")}.`}
                      status={
                        <span>
                          <StateLabel state="active" label={session.state === "pending" ? "Signing in" : "Active"} />, ends{" "}
                          <Ago at={session.expires_at} />
                        </span>
                      }
                      control={
                        <ReasonDialog
                          trigger="End session"
                          title={`End ${session.actor}'s session in ${session.organization.name}?`}
                          description="The session is revoked at once. The support actor has to start a new one, with a new reason, to return."
                          confirm="End session"
                          destructive
                          disabled={!access.canSupport}
                          disabledReason="Only support actors can end support sessions."
                          onConfirm={async (reason) => (await api.endSupportSession({ session: session.id, csrf: access.csrf, reason })).message}
                        />
                      }
                    />
                  ))
                )}
              </SettingsSection>
              <SettingsSection title="History">
                {past.length === 0 ? <EmptyNote>No support session has run yet.</EmptyNote> : <SessionsTable sessions={past} />}
              </SettingsSection>
            </>
          );
        }}
      </Loadable>
    </PlatformPage>
  );
}
