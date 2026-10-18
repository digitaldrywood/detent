import React from "react";
import { X } from "lucide-react";
import { Link, useNavigate, useRouterState } from "@tanstack/react-router";

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
import {
  Popover,
  PopoverTrigger,
  PopoverPopup,
} from "../../components/ui/popover.tsx";
import { Spinner } from "../../components/ui/spinner.tsx";
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
import {
  auditAction,
  auditActor,
  auditCSV,
  auditDetails,
  auditTenant,
  auditWhen,
} from "./platformAuditPresentation.ts";
import { PlatformPageActions } from "./PlatformPageActions.tsx";
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
  actorPending,
}: {
  readonly query: string;
  readonly actorPending: boolean;
  readonly onChoices: (choices: AuditChoices) => void;
}): React.ReactElement {
  const api = useEntryApi();
  const resource = usePlatformResource(() => api.platformAudit(query), [api]);
  const page = resource.value;
  const active = React.useRef(true);
  const exportController = React.useRef<AbortController | null>(null);
  React.useEffect(() => {
    active.current = true;
    return () => {
      active.current = false;
      exportController.current?.abort();
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
  const exporting = useMutation(async () => {
    const controller = new AbortController();
    exportController.current = controller;
    const chunks: Blob[] = [];
    let rowCount = 0;
    let cursor = "";
    try {
      do {
        const params = new URLSearchParams(query);
        params.set("limit", "200");
        if (cursor !== "") params.set("cursor", cursor);
        const next = await api.platformAudit(params.toString(), controller.signal);
        if (controller.signal.aborted || !active.current) return;
        rowCount += next.rows.length;
        if (rowCount > 10_000 || (rowCount === 10_000 && next.next_cursor !== "")) {
          throw new Error("Export exceeds 10,000 events. Narrow the date range or other filters and try again.");
        }
        chunks.push(new Blob([auditCSV(next.rows, next.tenants, chunks.length === 0)]));
        cursor = next.next_cursor;
      } while (cursor !== "");
      const url = URL.createObjectURL(
        new Blob(chunks, { type: "text/csv;charset=utf-8" }),
      );
      try {
        const link = document.createElement("a");
        link.href = url;
        link.download = "audit-log.csv";
        link.click();
      } finally {
        URL.revokeObjectURL(url);
      }
    } catch (error) {
      if (!controller.signal.aborted) throw error;
    } finally {
      exportController.current = null;
    }
  });
  React.useEffect(() => {
    redirectPlatformSignIn(exporting.error);
  }, [exporting.error]);
  return (
    <>
      <PlatformPageActions>
        <Button
          variant="outline"
          size="sm"
          disabled={!exporting.pending && (page === undefined || actorPending)}
          aria-busy={exporting.pending}
          onClick={() => {
            if (exporting.pending) exportController.current?.abort();
            else void exporting.call();
          }}
        >
          {exporting.pending ? <Spinner aria-hidden="true" /> : null}
          {exporting.pending ? "Cancel export" : "Export CSV"}
        </Button>
      </PlatformPageActions>
      <section
        aria-label="Audit timeline"
        aria-busy={resource.loading || older.pending}
        className="grid min-w-0 gap-3"
      >
        <div className="flex items-center justify-between gap-2">
          <h2 className="text-sm font-medium">Events</h2>
          {page === undefined ? null : (
            <Badge
              variant="outline"
              aria-label={`${page.rows.length} events loaded`}
            >
              {page.rows.length}
            </Badge>
          )}
        </div>
        <Problem
          message={
            resource.error?.message ??
            older.error?.message ??
            exporting.error?.message ??
            null
          }
        />
        <div className="min-w-0 overflow-hidden rounded-xl border border-border">
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
                <EmptyDescription>
                  No events match these filters.
                </EmptyDescription>
              </EmptyHeader>
            </Empty>
          ) : (
            <Table aria-label="Audit events">
              <TableHeader>
                <TableRow>
                  {["When", "Action", "Tenant", "By", "Details"].map(
                    (label) => (
                      <TableHead key={label}>{label}</TableHead>
                    ),
                  )}
                </TableRow>
              </TableHeader>
              <TableBody>
                {page.rows.map((row) => {
                  const action = auditAction(row.event);
                  const detail = auditDetails(row);
                  return (
                    <TableRow key={`${row.source}:${row.id}`}>
                      <TableCell className="tabular-nums text-muted-foreground">
                        <time dateTime={row.at}>{auditWhen(row.at)}</time>
                      </TableCell>
                      <TableCell>
                        <Badge variant={action.variant}>{action.label}</Badge>
                      </TableCell>
                      <TableCell className="min-w-32 max-w-48 whitespace-normal break-words">
                        {row.organization_id === "" ? (
                          <span className="text-muted-foreground">—</span>
                        ) : (
                          <div className="flex flex-wrap items-center gap-2">
                            <Link
                              className="text-info-foreground underline underline-offset-4"
                              to={"/platform/tenants/$organization" as never}
                              params={{ organization: row.organization_id } as never}
                            >
                              {auditTenant(row, page.tenants)}
                            </Link>
                            {row.organization_deleted ? (
                              <Badge variant="outline">Deleted</Badge>
                            ) : null}
                          </div>
                        )}
                      </TableCell>
                      <TableCell className="min-w-32 max-w-48 whitespace-normal break-words">
                        {auditActor(row.actor)}
                      </TableCell>
                      <TableCell className="min-w-48 max-w-96 whitespace-normal break-words">
                        <p>{detail.summary}</p>
                        {detail.reason === "" ? null : (
                          <p className="text-muted-foreground">
                            {detail.reason}
                          </p>
                        )}
                      </TableCell>
                    </TableRow>
                  );
                })}
              </TableBody>
            </Table>
          )}
        </div>
        {!page?.next_cursor ? null : (
          <div className="flex justify-center">
            <Button
              variant="outline"
              size="sm"
              disabled={older.pending}
              aria-busy={older.pending}
              onClick={() => {
                void older.call();
              }}
            >
              {older.pending ? <Spinner aria-hidden="true" /> : null}Load older
              events
            </Button>
          </div>
        )}
      </section>
    </>
  );
}

function ActorFilter({
  value,
  onChange,
  draft,
  setDraft,
}: {
  readonly value: string;
  readonly onChange: (value: string) => void;
  readonly draft: string;
  readonly setDraft: (value: string) => void;
}): React.ReactElement {
  React.useEffect(() => {
    if (draft === value) return;
    const timeout = window.setTimeout(() => onChange(draft), 350);
    return () => window.clearTimeout(timeout);
  }, [draft, value, onChange]);
  return (
    <Popover>
      <PopoverTrigger render={<Button variant="outline" size="sm" />}>
        <span className="max-w-56 truncate">Actor: {draft || "Anyone"}</span>
      </PopoverTrigger>
      <PopoverPopup width="sm" align="start">
        <div className="grid gap-1.5">
          <Label htmlFor="audit-actor">Actor</Label>
          <Input
            id="audit-actor"
            placeholder="Email or subject"
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
          />
        </div>
      </PopoverPopup>
    </Popover>
  );
}

function dateRangeLabel(from: string, to: string): string {
  if (!from && !to) return "All dates";
  const today = new Date().toISOString().slice(0, 10);
  for (const days of [7, 30, 90]) {
    if (from === daysAgo(days) && to === today) return `Last ${days} days`;
  }
  return `${from || "Any date"} – ${to || "Today"}`;
}

function daysAgo(days: number): string {
  const date = new Date();
  date.setUTCDate(date.getUTCDate() - days + 1);
  return date.toISOString().slice(0, 10);
}

export function PlatformAuditPage(): React.ReactElement {
  const search = useRouterState({
    select: (state) => state.location.searchStr,
  });
  const navigate = useNavigate();
  const query = auditFilterQuery(new URLSearchParams(search));
  const filters = new URLSearchParams(query);
  const actor = filters.get("actor") ?? "";
  const [actorDraft, setActorDraft] = React.useState(actor);
  React.useEffect(() => {
    setActorDraft(actor);
  }, [actor]);
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
      ? { id: filters.get("tenant")!, name: "Selected tenant", deleted: false }
      : tenants[0]!);
  const changeFilters = React.useCallback(
    (values: Record<string, string>) => {
      const next = new URLSearchParams(query);
      for (const [name, value] of Object.entries(values)) {
        if (value === "") next.delete(name);
        else next.set(name, value);
      }
      const nextQuery = auditFilterQuery(next);
      void navigate({
        href: `/platform/audit${nextQuery === "" ? "" : `?${nextQuery}`}`,
      } as never);
    },
    [query, navigate],
  );
  const changeActor = React.useCallback(
    (value: string) => changeFilters({ actor: value }),
    [changeFilters],
  );
  const from = filters.get("from") ?? "";
  const to = filters.get("to") ?? "";
  return (
    <>
      <PlatformPageTitle section="audit" />
      <div
        role="group"
        aria-label="Audit filters"
        className="flex min-w-0 flex-wrap items-center gap-2"
      >
        <div className="flex min-w-0 max-w-full items-center gap-1">
          <Popover>
            <PopoverTrigger render={<Button variant="outline" size="sm" />}>
              <span className="max-w-56 truncate">
                Tenant: {selectedTenant.id ? selectedTenant.name : "All"}
              </span>
            </PopoverTrigger>
            <PopoverPopup width="md" align="start">
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
                  onValueChange={(tenant) =>
                    changeFilters({ tenant: tenant?.id ?? "" })
                  }
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
            </PopoverPopup>
          </Popover>
          {selectedTenant.id ? (
            <Button
              variant="ghost"
              size="icon-sm"
              aria-label="Remove tenant filter"
              onClick={() => changeFilters({ tenant: "" })}
            >
              <X aria-hidden="true" />
            </Button>
          ) : null}
        </div>
        <ActorFilter
          value={actor}
          onChange={changeActor}
          draft={actorDraft}
          setDraft={setActorDraft}
        />
        <Popover>
          <PopoverTrigger render={<Button variant="outline" size="sm" />}>
            <span className="max-w-56 truncate">
              Action:{" "}
              {filters.get("event")
                ? auditAction(filters.get("event")!).label
                : "All"}
            </span>
          </PopoverTrigger>
          <PopoverPopup width="md" align="start">
            <div className="grid gap-1.5">
              <Label htmlFor="audit-event">Action</Label>
              <Select
                value={filters.get("event") ?? ""}
                onValueChange={(event) => changeFilters({ event: event ?? "" })}
              >
                <SelectTrigger id="audit-event">
                  <SelectValue>
                    {filters.get("event")
                      ? auditAction(filters.get("event")!).label
                      : "All actions"}
                  </SelectValue>
                </SelectTrigger>
                <SelectPopup>
                  <SelectItem value="">All actions</SelectItem>
                  {choices.events.map((event) => (
                    <SelectItem key={event} value={event}>
                      {auditAction(event).label}
                    </SelectItem>
                  ))}
                </SelectPopup>
              </Select>
            </div>
          </PopoverPopup>
        </Popover>
        <Popover>
          <PopoverTrigger render={<Button variant="outline" size="sm" />}>
            {dateRangeLabel(from, to)}
          </PopoverTrigger>
          <PopoverPopup width="sm" align="start">
            <div className="grid gap-3">
              <div className="flex flex-wrap gap-2">
                {[7, 30, 90].map((days) => (
                  <Button
                    key={days}
                    variant="outline"
                    size="sm"
                    onClick={() =>
                      changeFilters({
                        from: daysAgo(days),
                        to: new Date().toISOString().slice(0, 10),
                      })
                    }
                  >
                    Last {days} days
                  </Button>
                ))}
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() => changeFilters({ from: "", to: "" })}
                >
                  All dates
                </Button>
              </div>
              <p className="text-sm font-medium">Custom range (UTC)</p>
              <div className="grid gap-1.5">
                <Label htmlFor="audit-from">From</Label>
                <Input
                  id="audit-from"
                  type="date"
                  value={from}
                  onChange={(event) =>
                    changeFilters({ from: event.target.value })
                  }
                />
              </div>
              <div className="grid gap-1.5">
                <Label htmlFor="audit-to">To</Label>
                <Input
                  id="audit-to"
                  type="date"
                  value={to}
                  onChange={(event) =>
                    changeFilters({ to: event.target.value })
                  }
                />
              </div>
            </div>
          </PopoverPopup>
        </Popover>
        <Button
          variant="ghost"
          size="sm"
          onClick={() => {
            setActorDraft("");
            void navigate({ to: "/platform/audit", search: {} } as never);
          }}
        >
          Clear filters
        </Button>
      </div>
      <AuditTimeline
        key={query}
        query={query}
        onChoices={setChoices}
        actorPending={actorDraft !== actor}
      />
    </>
  );
}
