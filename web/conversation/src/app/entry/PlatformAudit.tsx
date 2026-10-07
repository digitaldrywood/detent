import React from "react";
import { useNavigate, useRouterState } from "@tanstack/react-router";

import { Badge } from "../../components/ui/badge.tsx";
import { Button } from "../../components/ui/button.tsx";
import {
  Combobox,
  ComboboxEmpty,
  ComboboxInput,
  ComboboxItem,
  ComboboxList,
  ComboboxPopup,
} from "../../components/ui/combobox.tsx";
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from "../../components/ui/empty.tsx";
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import {
  Select,
  SelectItem,
  SelectPopup,
  SelectTrigger,
  SelectValue,
} from "../../components/ui/select.tsx";
import { Skeleton } from "../../components/ui/skeleton.tsx";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "../../components/ui/table.tsx";
import { useMutation } from "../account/useResource.ts";
import { type PlatformAudit } from "./api.ts";
import { Problem, useEntryApi } from "./EntryScreens.tsx";
import { PlatformPageTitle } from "./PlatformConsole.tsx";
import {
  redirectPlatformSignIn,
  usePlatformResource,
} from "./usePlatformResource.ts";

type AuditChoices = Pick<PlatformAudit, "events" | "tenants">;

function auditFilterQuery(params: URLSearchParams): string {
  const filters = new URLSearchParams();
  for (const name of ["tenant", "actor", "event", "from", "to"]) {
    const value = params.get(name);
    if (value) filters.set(name, value);
  }
  return filters.toString();
}

function AuditTimeline({
  query,
  onChoices,
}: {
  readonly query: string;
  readonly onChoices: (choices: AuditChoices) => void;
}): React.ReactElement {
  const api = useEntryApi();
  const resource = usePlatformResource(() => api.platformAudit(query), [api]);
  const page = resource.value;
  const active = React.useRef(true);
  React.useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
    };
  }, []);
  React.useEffect(() => {
    if (page !== undefined) onChoices(page);
  }, [page, onChoices]);
  const older = useMutation(async () => {
    if (page === undefined || page.next_cursor === "") return;
    const params = new URLSearchParams(query);
    params.set("cursor", page.next_cursor);
    const next = await api.platformAudit(params.toString());
    if (active.current)
      resource.set({ ...next, rows: [...page.rows, ...next.rows] });
  });
  React.useEffect(() => {
    redirectPlatformSignIn(older.error);
  }, [older.error]);
  return (
    <section
      aria-label="Audit timeline"
      aria-busy={resource.loading || older.pending}
      className="min-w-0 rounded-2xl border border-border bg-card"
    >
      <Problem
        message={resource.error?.message ?? older.error?.message ?? null}
      />
      {page === undefined ? (
        resource.loading ? (
          <div role="status" className="grid gap-3 p-4">
            <span className="sr-only">Loading audit events…</span>
            {[0, 1, 2].map((row) => (
              <Skeleton key={row} className="h-8 w-full" />
            ))}
          </div>
        ) : null
      ) : page.rows.length === 0 ? (
        <Empty>
          <EmptyHeader>
            <EmptyTitle>No audit events</EmptyTitle>
            <EmptyDescription>No events match these filters.</EmptyDescription>
          </EmptyHeader>
        </Empty>
      ) : (
        <>
          <Table aria-label="Audit events">
            <TableHeader>
              <TableRow>
                {["When", "Actor", "Event", "Tenant", "Detail"].map((label) => (
                  <TableHead key={label}>{label}</TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {page.rows.map((row) => (
                <TableRow key={`${row.source}:${row.id}`}>
                  <TableCell className="tabular-nums">
                    <time dateTime={row.at} title={row.at}>
                      {row.at.slice(0, 16).replace("T", " ")}Z
                    </time>
                  </TableCell>
                  <TableCell>
                    <span
                      className={
                        row.actor.includes("@") || row.actor === "system"
                          ? undefined
                          : "font-mono"
                      }
                    >
                      {row.actor}
                    </span>
                  </TableCell>
                  <TableCell>{row.event}</TableCell>
                  <TableCell>
                    {row.organization_id === "" ? (
                      <span className="text-muted-foreground">—</span>
                    ) : (
                      <div className="flex items-center gap-2">
                        <a
                          className="underline underline-offset-4"
                          href={`/platform/tenants?tenant=${encodeURIComponent(row.organization_id)}`}
                        >
                          {row.organization_name || (
                            <span className="font-mono">
                              {row.organization_id}
                            </span>
                          )}
                        </a>
                        {row.organization_deleted ? (
                          <Badge variant="outline">deleted</Badge>
                        ) : null}
                      </div>
                    )}
                  </TableCell>
                  <TableCell className="min-w-48 max-w-96 whitespace-normal break-words">
                    {row.detail}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {page.next_cursor === "" ? null : (
            <div className="flex justify-end border-t border-border p-3">
              <Button
                variant="outline"
                disabled={older.pending}
                onClick={() => {
                  void older.call();
                }}
              >
                {older.pending ? "Loading older…" : "Load older"}
              </Button>
            </div>
          )}
        </>
      )}
    </section>
  );
}

export function PlatformAuditPage(): React.ReactElement {
  const search = useRouterState({
    select: (state) => state.location.searchStr,
  });
  const navigate = useNavigate();
  const query = auditFilterQuery(new URLSearchParams(search));
  const filters = new URLSearchParams(query);
  const [choices, setChoices] = React.useState<AuditChoices>({
    events: [],
    tenants: [],
  });
  const tenants = React.useMemo(
    () => [{ id: "", name: "Any tenant", deleted: false }, ...choices.tenants],
    [choices.tenants],
  );
  const selectedTenant =
    tenants.find((tenant) => tenant.id === filters.get("tenant")) ??
    (filters.has("tenant")
      ? {
          id: filters.get("tenant")!,
          name: filters.get("tenant")!,
          deleted: false,
        }
      : tenants[0]!);
  function change(name: string, value: string) {
    const next = new URLSearchParams(query);
    if (value === "") next.delete(name);
    else next.set(name, value);
    const nextQuery = auditFilterQuery(next);
    void navigate({
      href: `/platform/audit${nextQuery === "" ? "" : `?${nextQuery}`}`,
    } as never);
  }
  return (
    <>
      <PlatformPageTitle section="audit" />
      <div className="grid min-w-0 grid-cols-1 items-end gap-3 sm:grid-cols-2 xl:grid-cols-[minmax(10rem,1fr)_minmax(10rem,1fr)_minmax(12rem,1fr)_auto_auto_auto]">
        <div className="grid min-w-0 gap-1.5">
          <Label htmlFor="audit-tenant">Tenant</Label>
          <Combobox
            key={`${selectedTenant.id}:${selectedTenant.name}`}
            items={tenants}
            value={selectedTenant}
            itemToStringLabel={(tenant) =>
              `${tenant.name}${tenant.deleted ? " (deleted)" : ""}`
            }
            isItemEqualToValue={(a, b) => a.id === b.id}
            onValueChange={(tenant) => change("tenant", tenant?.id ?? "")}
          >
            <ComboboxInput id="audit-tenant" />
            <ComboboxPopup>
              <ComboboxEmpty>No tenants found.</ComboboxEmpty>
              <ComboboxList>
                {(tenant: (typeof tenants)[number]) => (
                  <ComboboxItem key={tenant.id} value={tenant}>
                    {tenant.name}
                    {tenant.deleted ? " (deleted)" : ""}
                  </ComboboxItem>
                )}
              </ComboboxList>
            </ComboboxPopup>
          </Combobox>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor="audit-actor">Actor</Label>
          <Input
            id="audit-actor"
            placeholder="Email or subject"
            value={filters.get("actor") ?? ""}
            onChange={(event) => change("actor", event.target.value)}
          />
        </div>
        <div className="grid min-w-0 gap-1.5">
          <Label htmlFor="audit-event">Event</Label>
          <Select
            value={filters.get("event") ?? ""}
            onValueChange={(event) => change("event", event ?? "")}
          >
            <SelectTrigger id="audit-event" className="w-full">
              <SelectValue>{filters.get("event") || "Any event"}</SelectValue>
            </SelectTrigger>
            <SelectPopup>
              <SelectItem value="">Any event</SelectItem>
              {choices.events.map((event) => (
                <SelectItem key={event} value={event}>
                  {event}
                </SelectItem>
              ))}
            </SelectPopup>
          </Select>
        </div>
        <div className="grid min-w-0 gap-1.5">
          <Label htmlFor="audit-from">From</Label>
          <Input
            id="audit-from"
            type="date"
            value={filters.get("from") ?? ""}
            onChange={(event) => change("from", event.target.value)}
          />
        </div>
        <div className="grid min-w-0 gap-1.5">
          <Label htmlFor="audit-to">To</Label>
          <Input
            id="audit-to"
            type="date"
            value={filters.get("to") ?? ""}
            onChange={(event) => change("to", event.target.value)}
          />
        </div>
        <Button
          variant="outline"
          onClick={() => {
            void navigate({ to: "/platform/audit" } as never);
          }}
        >
          Clear
        </Button>
      </div>
      <AuditTimeline key={query} query={query} onChoices={setChoices} />
    </>
  );
}
