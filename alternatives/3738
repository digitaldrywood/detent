import { BotIcon } from "lucide-react";

import { PROVIDER_ICON_BY_PROVIDER } from "../../components/chat/providerIconUtils.ts";
import type { Icon } from "../../components/Icons.tsx";
import { ProviderDriverKind } from "../../contracts/ui.ts";

const DRIVER_BY_PROVIDER: Readonly<Record<string, string>> = {
  openai: "codex",
  codex: "codex",
  anthropic: "claudeAgent",
  claude: "claudeAgent",
  claudeagent: "claudeAgent",
  "claude-code": "claudeAgent",
  claudecode: "claudeAgent",
  cursor: "cursor",
  grok: "grok",
  xai: "grok",
  opencode: "opencode",
  antigravity: "antigravity",
};

/** The vendor names Detent spells for itself; everything else is title-cased. */
const PROVIDER_NAMES: Readonly<Record<string, string>> = {
  openai: "OpenAI",
  anthropic: "Anthropic",
  google: "Google",
  xai: "xAI",
};

/** What a provider is called, for a label, a tooltip or a row. */
export function providerName(provider: string): string {
  const key = provider.trim().toLowerCase();
  const known = PROVIDER_NAMES[key];
  if (known !== undefined) return known;
  return key.length === 0 ? provider : key.charAt(0).toUpperCase() + key.slice(1);
}

export function providerMark(provider: string): { readonly icon: Icon; readonly branded: boolean } {
  const driver = DRIVER_BY_PROVIDER[provider.trim().toLowerCase()];
  const mark = driver === undefined ? undefined : PROVIDER_ICON_BY_PROVIDER[ProviderDriverKind.make(driver)];
  return mark === undefined ? { icon: BotIcon as Icon, branded: false } : { icon: mark, branded: true };
}
