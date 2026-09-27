import type { PreferenceField } from "../components/ComposerPreferenceControls.tsx";

export interface SlashCommand {
  /** The command's name without its leading slash, e.g. `model`. */
  readonly name: string;
  /** One line, said the way the menu shows it. */
  readonly description: string;
  readonly run: () => void;
}

/**
 * What the composer itself can do, as the built-in commands see it.
 *
 * Every member is nullable because a composer surface that cannot do the thing
 * must not offer the command: the draft composer has no turn to interrupt, the
 * issue page's comment mode has no pickers and no attachments, and a reader
 * who may not send has none of it. A command that would do nothing is worse
 * than a command that is not there, because the reader cannot tell which.
 */
export interface ComposerSlashContext {
  /** Opens one of the three footer pickers. Null where the footer has none. */
  readonly openPreference: ((field: PreferenceField) => void) | null;
  /** Opens the file chooser. Null where this surface takes no attachments. */
  readonly attach: (() => void) | null;
  /** Empties the draft. Null for a composer that may not be edited. */
  readonly clear: (() => void) | null;
  /** Interrupts the running turn. Null unless one is actually running. */
  readonly stop: (() => void) | null;
}

/**
 * The rows a runner's own skills and prompts would fill.
 *
 * `ServerProviderSlashCommand` is in the contracts and `runtime/providerSkills.ts`
 * knows how to present them, but the hub serves neither yet. The menu says so
 * in a disabled row rather than dropping the feature silently (decisions.md
 * §16) and rather than inventing entries that would not run.
 */
export const PROVIDER_SKILLS_NOTICE =
  "Runner skills and prompts appear here once the hub serves them";

/** The commands the composer provides for itself, in the order the menu shows them. */
export function builtinSlashCommands(context: ComposerSlashContext): readonly SlashCommand[] {
  const commands: SlashCommand[] = [];
  const openPreference = context.openPreference;
  if (openPreference !== null) {
    commands.push(
      {
        name: "model",
        description: "Switch the model this conversation's turns run on",
        run: () => openPreference("model"),
      },
      {
        name: "effort",
        description: "Switch how much reasoning effort a turn is given",
        run: () => openPreference("reasoning_effort"),
      },
      {
        name: "access",
        description: "Switch what a turn may do on the runner",
        run: () => openPreference("access"),
      },
    );
  }
  if (context.stop !== null) {
    const stop = context.stop;
    commands.push({
      name: "stop",
      description: "Interrupt the turn that is running",
      run: stop,
    });
  }
  if (context.attach !== null) {
    const attach = context.attach;
    commands.push({
      name: "attach",
      description: "Attach files to this message",
      run: attach,
    });
  }
  if (context.clear !== null) {
    const clear = context.clear;
    commands.push({
      name: "clear",
      description: "Clear the draft",
      run: clear,
    });
  }
  return commands;
}

/**
 * The built-ins followed by the surface's own commands.
 *
 * A surface command with a built-in's name replaces it in place rather than
 * appending a second row of the same name: two `/model` rows would be a menu
 * that cannot say which one a press runs.
 */
export function resolveSlashCommands(
  builtin: readonly SlashCommand[],
  extra: readonly SlashCommand[],
): readonly SlashCommand[] {
  const resolved = [...builtin];
  for (const command of extra) {
    const index = resolved.findIndex((candidate) => candidate.name === command.name);
    if (index === -1) resolved.push(command);
    else resolved[index] = command;
  }
  return resolved;
}

/**
 * The `/…` token a prompt opens with, or null when there is none.
 *
 * The menu opens on a slash typed at the start of an otherwise empty prompt —
 * leading whitespace is allowed, because a draft that begins with a stray
 * newline is still an empty draft — and closes the moment the prompt stops
 * being that token alone. A slash in the middle of a sentence is a slash, not
 * a command, and a space after the name ends the token: `/model please` is
 * prose about the model, and the menu must be out of the way for it.
 */
export function readSlashQuery(text: string): string | null {
  const match = /^\s*\/([A-Za-z0-9:_-]*)$/.exec(text);
  return match === null ? null : (match[1] ?? "");
}

/**
 * The commands a query matches, name prefixes first.
 *
 * Ranking a prefix above a mere containment is what makes "mod" land on
 * `/model` rather than on something that merely has "mod" inside it; within
 * each group the registry's own order is kept, so the menu never reshuffles
 * under the reader for a reason they cannot see.
 */
export function filterSlashCommands(
  commands: readonly SlashCommand[],
  query: string,
): readonly SlashCommand[] {
  const needle = query.trim().toLowerCase();
  if (needle.length === 0) return commands;
  const prefix: SlashCommand[] = [];
  const contains: SlashCommand[] = [];
  for (const command of commands) {
    const name = command.name.toLowerCase();
    if (name.startsWith(needle)) prefix.push(command);
    else if (name.includes(needle)) contains.push(command);
  }
  return [...prefix, ...contains];
}

/**
 * Where Up or Down moves the highlight, wrapping at both ends the way every
 * other menu in the client does. An empty list has no row to highlight, and
 * an index that has fallen off the end of a shrinking list comes back to the
 * first row rather than to nothing.
 */
export function stepSlashHighlight(
  count: number,
  index: number,
  direction: "up" | "down",
): number {
  if (count <= 0) return 0;
  const from = index < 0 || index >= count ? 0 : index;
  return direction === "down" ? (from + 1) % count : (from - 1 + count) % count;
}
