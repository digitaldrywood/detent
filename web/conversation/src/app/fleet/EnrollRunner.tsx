import { KeyRoundIcon } from "lucide-react";
import React from "react";

import { Button } from "../../components/ui/button.tsx";
import { Checkbox } from "../../components/ui/checkbox.tsx";
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
import { Input } from "../../components/ui/input.tsx";
import { Label } from "../../components/ui/label.tsx";
import type { FleetResponse, FleetRunner } from "../../contracts/account.ts";
import { ContextHelp } from "../components/ContextHelp.tsx";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useMutation } from "../account/useResource.ts";
import { SettingsRow } from "../settings/settingsLayout.tsx";

/** The operations a runner needs to take issue runs and coordinator turns. */
const ENROLLMENT_OPERATIONS = ["read", "collaborate", "claim", "heartbeat", "events"] as const;

/** The hub refuses anything over 900 (`runnerauth.MaxEnrollmentTTL`). */
const ENROLLMENT_TTL_SECONDS = 900;

export interface PendingEnrollment {
  readonly id: string;
  readonly expiresAt: string;
  readonly name: string;
}

/**
 * The organization's Hub URL as a runner reaches it. The URL is the
 * organization's own public URL, because that is the address the runner has to
 * reach, not whatever the reader happens to have in the address bar.
 */
export function runnerHubUrl(publicUrl: string, basePath: string): string {
  const origin = publicUrl.replace(/\/+$/, "");
  const path = basePath.replace(/\/+$/, "");
  return path.length > 0 && !origin.endsWith(path) ? `${origin}${path}` : origin;
}

/** Quotes a value for a POSIX shell only when it needs it. */
export function shellArgument(value: string): string {
  return /^[A-Za-z0-9._/:@%+=-]+$/.test(value) ? value : `'${value.replaceAll("'", `'\\''`)}'`;
}

/**
 * True when the URL's path carries `/organizations/ORG` for exactly this
 * organization, which is how the CLI derives the organization from it.
 */
export function hubUrlNamesOrganization(hubUrl: string, organizationId: string): boolean {
  let path: string;
  try {
    path = new URL(hubUrl).pathname;
  } catch {
    return false;
  }
  const segments = path.split("/").filter((segment) => segment !== "");
  return segments.some(
    (segment, index) => segment === "organizations" && segments[index + 1] === organizationId,
  );
}

/** The capacity field's value as the CLI's integer, or null when it is not one. */
export function parseCapacity(value: string): number | null {
  if (!/^\d+$/.test(value.trim())) return null;
  const capacity = Number(value.trim());
  return capacity >= 1 && capacity <= MAX_RUNNER_CAPACITY ? capacity : null;
}

export interface RegisterCommandInput {
  readonly hubUrl: string;
  readonly organizationId: string;
  readonly token: string;
  readonly name: string;
  readonly capacity: number;
  readonly service: boolean;
}

/**
 * The one command a host runs to become a runner. It generates the host's
 * identity locally, redeems the token, writes the runner configuration and,
 * with --service, installs the background service. The token is single-use and
 * short-lived, so carrying it in the command is as safe as the token itself.
 */
export function registerCommand(input: RegisterCommandInput): string {
  const parts = ["detent hub runner register", "--url", shellArgument(input.hubUrl)];
  if (!hubUrlNamesOrganization(input.hubUrl, input.organizationId)) {
    parts.push("--organization", shellArgument(input.organizationId));
  }
  parts.push("--token", shellArgument(input.token));
  if (input.name.trim() !== "") parts.push("--name", shellArgument(input.name.trim()));
  if (input.capacity !== 1) parts.push("--capacity", String(input.capacity));
  if (input.service) parts.push("--service");
  return parts.join(" ");
}

export const MAX_RUNNER_CAPACITY = 16;

/** The Hub limits a runner's display name to 200 bytes of UTF-8. */
export const MAX_RUNNER_NAME_BYTES = 200;

export function runnerNameFits(name: string): boolean {
  return new TextEncoder().encode(name.trim()).length <= MAX_RUNNER_NAME_BYTES;
}

/** A monospace value with the copy affordance, sized for a command line. */
export function CopyableCommand({
  value,
  displayValue = value,
  label,
}: {
  readonly value: string;
  readonly displayValue?: string;
  readonly label: string;
}): React.ReactElement {
  const [copied, setCopied] = React.useState(false);
  React.useEffect(() => {
    if (!copied) return;
    const timer = globalThis.setTimeout(() => setCopied(false), 2_000);
    return () => globalThis.clearTimeout(timer);
  }, [copied]);
  return (
    <div className="flex items-start gap-2 rounded-lg border border-border/60 bg-muted px-3 py-2">
      <code className="min-w-0 flex-1 break-all font-mono text-xs text-foreground">{displayValue}</code>
      <Button
        size="xs"
        variant="outline"
        className="shrink-0"
        aria-label={copied ? `Copied ${label}` : `Copy ${label}`}
        onClick={() => {
          void globalThis.navigator?.clipboard?.writeText(value).catch(() => undefined);
          setCopied(true);
        }}
      >
        {copied ? "Copied" : "Copy"}
      </Button>
    </div>
  );
}

export function EnrollRunnerDialog({
  open,
  onOpenChange,
  onEnrolled,
  onConnected,
  fleet,
  projectIds,
  initialName = "",
}: {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly onEnrolled: (enrollment: PendingEnrollment) => void;
  readonly onConnected?: (enrollment: PendingEnrollment) => void;
  readonly fleet: {
    readonly value: FleetResponse | undefined;
    readonly refresh: () => Promise<void>;
  };
  /** Preselects these projects instead of every readable one. */
  readonly projectIds?: readonly string[];
  readonly initialName?: string;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const organizationId = bootstrap?.organization.id ?? "";
  // The URL the runner has to reach is the organization's published one, not
  // the address bar: a reader behind a tunnel or a preview host would
  // otherwise copy an address the host cannot resolve. `location.origin` is
  // the fallback for a payload that carries no public URL. Behind the shared
  // entry the organization's Hub lives under its base path, and a runner
  // pointed at the bare origin reaches no Hub at all.
  const hubUrl = runnerHubUrl(
    bootstrap?.organizations.find((entry) => entry.current)?.public_url ??
      globalThis.location?.origin ??
      "",
    bootstrap?.base_path ?? "",
  );
  const projects = React.useMemo(() => bootstrap?.projects ?? [], [bootstrap]);
  // Every readable project by default: a host the reader is enrolling is
  // normally the host for their whole organization, and narrowing it is the
  // deliberate act, not widening it.
  const initialSelection = React.useCallback(
    () => projectIds ?? projects.map((project) => project.id),
    [projectIds, projects],
  );

  const [name, setName] = React.useState(initialName);
  React.useEffect(() => {
    if (open) setName(initialName);
  }, [open, initialName]);
  const [capacityText, setCapacityText] = React.useState("1");
  const capacity = parseCapacity(capacityText);
  // A response for a dialog the reader already closed must not come back as
  // the next opening's command.
  const generation = React.useRef(0);
  const [service, setService] = React.useState(true);
  const [selected, setSelected] = React.useState<readonly string[]>(initialSelection);
  const [enrollment, setEnrollment] = React.useState<(PendingEnrollment & {
    readonly command: string;
    readonly maskedCommand: string;
    readonly existingRunnerIds: readonly string[];
  }) | null>(null);
  const [showToken, setShowToken] = React.useState(false);
  const [connected, setConnected] = React.useState<FleetRunner | null>(null);

  // Reopening starts a fresh enrollment: the previous token was shown once and
  // leaving it on screen invites redeeming a token that has already expired.
  React.useEffect(() => {
    if (open) return;
    generation.current += 1;
    setName(initialName);
    setCapacityText("1");
    setService(true);
    setEnrollment(null);
    setShowToken(false);
    setConnected(null);
    setSelected(initialSelection());
  }, [open, initialSelection, initialName]);

  React.useEffect(() => {
    if (!open || enrollment === null || connected !== null) return;
    const runner = fleet.value?.runners.find((entry) =>
      !enrollment.existingRunnerIds.includes(entry.id) &&
      entry.display_name === enrollment.name && entry.last_heartbeat_at !== "",
    );
    if (runner === undefined) return;
    setConnected(runner);
    onConnected?.(enrollment);
  }, [open, enrollment, connected, fleet.value, onConnected]);

  React.useEffect(() => {
    if (!open || enrollment === null || connected !== null) return;
    let stopped = false;
    let timer: ReturnType<typeof globalThis.setTimeout>;
    const refresh = async () => {
      await fleet.refresh();
      if (!stopped) timer = globalThis.setTimeout(() => void refresh(), 2_000);
    };
    timer = globalThis.setTimeout(() => void refresh(), 2_000);
    return () => {
      stopped = true;
      globalThis.clearTimeout(timer);
    };
  }, [open, enrollment, connected, fleet.refresh]);

  const create = useMutation(async () => {
    const mine = generation.current;
    const existingRunnerIds = fleet.value?.runners.map((runner) => runner.id) ?? [];
    const created = await api.enrollRunner({
      projectIds: selected,
      operations: [...ENROLLMENT_OPERATIONS],
      ttlSeconds: ENROLLMENT_TTL_SECONDS,
    });
    const entry: PendingEnrollment = {
      id: created.id,
      expiresAt: created.expires_at,
      name: name.trim() || "Unnamed runner",
    };
    if (generation.current === mine) {
      const input = { hubUrl, organizationId, token: created.token, name: entry.name, capacity: capacity ?? 1, service };
      setEnrollment({
        ...entry,
        command: registerCommand(input),
        maskedCommand: registerCommand({ ...input, token: "detent_••••••••" }),
        existingRunnerIds,
      });
    }
    onEnrolled(entry);
    return created;
  });

  const nameFits = runnerNameFits(name);
  const ready = selected.length > 0 && capacity !== null && nameFits;
  const step = connected !== null ? 2 : enrollment !== null ? 1 : 0;

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogPopup className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>Enroll a runner</DialogTitle>
          <DialogDescription>
            A runner takes issue runs and coordinator turns on your own machine, with your own
            provider login. The hub never holds a provider credential.
          </DialogDescription>
        </DialogHeader>
        <DialogPanel className="flex flex-col gap-4">
          <ol aria-label="Runner enrollment steps" className="flex gap-3 text-xs sm:gap-6">
            {["Set it up", "Run the command", "Connected"].map((label, index) => (
              <li
                key={label}
                aria-current={step === index ? "step" : undefined}
                className={step === index ? "font-medium text-foreground" : "text-muted-foreground"}
              >
                <span aria-hidden="true">{index + 1}. </span>{label}
              </li>
            ))}
          </ol>
          {connected !== null ? (
            <section className="flex flex-col gap-3" aria-labelledby="enroll-connected">
              <h3 id="enroll-connected" role="status" className="text-sm font-medium">
                {connected.display_name} is connected
              </h3>
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-[13px]">
                <dt className="text-muted-foreground">Hostname</dt><dd className="break-all">{connected.hostname}</dd>
                <dt className="text-muted-foreground">OS / architecture</dt><dd>{connected.os} / {connected.architecture}</dd>
                <dt className="text-muted-foreground">Slots</dt><dd>{connected.reported_capacity}</dd>
                <dt className="text-muted-foreground">Providers</dt>
                <dd>{[...new Set(connected.provider_capacity.map((provider) => provider.provider))].join(", ") || "None reported"}</dd>
              </dl>
            </section>
          ) : enrollment === null ? (
            <>
              <div className="flex flex-col gap-2 sm:flex-row">
                <div className="flex flex-1 flex-col gap-1.5">
                  <Label htmlFor="enroll-runner-name" className="sm:min-h-7">Name</Label>
                  <Input
                    id="enroll-runner-name"
                    autoComplete="off"
                    spellCheck={false}
                    aria-invalid={!nameFits}
                    placeholder="Build host"
                    value={name}
                    onChange={(event) => setName(event.currentTarget.value)}
                  />
                  {nameFits ? null : (
                    <p className="text-xs text-destructive-foreground">That name is too long; shorten it.</p>
                  )}
                </div>
                <div className="flex flex-col gap-1.5 sm:w-32">
                  <div className="flex items-center gap-1">
                    <Label htmlFor="enroll-runner-capacity">Concurrency</Label>
                    <ContextHelp label="Concurrency">
                      Concurrent work items: one runner identity and process can execute up to this many independent work items concurrently, each with its own workspace and agent. 6 means up to six jobs on this runner; it does not enroll six runners. Start with 1, then increase as CPU, memory, and provider capacity allow. Shared host, project, provider, and other dispatch limits can lower effective concurrency.
                    </ContextHelp>
                  </div>
                  <Input
                    id="enroll-runner-capacity"
                    type="number"
                    inputMode="numeric"
                    min={1}
                    max={MAX_RUNNER_CAPACITY}
                    step={1}
                    aria-invalid={capacity === null}
                    aria-describedby="enroll-runner-capacity-hint"
                    value={capacityText}
                    onChange={(event) => setCapacityText(event.currentTarget.value)}
                  />
                  <p
                    id="enroll-runner-capacity-hint"
                    className={
                      capacity === null
                        ? "text-xs text-destructive-foreground"
                        : "text-xs text-muted-foreground"
                    }
                  >
                    {capacity === null ? `A whole number from 1 to ${MAX_RUNNER_CAPACITY}` : `1 to ${MAX_RUNNER_CAPACITY}`}
                  </p>
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                Default: 1 independent work item at a time. Higher capacity lets this host run that many independent work items concurrently, each with its own workspace and agent, subject to host resources and provider limits.
              </p>
              <fieldset className="flex flex-col gap-2">
                <legend className="pb-1 text-[13px] font-medium">
                  <span className="flex items-center gap-1">
                    Projects
                    <ContextHelp label="Projects">
                      Selected projects define this runner’s access and routing scope. One runner
                      can serve several projects; selecting three projects does not create three
                      runners. Work still needs matching policy, a fresh heartbeat, and available capacity.
                    </ContextHelp>
                  </span>
                </legend>
                {projects.length === 0 ? (
                  <p className="text-[13px] text-muted-foreground">
                    You can read no projects on this organization, so there is nothing to enroll a
                    runner for.
                  </p>
                ) : (
                  projects.map((project) => (
                    <label
                      key={project.id}
                      className="flex items-center gap-2 text-[13px]"
                      htmlFor={`enroll-project-${project.id}`}
                    >
                      <Checkbox
                        id={`enroll-project-${project.id}`}
                        checked={selected.includes(project.id)}
                        onCheckedChange={(checked) =>
                          setSelected((current) =>
                            checked === true
                              ? current.includes(project.id)
                                ? current
                                : [...current, project.id]
                              : current.filter((id) => id !== project.id),
                          )
                        }
                      />
                      <span className="truncate">{project.name}</span>
                    </label>
                  ))
                )}
              </fieldset>
              <label className="flex items-start gap-2 text-[13px]" htmlFor="enroll-runner-service">
                <Checkbox
                  id="enroll-runner-service"
                  checked={service}
                  onCheckedChange={(checked) => setService(checked === true)}
                />
                <span>
                  Install it as a background service
                  <span className="block text-muted-foreground">
                    Starts at login and keeps running, separate from any local Detent board on the
                    same machine. It starts once each project's repository is cloned there; until
                    then the command prints what to clone and how to start it.
                  </span>
                </span>
              </label>
            </>
          ) : (
            <section className="flex flex-col gap-2" aria-labelledby="enroll-run-command">
              <h3 id="enroll-run-command" className="text-[13px] font-medium">
                Run this on the machine that will take the work.
              </h3>
              <CopyableCommand
                value={enrollment.command}
                displayValue={showToken ? enrollment.command : enrollment.maskedCommand}
                label="the register command"
              />
              <Button
                size="xs"
                variant="outline"
                className="self-start"
                aria-pressed={showToken}
                onClick={() => setShowToken((current) => !current)}
              >
                {showToken ? "Hide token" : "Show token"}
              </Button>
              <p role="status" className="text-[13px] text-muted-foreground">
                Waiting for {enrollment.name || "the runner"} to check in. The command works once, until{" "}
                {new Date(enrollment.expiresAt).toLocaleTimeString()}.
              </p>
              <p className="text-xs text-muted-foreground">
                Closing this dialog clears the command from the page. The runner still connects if you already ran it.
              </p>
            </section>
          )}
          <ControlError message={create.error?.message ?? null} />
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline">{connected !== null ? "Done" : enrollment === null ? "Cancel" : "Close"}</Button>} />
          {enrollment === null ? (
            <Button disabled={!ready || create.pending} onClick={() => void create.call()}>
              {create.pending ? "Creating…" : "Create command"}
            </Button>
          ) : null}
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

/** The rows under the Runners section: what this screen has handed out. */
export function PendingEnrollments({
  enrollments,
  onRenew,
}: {
  readonly enrollments: readonly PendingEnrollment[];
  readonly onRenew?: (name: string) => void;
}): React.ReactElement | null {
  if (enrollments.length === 0) return null;
  return (
    <>
      {enrollments.map((entry) => (
        <SettingsRow
          key={entry.id}
          title={
            <span className="flex min-w-0 items-center gap-2">
              <KeyRoundIcon aria-hidden="true" className="size-3.5 shrink-0" />
              <span className="truncate">{entry.name === "" ? "Unnamed runner" : entry.name}</span>
            </span>
          }
          description={`No check-in yet · expires ${new Date(entry.expiresAt).toLocaleTimeString()}`}
          control={onRenew === undefined ? null : <Button size="xs" variant="outline" onClick={() => onRenew(entry.name)}>Make a new command</Button>}
        />
      ))}
    </>
  );
}
