import { projectSettingsPath } from "../settings/projectSettings.ts";
import { KeyRoundIcon } from "lucide-react";
import React from "react";

import { Alert, AlertDescription, AlertTitle } from "../../components/ui/alert.tsx";
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
import { Radio, RadioGroup } from "../../components/ui/radio-group.tsx";
import type { FleetResponse, FleetRunner } from "../../contracts/account.ts";
import { ContextHelp } from "../components/ContextHelp.tsx";
import { NativeSelect } from "../account/controls.tsx";
import { ControlError } from "../account/controls.tsx";
import { AccountError } from "../account/api.ts";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useMutation, useResource, type Resource } from "../account/useResource.ts";
import { SettingsRow } from "../settings/settingsLayout.tsx";
import { runnerAccessOptions, spriteSandboxUnavailableReason, type RunnerAccessTier } from "./runnerAccess.ts";

/** The operations a runner needs to take issue runs and coordinator turns. */
const ENROLLMENT_OPERATIONS = ["read", "collaborate", "claim", "heartbeat", "events"] as const;

/** The hub refuses anything over 900 (`runnerauth.MaxEnrollmentTTL`). */
const ENROLLMENT_TTL_SECONDS = 900;

function enrollmentError(error: unknown): string | null {
  if (error instanceof AccountError) return `${error.code}: ${error.message}`;
  return error instanceof Error ? error.message : null;
}

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
  readonly isolationTier: RunnerAccessTier;
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
  parts.push("--isolation-tier", input.isolationTier);
  if (input.service) parts.push("--service");
  return parts.join(" ");
}

export const MAX_RUNNER_CAPACITY = 16;

export const INSTALL_DETENT_COMMAND = "curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/main/install.sh | sh";
export const BREW_INSTALL_DETENT_COMMAND = "brew install digitaldrywood/tap/detent";

export function runnerInstallPlatform(): "macos" | "linux" {
  return /linux/i.test(`${globalThis.navigator?.platform ?? ""} ${globalThis.navigator?.userAgent ?? ""}`) ? "linux" : "macos";
}

export function installAndRegisterCommand(command: string): string {
  return `${INSTALL_DETENT_COMMAND} && export PATH="$HOME/.local/bin:/usr/local/bin:$PATH" && ${command}`;
}

export function spriteBootstrapPin(version: string): { ref: string; release: string | null } {
  if (/^v?\d+\.\d+\.\d+$/.test(version)) {
    const release = `v${version.replace(/^v/, "")}`;
    return { ref: release, release };
  }
  const commit = /^(?:operator-landed-)?([a-f0-9]{7,40})$/.exec(version)?.[1];
  return { ref: commit ?? "latest", release: null };
}

const LATEST_RELEASE_COMMAND = 'detent_release_url=$(curl -fsSL -o /dev/null -w "%{url_effective}" https://github.com/digitaldrywood/detent/releases/latest) && detent_release=${detent_release_url##*/}';

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
  readonly fleet: Pick<Resource<FleetResponse>, "value" | "error" | "loading" | "refresh">;
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
  const [location, setLocation] = React.useState<"machine" | "sprite">("machine");
  const [platform, setPlatform] = React.useState(runnerInstallPlatform);
  const [capacityText, setCapacityText] = React.useState("1");
  const [machineIsolationTier, setMachineIsolationTier] = React.useState<RunnerAccessTier | null>(null);
  const isolationTier = location === "sprite" ? "native-trusted" : machineIsolationTier;
  const capacity = parseCapacity(capacityText);
  // A response for a dialog the reader already closed must not come back as
  // the next opening's command.
  const generation = React.useRef(0);
  const [service, setService] = React.useState(true);
  const [scope, setScope] = React.useState<"organization" | "projects">(projectIds === undefined ? "organization" : "projects");
  const [selected, setSelected] = React.useState<readonly string[]>(initialSelection);
  const [enrollment, setEnrollment] = React.useState<(PendingEnrollment & {
    readonly command: string;
    readonly maskedCommand: string;
    readonly existingRunnerIds: readonly string[];
  }) | null>(null);
  const selectedKey = selected.join(",");
  const sprites = useResource(async () => {
    if (!open || location !== "sprite") return [];
    return Promise.all((scope === "organization" ? [""] : selected).map(async (id) => {
      try {
        const projectSecret = await api.spritesSecret(id);
        const secret = !projectSecret.present && id !== "" ? await api.spritesSecret("") : projectSecret;
        return { id, present: secret.present, organization: secret.organization_slug, error: null };
      } catch (error) {
        return { id, present: null, organization: undefined, error: enrollmentError(error) };
      }
    }));
  }, [api, open, location, selectedKey, scope]);
  const canWake = sprites.value?.length === (scope === "organization" ? 1 : selected.length) && sprites.value.every((entry) => entry.present === true);
  const spriteScope = scope === "organization" ? [""] : selected;
  const currentSprites = !sprites.loading && sprites.error === null && sprites.value?.length === spriteScope.length && sprites.value.every((entry, index) => entry.id === spriteScope[index]) ? sprites.value : [];
  const spriteOrganizations = [...new Set(currentSprites.filter((entry) => entry.present).map((entry) => entry.organization))];
  const spriteOrganization = spriteOrganizations.length === 1 ? spriteOrganizations[0] : undefined;
  const spriteOrganizationArgument = spriteOrganization ? shellArgument(spriteOrganization) : '"${SPRITE_ORG:?Set SPRITE_ORG to your Fly organization}"';
  const publishedVersion = fleet.value?.current ?? bootstrap?.version ?? "";
  const pin = spriteBootstrapPin(publishedVersion);
  const downloadRef = pin.ref === "latest" ? '${detent_release}' : pin.ref;
  const downloadCommand = `${pin.ref === "latest" ? `${LATEST_RELEASE_COMMAND} && ` : ""}curl -fsSL https://raw.githubusercontent.com/digitaldrywood/detent/${downloadRef}/scripts/sprite-runner-bootstrap.sh -o sprite-runner-bootstrap.sh`;
  const bootstrapCommand = pin.release === null
    ? `${LATEST_RELEASE_COMMAND} && bash sprite-runner-bootstrap.sh --version "$detent_release"`
    : `bash sprite-runner-bootstrap.sh --version ${pin.release}`;
  const [showToken, setShowToken] = React.useState(false);
  const [step, setStep] = React.useState(0);
  const [enrolledRunner, setEnrolledRunner] = React.useState<FleetRunner | null>(null);
  const [connected, setConnected] = React.useState<FleetRunner | null>(null);

  // Reopening starts a fresh enrollment: the previous token was shown once and
  // leaving it on screen invites redeeming a token that has already expired.
  React.useEffect(() => {
    if (open) return;
    generation.current += 1;
    setName(initialName);
    setCapacityText("1");
    setMachineIsolationTier(null);
    setService(true);
    setLocation("machine");
    setPlatform(runnerInstallPlatform());
    setEnrollment(null);
    setShowToken(false);
    setConnected(null);
    setEnrolledRunner(null);
    setStep(0);
    setSelected(initialSelection());
    setScope(projectIds === undefined ? "organization" : "projects");
  }, [open, initialSelection, initialName]);

  React.useEffect(() => {
    if (!open || enrollment === null || connected !== null) return;
    const runner = fleet.value?.runners.find((entry) =>
      !enrollment.existingRunnerIds.includes(entry.id) &&
      entry.display_name === enrollment.name,
    );
    if (runner === undefined) return;
    setEnrolledRunner(runner);
    setShowToken(false);
    if (enrollment.command !== enrollment.maskedCommand) {
      setEnrollment({ ...enrollment, command: enrollment.maskedCommand });
    }
    if (runner.has_heartbeat !== true) return;
    setConnected(runner);
    onConnected?.({ id: enrollment.id, expiresAt: enrollment.expiresAt, name: enrollment.name });
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

  const create = useMutation(async (baseline: FleetResponse) => {
    if (isolationTier === null) throw new Error("Choose agent access before creating a command.");
    const mine = generation.current;
    const existingRunnerIds = baseline.runners.map((runner) => runner.id);
    const created = await api.enrollRunner({
      scope,
      projectIds: scope === "organization" ? [] : selected,
      operations: [...ENROLLMENT_OPERATIONS],
      ttlSeconds: ENROLLMENT_TTL_SECONDS,
    });
    const entry: PendingEnrollment = {
      id: created.id,
      expiresAt: created.expires_at,
      name: name.trim() || "Unnamed runner",
    };
    if (generation.current === mine) {
      setStep(1);
      const input = { hubUrl, organizationId, token: created.token, name: entry.name, capacity: capacity ?? 1, isolationTier, service: location === "machine" && service };
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
  const spriteNameFits = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/.test(name.trim());
  const ready = fleet.value !== undefined && (scope === "organization" || selected.length > 0) && capacity !== null && isolationTier !== null && nameFits && (location === "machine" || spriteNameFits);
  const baselinePending = enrollment === null && fleet.value === undefined;
  const minimumVersion = fleet.value?.minimum_runner_version ?? "";
  const installInstructions = <div className="flex flex-col gap-2">
    <h3 className="text-[13px] font-medium">Install Detent</h3>
    <fieldset className="flex flex-col gap-2 text-[13px]">
      <legend className="pb-1 font-medium">Operating system</legend>
      <div className="flex gap-4">
        {([["macos", "macOS"], ["linux", "Linux"]] as const).map(([value, label]) => (
          <label key={value} className="flex items-center gap-2">
            <input type="radio" name="runner-platform" value={value} checked={platform === value} onChange={() => setPlatform(value)} />
            {label}
          </label>
        ))}
      </div>
    </fieldset>
    <p className="text-xs text-muted-foreground">Installs the latest release for {platform === "macos" ? "macOS" : "Linux"}, automatically detecting amd64 or arm64.</p>
    <CopyableCommand value={INSTALL_DETENT_COMMAND} label="the install command" />
    {platform === "macos" ? <>
      <p className="text-xs text-muted-foreground">Or install with Homebrew:</p>
      <CopyableCommand value={BREW_INSTALL_DETENT_COMMAND} label="the Homebrew command" />
    </> : null}
    {minimumVersion ? <p className="text-xs text-muted-foreground">This Hub requires Detent {minimumVersion} or newer. Check with <code>detent version</code>; if yours is older, update with <code>detent update --yes --from-release</code>{platform === "macos" ? <> or <code>brew upgrade digitaldrywood/tap/detent</code> for Homebrew</> : null} before registering.</p> : null}
  </div>;

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
          {step === 2 && connected !== null ? (
            <section className="flex flex-col gap-3" aria-labelledby="enroll-connected">
              <h3 id="enroll-connected" role="status" className="text-sm font-medium">
                {connected.display_name} is connected
              </h3>
              {location === "sprite" && canWake ? <p className="text-sm text-muted-foreground">Sleeps when idle; the Hub wakes it for new work</p> : null}
              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 text-[13px]">
                <dt className="text-muted-foreground">Hostname</dt><dd className="break-all">{connected.hostname}</dd>
                <dt className="text-muted-foreground">OS / architecture</dt><dd>{connected.os} / {connected.architecture}</dd>
                <dt className="text-muted-foreground">Slots</dt><dd>{connected.reported_capacity}</dd>
                <dt className="text-muted-foreground">Providers</dt>
                <dd>{[...new Set(connected.provider_capacity.map((provider) => provider.provider))].join(", ") || "None reported"}</dd>
              </dl>
              {connected.provider_capacity.length === 0 ? (
                <Alert variant="warning">
                  <AlertTitle>No providers reported</AlertTitle>
                  <AlertDescription>
                    This runner cannot take work yet. Check that its provider CLIs are installed, signed in, and available on the service PATH, then check the runner service logs.
                  </AlertDescription>
                </Alert>
              ) : null}
            </section>
          ) : step === 0 ? (
            <fieldset disabled={enrollment !== null} className="flex flex-col gap-4">
              <fieldset className="flex flex-col gap-2 text-[13px]">
                <legend id="enroll-agent-access" className="pb-1 font-medium">Agent access</legend>
                <RadioGroup aria-labelledby="enroll-agent-access" value={isolationTier} onValueChange={(value) => setMachineIsolationTier(value as RunnerAccessTier)}>
                  {runnerAccessOptions.map(({ tier, label, hint }) => (
                    <label key={tier} className="flex cursor-pointer items-start gap-2">
                      <Radio value={tier} disabled={location === "sprite" && tier === "sandbox"} aria-labelledby={`enroll-access-${tier}`} aria-describedby={`enroll-access-${tier}-hint`} className="mt-1" />
                      <span><span id={`enroll-access-${tier}`} className="block font-medium">{label}</span><span id={`enroll-access-${tier}-hint`} className="block text-xs text-muted-foreground">{location === "sprite" && tier === "sandbox" ? spriteSandboxUnavailableReason : hint}</span></span>
                    </label>
                  ))}
                </RadioGroup>
                {isolationTier === null ? <p className="text-xs text-muted-foreground">Choose Sandbox or Full access to create a command.</p> : null}
              </fieldset>
              <fieldset className="flex flex-col gap-2 text-[13px]">
                <legend className="pb-1 font-medium">Where will it run?</legend>
                {([["machine", "A machine I run"], ["sprite", "A Fly Sprite"]] as const).map(([value, label]) => (
                  <label key={value} className="flex items-center gap-2">
                    <input type="radio" name="runner-location" value={value} checked={location === value} onChange={() => {
                      setLocation(value);
                      if (value === "sprite") setName(name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 63).replace(/-+$/g, "") || `detent-${globalThis.crypto.randomUUID().slice(0, 8)}`);
                    }} />
                    {label}
                  </label>
                ))}
              </fieldset>
              {location === "machine" ? installInstructions : null}
              {location === "sprite" ? <>
                <p className="text-xs text-muted-foreground">The Sprite gets the same name as this runner. Use lowercase letters, numbers and hyphens.</p>
                {!spriteNameFits ? <p className="text-xs text-destructive-foreground">Enter a lowercase Sprite name, up to 63 characters, starting and ending with a letter or number.</p> : null}
                {pin.release === null ? <p className="text-xs text-warning">The bootstrap installs the latest tagged release. Upgrade the runner to {publishedVersion || "the Hub’s build"} afterwards.</p> : null}
                {sprites.loading ? <p className="text-xs text-muted-foreground">Checking Sprites tokens…</p> : sprites.value?.filter((entry) => entry.present !== true).map((entry) => (
                  <p key={entry.id} className="text-xs text-warning">
                    {entry.present === false ? "No Sprites token is set" : "Could not check the Sprites token"} for {entry.id === "" ? "the organization" : projects.find((project) => project.id === entry.id)?.name}. Without it the Hub cannot wake the Sprite.{entry.id === "" ? "" : " You can uncheck the project to continue without it."}{" "}
                    {entry.error === null ? null : <>{entry.error}{" "}</>}
                    <a className="underline" href={`${bootstrap?.base_path ?? ""}${entry.id === "" ? "/settings/integrations" : projectSettingsPath(bootstrap?.projects.find((project) => project.id === entry.id) ?? entry.id, "runners")}#sprites`}>Set a Sprites token</a>
                  </p>
                ))}
              </> : null}
              <div className="flex flex-col gap-2 sm:flex-row">
                <div className="flex flex-1 flex-col gap-1.5">
                  <Label htmlFor="enroll-runner-name" className="sm:min-h-7">Name</Label>
                  <Input
                    id="enroll-runner-name"
                    autoComplete="off"
                    spellCheck={false}
                    aria-invalid={!nameFits || (location === "sprite" && !spriteNameFits)}
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
                <NativeSelect aria-label="Runner scope" value={scope} onValueChange={(value) => setScope(value as "organization" | "projects")} options={[{ value: "organization", label: "All projects" }, { value: "projects", label: "Selected projects" }]} />
                {scope === "organization" ? <p className="text-xs text-muted-foreground">Serves all current and future projects, subject to each project’s approved policy and capacity.</p> : projects.length === 0 ? (
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
              {location === "machine" ? <label className="flex items-start gap-2 text-[13px]" htmlFor="enroll-runner-service">
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
              </label> : null}
            </fieldset>
          ) : enrollment !== null ? (
            <section className="flex flex-col gap-2" aria-labelledby="enroll-run-command">
              {location === "sprite" ? (
                <>
                  <h3 id="enroll-run-command" className="text-[13px] font-medium">Set up {enrollment.name} on a Fly Sprite</h3>
                  <ol aria-label="Sprite setup instructions" className="list-decimal space-y-4 pl-5 text-[13px]">
                    <li>On your own machine, sign in with <code>sprite login</code>. Use the Fly organization whose token is configured for these projects{spriteOrganization ? `: ${spriteOrganization}` : '; set SPRITE_ORG to that organization first'}. Check access, then create the Sprite.
                      <CopyableCommand value={`sprite list -o ${spriteOrganizationArgument} && sprite create -o ${spriteOrganizationArgument} ${shellArgument(enrollment.name)}`} label="the create command" />
                    </li>
                    <li>On your own machine, open its console.
                      <CopyableCommand value={`sprite console -o ${spriteOrganizationArgument} -s ${shellArgument(enrollment.name)}`} label="the console command" />
                    </li>
                    <li>Inside the Sprite, run <code>cd "$HOME"</code>, then download the bootstrap script {pin.ref === "latest" ? "from the latest tagged release" : `pinned to ${pin.ref}`}.
                      <CopyableCommand value={downloadCommand} label="the download command" />
                    </li>
                    <li>Run the bootstrap. It installs Detent and the toolchain, registers the runner, and starts the detent-runner Sprite Service.
                      <CopyableCommand value={bootstrapCommand} label="the bootstrap command" />
                      {pin.release === null ? <p className="text-xs text-warning">Upgrade the installed runner to {publishedVersion || "the Hub’s build"} afterwards.</p> : null}
                    </li>
                    <li>Copy this complete enrollment command and paste it at the bootstrap’s hidden prompt, then press Enter. Paste is invisible. Empty input is only for an already registered runner; if the token expires before registration, create a fresh command and rerun the same bootstrap.
                      <CopyableCommand value={enrollment.command} displayValue={enrollment.maskedCommand} label="the register command" />
                    </li>
                    <li>Inside the Sprite, sign in to the providers your project uses: <code>claude auth login</code> or <code>codex login --device-auth</code>. Verify with <code>claude auth status</code> or <code>codex login status</code>.</li>
                    <li>Inside the Sprite, authenticate GitHub, then check repository access with <code>git ls-remote REPOSITORY_URL HEAD</code> using your repository URL.
                      <CopyableCommand value="gh auth login --hostname github.com --git-protocol https --web && gh auth setup-git && gh auth status --hostname github.com" label="the GitHub authentication command" />
                      The runner clones allowed repositories and runs their <code>hooks.runner_setup</code>. For a fresh runner, each managed checkout is <code>~/detent-runner/PROJECT_ID</code>, using the project’s exact Hub ID. Use the bootstrap’s printed paths for project-scoped enrollment. Verify the checkout and its origin before running dependencies there with repository-pinned tools; keep <code>cd</code> and dependency commands joined with <code>&amp;&amp;</code>.
                    </li>
                    <li>Check the Hub’s runner connection, project setup and reported repository policy; approve policy only if offered. Re-run the same bootstrap with empty input after authentication, then inspect <code>sprite-env services get detent-runner</code>. Its installation checkpoint does not prove readiness. Confirm a small authorized Todo task publishes its real Change, then save a final checkpoint with <code>sprite-env checkpoints create</code>. If any step fails, use the bootstrap’s last completed step and resume command. See the <a className="underline" href="https://github.com/digitaldrywood/detent/blob/main/docs/sprite-runners.md">Sprite runner guide</a> for dependency and storage recovery.</li>
                  </ol>
                </>
              ) : <>
              {installInstructions}
              <h3 id="enroll-run-command" className="text-[13px] font-medium">
                Run this on the machine that will take the work.
              </h3>
              <p className="text-xs text-muted-foreground">Fresh machine? Install and register with one paste:</p>
              <CopyableCommand
                value={installAndRegisterCommand(enrollment.command)}
                displayValue={installAndRegisterCommand(showToken ? enrollment.command : enrollment.maskedCommand)}
                label="the install and register command"
              />
              <p className="text-xs text-muted-foreground">Already installed? Register only:</p>
              <CopyableCommand
                value={enrollment.command}
                displayValue={showToken ? enrollment.command : enrollment.maskedCommand}
                label="the register command"
              />
              {enrolledRunner === null ? <Button
                size="xs"
                variant="outline"
                className="self-start"
                aria-pressed={showToken}
                onClick={() => setShowToken((current) => !current)}
              >
                {showToken ? "Hide token" : "Show token"}
              </Button> : null}
              </>}
              <p role="status" className="text-[13px] text-muted-foreground">
                {connected !== null ? `${connected.display_name} is connected` : enrolledRunner !== null
                  ? `${enrollment.name} is enrolled, waiting for the runner to start.`
                  : <>Waiting for {enrollment.name || "the runner"} to check in. The command works once, until{" "}
                    {new Date(enrollment.expiresAt).toLocaleTimeString()}.</>}
              </p>
              {enrolledRunner !== null ? <p className="text-xs text-muted-foreground">The one-time token has been used and is no longer available. You can still copy the command with the token masked.</p> : null}
              <p className="text-xs text-muted-foreground">
                Closing this dialog clears the command from the page. The runner still connects if you already ran it.
              </p>
            </section>
          ) : null}
          {baselinePending && fleet.error === null ? (
            <p role="status" className="text-xs text-muted-foreground">Reading runners before creating a command…</p>
          ) : null}
          <ControlError message={enrollmentError(create.error ?? fleet.error)} />
          {baselinePending && fleet.error !== null ? (
            <Button variant="outline" disabled={fleet.loading} onClick={() => void fleet.refresh()}>Retry runner read</Button>
          ) : null}
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline">{connected !== null ? "Done" : enrollment === null ? "Cancel" : "Close"}</Button>} />
          {step > 0 ? <Button variant="outline" onClick={() => setStep((current) => current - 1)}>Back</Button> : null}
          {enrollment !== null && step < 2 ? (
            <Button disabled={step === 1 && connected === null} onClick={() => setStep((current) => current + 1)}>Next</Button>
          ) : enrollment === null ? (
            <Button disabled={!ready || create.pending} onClick={() => void create.call(fleet.value!)}>
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
