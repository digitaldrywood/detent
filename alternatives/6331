import {
  KeybindingRule as KeybindingRuleSchema,
  type KeybindingCommand,
  type KeybindingRule,
  type ResolvedKeybindingsConfig,
} from "../contracts/ui.ts";
import { registeredProjectActionChord } from "../app/adapters/keybindings.ts";
import * as Schema from "effect/Schema";

export const PROJECT_SCRIPT_KEYBINDING_INVALID_MESSAGE = "Invalid keybinding.";

const decodeKeybindingRule = Schema.decodeUnknownOption(KeybindingRuleSchema);

function normalizeProjectScriptKeybindingInput(
  keybinding: string | null | undefined,
): string | null {
  const trimmed = keybinding?.trim() ?? "";
  return trimmed.length > 0 ? trimmed : null;
}

export function decodeProjectScriptKeybindingRule(input: {
  keybinding: string | null | undefined;
  command: KeybindingCommand | null;
}): KeybindingRule | null {
  const normalizedKey = normalizeProjectScriptKeybindingInput(input.keybinding);
  if (!normalizedKey) return null;

  if (input.command === null) {
    throw new Error(PROJECT_SCRIPT_KEYBINDING_INVALID_MESSAGE);
  }

  const decoded = decodeKeybindingRule({
    key: normalizedKey,
    command: input.command,
  });
  if (decoded._tag === "None") {
    throw new Error(PROJECT_SCRIPT_KEYBINDING_INVALID_MESSAGE);
  }
  return decoded.value;
}

export function keybindingValueForCommand(
  keybindings: ResolvedKeybindingsConfig,
  command: KeybindingCommand | null,
): string | null {
  if (command === null) return null;
  const bound = keybindings.bindings[command];
  const last = bound?.at(-1);
  if (last !== undefined && last.length > 0) return last;
  return registeredProjectActionChord(command);
}
