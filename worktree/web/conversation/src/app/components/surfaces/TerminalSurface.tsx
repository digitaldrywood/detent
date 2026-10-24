import { PlusIcon, XIcon } from "lucide-react";
import React from "react";
import type { FitAddon } from "@xterm/addon-fit";
import type { Terminal as XTerm } from "@xterm/xterm";

import { DiffPanelShell } from "../../../components/DiffPanelShell.tsx";
import { Button } from "../../../components/ui/button.tsx";
import {
  Menu,
  MenuItem,
  MenuPopup,
  MenuShortcut,
  MenuTrigger,
} from "../../../components/ui/menu.tsx";
import { cn } from "../../../lib/utils.ts";
import { getTerminalLabel } from "../../adapters/terminalLabels.ts";
import type { WorkspaceReason, WorkspaceState } from "../../../contracts/work.ts";
import type { TerminalHandle, TerminalSnapshot } from "../../adapters/terminalStream.ts";
import {
  describeWorkspaceStatus,
  type WorkspaceSessionFacts,
} from "../../lib/workspaceStatus.ts";
import { WorkspaceStatusView } from "./WorkspaceStatusView.tsx";

export interface TerminalSurfaceProps {
  readonly terminals: readonly TerminalHandle[];
  readonly activeId: string | null;
  readonly onSelect: (id: string) => void;
  /** §18.2: a second terminal is its own stream and its own PTY. */
  readonly onNewTerminal: () => void;
  readonly onCloseTerminal: (id: string) => void;
  readonly registerOutput: (id: string, sink: (data: string) => void) => () => void;
  /** Null before the hub has answered at all. */
  readonly state: WorkspaceState | null;
  readonly reason: WorkspaceReason | null;
  /** A request that failed outright, rather than a workspace that failed. */
  readonly error: string | null;
  readonly loading: boolean;
  readonly session?: WorkspaceSessionFacts | null;
  readonly onRetry: () => void;
}

function EmptyState({
  children,
  testId,
}: {
  children: React.ReactNode;
  testId: string;
}): React.ReactElement {
  return (
    <div
      className="flex items-center justify-center px-3 py-6 text-center text-muted-foreground/70 text-xs"
      data-testid={testId}
    >
      <p>{children}</p>
    </div>
  );
}

/** The sentence a terminal's status is worth to a reader. */
export function terminalStatusLabel(snapshot: TerminalSnapshot): string {
  switch (snapshot.status) {
    case "connecting":
      return "Connecting";
    case "open":
      return "Connected";
    case "reconnecting":
      // §18.2 gives a dropped connection sixty seconds, and the runner keeps
      // the PTY alive exactly that long, so this is a wait rather than a loss.
      return "Reconnecting";
    case "exited":
      return snapshot.exitCode === null
        ? snapshot.signal === null
          ? "Closed"
          : `Killed by ${snapshot.signal}`
        : `Exited ${snapshot.exitCode}`;
    case "failed":
      return "Failed";
  }
}

function TerminalStatusChip({ snapshot }: { snapshot: TerminalSnapshot }): React.ReactElement {
  const tone =
    snapshot.status === "failed"
      ? "bg-destructive/12 text-destructive"
      : "bg-accent text-accent-foreground";
  return (
    <span
      className={cn(
        "inline-flex h-6 max-w-full shrink-0 items-center gap-1 rounded-md px-2 font-medium text-xs",
        tone,
      )}
      data-testid="terminal-status"
      data-status={snapshot.status}
    >
      <span className="truncate">{terminalStatusLabel(snapshot)}</span>
    </span>
  );
}

function terminalTheme(): Record<string, string> {
  const fallback = { background: "#0b0b0d", foreground: "#e6e6e6", cursor: "#e6e6e6" };
  if (typeof globalThis.getComputedStyle !== "function" || typeof document === "undefined") {
    return fallback;
  }
  const style = globalThis.getComputedStyle(document.documentElement);
  const token = (name: string, backup: string): string => {
    const value = style.getPropertyValue(name).trim();
    return value.length > 0 ? value : backup;
  };
  return {
    background: token("--color-card", fallback.background),
    foreground: token("--color-foreground", fallback.foreground),
    cursor: token("--color-foreground", fallback.cursor),
    selectionBackground: token("--color-accent", "#3b3b45"),
  };
}

/**
 * One live terminal.
 *
 * The xterm instance is created once per terminal id and torn down with it. It
 * is deliberately not re-created on a status change: a reconnect inside §18.2's
 * window puts the person back in front of the same shell, and a screen wiped
 * and redrawn would lose the scrollback the resume exists to preserve.
 */
function TerminalView({
  terminal,
  registerOutput,
  active,
}: {
  terminal: TerminalHandle;
  registerOutput: (id: string, sink: (data: string) => void) => () => void;
  active: boolean;
}): React.ReactElement {
  const host = React.useRef<HTMLDivElement | null>(null);
  const stream = terminal.stream;
  const view = React.useRef<XTerm | null>(null);
  const fit = React.useRef<FitAddon | null>(null);

  // xterm is loaded when a terminal is opened rather than when the module is.
  //
  // Two reasons, and both are load-bearing. It is a renderer nobody who never
  // opens a shell needs, and the bundle §18.11 keeps an eye on should not carry
  // it on every page. And `@xterm/addon-fit` ships as a UMD bundle that reaches
  // for `self` at module scope, which does not exist outside a browser — a
  // static import therefore breaks every Node-environment test that
  // transitively imports this panel, and a surface must not be able to do that
  // to tests that have nothing to do with it.
  const [ready, setReady] = React.useState(false);
  React.useEffect(() => {
    const element = host.current;
    if (element === null) return;
    let disposed = false;
    let created: XTerm | null = null;
    void Promise.all([import("@xterm/xterm"), import("@xterm/addon-fit"), import("@xterm/xterm/css/xterm.css")])
      .then(([terminalModule, fitModule]) => {
        if (disposed) return;
        const xterm = new terminalModule.Terminal({
          convertEol: false,
          cursorBlink: true,
          fontSize: 12,
          fontFamily:
            'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
          theme: terminalTheme(),
          scrollback: 5_000,
        });
        const addon = new fitModule.FitAddon();
        xterm.loadAddon(addon);
        xterm.open(element);
        created = xterm;
        view.current = xterm;
        fit.current = addon;
        setReady(true);
      })
      .catch(() => {
        // A renderer that could not be loaded leaves the shell unreadable, and
        // the surface says so through the stream's own status rather than by
        // drawing a blank box: there is nothing useful to render here.
      });
    return () => {
      disposed = true;
      view.current = null;
      fit.current = null;
      created?.dispose();
    };
  }, []);

  // Output goes straight into xterm rather than through React state: a shell
  // printing a build log would otherwise re-render the panel thousands of
  // times, and the bytes are the renderer's business rather than the tree's.
  React.useEffect(() => {
    if (!ready) return;
    return registerOutput(terminal.id, (data) => view.current?.write(data));
  }, [ready, registerOutput, terminal.id]);

  // Keystrokes go to the runner. `onData` already carries the bytes a terminal
  // expects for control keys, so nothing here interprets them: §18.3 hands the
  // shell what the person typed.
  React.useEffect(() => {
    const xterm = view.current;
    if (!ready || xterm === null || stream === null) return;
    const subscription = xterm.onData((data) => stream.write(data));
    return () => subscription.dispose();
  }, [ready, stream]);

  // The window follows the container. A resize is a real ioctl on the runner
  // (§18.3), so a program drawing to the window learns about it from SIGWINCH
  // rather than from a number the client kept to itself.
  React.useEffect(() => {
    const element = host.current;
    if (element === null) return;
    const apply = (): void => {
      const addon = fit.current;
      const xterm = view.current;
      if (addon === null || xterm === null) return;
      try {
        addon.fit();
      } catch {
        // fit() measures the DOM, and a container with no size yet throws
        // rather than answering zero. A terminal that has not been laid out is
        // not a terminal to resize.
        return;
      }
      stream?.resize(xterm.cols, xterm.rows);
    };
    apply();
    if (typeof ResizeObserver !== "function") return;
    const observer = new ResizeObserver(apply);
    observer.observe(element);
    return () => observer.disconnect();
  }, [ready, stream, active]);

  React.useEffect(() => {
    if (!active || !ready) return;
    view.current?.focus();
  }, [active, ready]);

  return (
    <div

      data-terminal-owner="right-panel"
      data-testid={`terminal-view-${terminal.id}`}
      className={cn("min-h-0 flex-1 overflow-hidden p-2", active ? "flex" : "hidden")}
    >
      <div className="min-h-0 min-w-0 flex-1" ref={host} />
    </div>
  );
}

export function TerminalSurface({
  terminals,
  activeId,
  onSelect,
  onNewTerminal,
  onCloseTerminal,
  registerOutput,
  state,
  reason,
  error,
  loading,
  session = null,
  onRetry,
}: TerminalSurfaceProps): React.ReactElement {
  const active =
    terminals.find((terminal) => terminal.id === activeId) ??
    terminals[terminals.length - 1] ??
    null;

  const header = (
    <>
      <div className="flex min-w-0 flex-1 items-center gap-2">
        {active === null ? (
          <span className="truncate text-muted-foreground/70 text-xs">No terminal</span>
        ) : (
          <>
            <TerminalStatusChip snapshot={active.snapshot} />
            {terminals.length > 1 ? (
              <Menu highlightItemOnHover={false}>
                <MenuTrigger
                  render={<Button size="xs" variant="ghost" aria-label="Choose a terminal" />}
                  data-testid="terminal-picker"
                >
                  <span className="truncate">{getTerminalLabel(active.id)}</span>
                </MenuTrigger>
                <MenuPopup align="end">
                  {terminals.map((terminal) => (
                    <MenuItem
                      key={terminal.id}
                      data-testid={`terminal-item-${terminal.id}`}
                      onClick={() => onSelect(terminal.id)}
                    >
                      <span className="truncate">{getTerminalLabel(terminal.id)}</span>
                      <MenuShortcut className="ms-auto">
                        {terminalStatusLabel(terminal.snapshot)}
                      </MenuShortcut>
                    </MenuItem>
                  ))}
                </MenuPopup>
              </Menu>
            ) : (
              <span className="truncate text-muted-foreground/70 text-xs">
                {getTerminalLabel(active.id)}
              </span>
            )}
          </>
        )}
      </div>
      <div className="flex shrink-0 items-center gap-1 pe-1">
        {active !== null ? (
          <Button
            size="xs"
            variant="ghost"
            aria-label="Close this terminal"
            data-testid="terminal-close"
            onClick={() => onCloseTerminal(active.id)}
          >
            <XIcon className="size-3" />
          </Button>
        ) : null}
        <Button
          size="xs"
          variant="ghost"
          aria-label="New terminal"
          data-testid="terminal-new"
          onClick={onNewTerminal}
        >
          <PlusIcon className="size-3" />
        </Button>
      </div>
    </>
  );

  const body = ((): React.ReactNode => {
    if (error !== null) {
      return (
        <div className="flex flex-col items-center gap-2 px-3 py-6 text-center">
          <EmptyState testId="terminal-error">{error}</EmptyState>
          <Button size="xs" variant="ghost" onClick={onRetry}>
            Try again
          </Button>
        </div>
      );
    }
    if (terminals.length === 0) {
      const status = describeWorkspaceStatus({
        state,
        reason,
        capability: "terminal",
        session,
        // A live workspace with no shell yet is still connecting: the relay
        // socket is what the surface waits on, and `connected` is the flag
        // `WORKSPACE_CONNECTING_STATUS` exists for.
        connected: !loading,
      });
      if (status !== null) {
        return <WorkspaceStatusView status={status} testIdPrefix="terminal" onRetry={onRetry} />;
      }
      return (
        <EmptyState testId="terminal-empty">
          Open a terminal to run a shell on this workspace&apos;s worktree.
        </EmptyState>
      );
    }
    return (
      <div className="flex min-h-0 flex-1 flex-col">
        {active !== null && active.snapshot.error !== null ? (
          <p className="px-3 pt-2 text-destructive text-xs" data-testid="terminal-run-error">
            {active.snapshot.error}
          </p>
        ) : null}
        {active !== null && active.snapshot.status === "exited" ? (
          // §18.3's `exit` shown in the surface. A shell that ended is not an
          // error and not an empty screen: the scrollback stays, and this says
          // why nothing more is coming.
          <p className="px-3 pt-2 text-muted-foreground/70 text-xs" data-testid="terminal-exit">
            {terminalStatusLabel(active.snapshot)} — open a new terminal to keep working.
          </p>
        ) : null}
        {terminals.map((terminal) => (
          <TerminalView
            key={terminal.id}
            terminal={terminal}
            registerOutput={registerOutput}
            active={terminal.id === active?.id}
          />
        ))}
      </div>
    );
  })();

  return (
    <DiffPanelShell mode="sheet" header={header}>
      <div className="flex min-h-0 flex-1 flex-col" data-testid="terminal-surface">
        {body}
      </div>
    </DiffPanelShell>
  );
}
