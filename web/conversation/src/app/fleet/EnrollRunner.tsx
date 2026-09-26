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
import { Kbd } from "../../components/ui/kbd.tsx";
import { Label } from "../../components/ui/label.tsx";
import type { RunnerEnrollment } from "../../contracts/account.ts";
import { ControlError } from "../account/controls.tsx";
import { useAccountApi, useAccountBootstrap } from "../account/context.ts";
import { useMutation } from "../account/useResource.ts";
import { SettingsRow } from "../settings/settingsLayout.tsx";

/** The operations a runner needs to take issue runs and coordinator turns. */
const ENROLLMENT_OPERATIONS = ["read", "collaborate", "claim", "heartbeat", "events"] as const;

/** The hub refuses anything over 900 (`runnerauth.MaxEnrollmentTTL`). */
const ENROLLMENT_TTL_SECONDS = 900;

/** One enrollment this screen created, with the command that redeems it. */
export interface PendingEnrollment {
  readonly id: string;
  readonly token: string;
  readonly expiresAt: string;
  readonly runnerId: string;
  readonly projectNames: readonly string[];
}

/**
 * `detent hub runner init` on the host. The URL is the organization's own
 * public URL, because that is the address the runner has to reach, not
 * whatever the reader happens to have in the address bar.
 */
export function initCommand(hubUrl: string): string {
  return `detent hub runner init --hub-url ${hubUrl}`;
}

/**
 * The redemption command. The token travels in the environment variable the
 * CLI reads (`--enrollment-token-env`, default `DETENT_RUNNER_ENROLLMENT_TOKEN`)
 * rather than in an argument, so it stays out of the host's process list.
 */
export function enrollCommand(token: string, organizationId: string): string {
  return `DETENT_RUNNER_ENROLLMENT_TOKEN=${token} detent hub runner enroll --organization ${organizationId}`;
}

/** A monospace value with the copy affordance, sized for a command line. */
export function CopyableCommand({
  value,
  label,
}: {
  readonly value: string;
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
      <code className="min-w-0 flex-1 break-all font-mono text-xs text-foreground">{value}</code>
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
}: {
  readonly open: boolean;
  readonly onOpenChange: (open: boolean) => void;
  readonly onEnrolled: (enrollment: PendingEnrollment) => void;
}): React.ReactElement {
  const api = useAccountApi();
  const bootstrap = useAccountBootstrap();
  const organizationId = bootstrap?.organization.id ?? "";
  // The URL the runner has to reach is the organization's published one, not
  // the address bar: a reader behind a tunnel or a preview host would
  // otherwise copy an address the host cannot resolve. `location.origin` is
  // the fallback for a payload that carries no public URL.
  const hubUrl =
    bootstrap?.organizations.find((entry) => entry.current)?.public_url ??
    globalThis.location?.origin ??
    "";
  const projects = React.useMemo(() => bootstrap?.projects ?? [], [bootstrap]);

  const [runnerId, setRunnerId] = React.useState("");
  const [machineId, setMachineId] = React.useState("");
  // Every readable project by default: a host the reader is enrolling is
  // normally the host for their whole organization, and narrowing it is the
  // deliberate act, not widening it.
  const [selected, setSelected] = React.useState<readonly string[]>(() =>
    projects.map((project) => project.id),
  );
  const [enrollment, setEnrollment] = React.useState<RunnerEnrollment | null>(null);

  // Reopening starts a fresh enrollment: the previous token was shown once and
  // leaving it on screen invites redeeming a token that has already expired.
  React.useEffect(() => {
    if (open) return;
    setRunnerId("");
    setMachineId("");
    setEnrollment(null);
    setSelected(projects.map((project) => project.id));
  }, [open, projects]);

  const create = useMutation(async () => {
    const created = await api.enrollRunner({
      projectIds: selected,
      runnerId: runnerId.trim(),
      machineId: machineId.trim(),
      operations: [...ENROLLMENT_OPERATIONS],
      ttlSeconds: ENROLLMENT_TTL_SECONDS,
    });
    setEnrollment(created);
    onEnrolled({
      id: created.id,
      token: created.token,
      expiresAt: created.expires_at,
      runnerId: runnerId.trim(),
      projectNames: projects
        .filter((project) => selected.includes(project.id))
        .map((project) => project.name),
    });
    return created;
  });

  const ready =
    runnerId.trim().length > 0 && machineId.trim().length > 0 && selected.length > 0;

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
          <section className="flex flex-col gap-2" aria-labelledby="enroll-step-one">
            <h3 id="enroll-step-one" className="text-[13px] font-medium">
              1. Generate the host identity
            </h3>
            <p className="text-[13px] text-muted-foreground">
              Run this on the machine that will take the work, then paste the two identifiers it
              prints. They are generated on the host and the hub never sees the credential behind
              them.
            </p>
            <CopyableCommand value={initCommand(hubUrl)} label="the init command" />
          </section>

          <section className="flex flex-col gap-2" aria-labelledby="enroll-step-two">
            <h3 id="enroll-step-two" className="text-[13px] font-medium">
              2. Name the host and its projects
            </h3>
            <div className="flex flex-col gap-2 sm:flex-row">
              <div className="flex flex-1 flex-col gap-1.5">
                <Label htmlFor="enroll-runner-id">Runner id</Label>
                <Input
                  id="enroll-runner-id"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="runner_…"
                  value={runnerId}
                  onChange={(event) => setRunnerId(event.currentTarget.value)}
                />
              </div>
              <div className="flex flex-1 flex-col gap-1.5">
                <Label htmlFor="enroll-machine-id">Machine id</Label>
                <Input
                  id="enroll-machine-id"
                  autoComplete="off"
                  spellCheck={false}
                  placeholder="machine_…"
                  value={machineId}
                  onChange={(event) => setMachineId(event.currentTarget.value)}
                />
              </div>
            </div>
            <fieldset className="flex flex-col gap-2">
              <legend className="pb-1 text-[13px] font-medium">Projects</legend>
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
          </section>

          {enrollment === null ? null : (
            <section className="flex flex-col gap-2" aria-labelledby="enroll-step-three">
              <h3 id="enroll-step-three" className="text-[13px] font-medium">
                3. Redeem it on the host
              </h3>
              <p role="status" className="text-[13px] text-muted-foreground">
                This token is shown once, expires {new Date(enrollment.expires_at).toLocaleString()}
                , and works only for the identifiers above. Copy it now; closing this dialog with{" "}
                <Kbd>Esc</Kbd> throws it away.
              </p>
              <CopyableCommand value={enrollment.token} label="the enrollment token" />
              <CopyableCommand
                value={enrollCommand(enrollment.token, organizationId)}
                label="the enroll command"
              />
            </section>
          )}
          <ControlError message={create.error?.message ?? null} />
        </DialogPanel>
        <DialogFooter>
          <DialogClose render={<Button variant="outline">Close</Button>} />
          <Button disabled={!ready || create.pending} onClick={() => void create.call()}>
            {create.pending
              ? "Creating…"
              : enrollment === null
                ? "Create enrollment token"
                : "Create another"}
          </Button>
        </DialogFooter>
      </DialogPopup>
    </Dialog>
  );
}

/** The rows under the Runners section: what this screen has handed out. */
export function PendingEnrollments({
  enrollments,
  organizationId,
}: {
  readonly enrollments: readonly PendingEnrollment[];
  readonly organizationId: string;
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
              <span className="truncate font-mono text-xs">{entry.runnerId}</span>
            </span>
          }
          description={`Waiting to be redeemed · ${
            entry.projectNames.length === 0 ? "no projects" : entry.projectNames.join(", ")
          } · expires ${new Date(entry.expiresAt).toLocaleTimeString()}`}
          control={
            <CopyableCommand
              value={enrollCommand(entry.token, organizationId)}
              label={`the enroll command for ${entry.runnerId}`}
            />
          }
        />
      ))}
    </>
  );
}
