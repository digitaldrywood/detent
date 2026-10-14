import {
  normalizeProviderAccentColor,
  shouldShowInstanceBadge,
} from "@t3tools/client-runtime/state/provider-instance-display";

import type {
  ModelSelection,
  ProviderDriverKind,
  ProviderInstanceId,
  ServerProvider,
} from "./contracts/ui.ts";
import type { ModelEsque } from "./components/chat/providerIconUtils.ts";

export { normalizeProviderAccentColor, shouldShowInstanceBadge };

export const NO_PROVIDER_MODEL_SELECTION: ModelSelection = {
  instanceId: "t3code_no_provider" as ProviderInstanceId,
  model: "",
};

export interface ProviderInstanceEntry {
  readonly instanceId: ProviderInstanceId;
  readonly driverKind: ProviderDriverKind;
  readonly displayName: string;
  readonly accentColor?: string | undefined;
  readonly enabled: boolean;
  readonly installed: boolean;
  readonly isDefault: boolean;
  readonly isAvailable: boolean;
  readonly snapshot: ServerProvider;
  /** The models this instance offers, as the row's hover card labels them. */
  readonly models: readonly ModelEsque[];
}

const NO_ENTRIES: readonly ProviderInstanceEntry[] = [];

export function deriveProviderInstanceEntries(
  _providers: readonly ServerProvider[],
): readonly ProviderInstanceEntry[] {
  return NO_ENTRIES;
}

/**
 * Upstream keys entries by environment because a default instance id is the
 * driver slug, which collides across servers. Detent has one environment and
 * no providers, so every lookup misses and the copied row uses its own
 * `EMPTY_PROVIDER_ENTRIES` path.
 */
export function deriveProviderEntriesByEnvironment(
  providersByEnvironment: Iterable<readonly [string, readonly ServerProvider[]]>,
): ReadonlyMap<string, ReadonlyMap<string, ProviderInstanceEntry>> {
  const byEnvironment = new Map<string, ReadonlyMap<string, ProviderInstanceEntry>>();
  for (const [environmentId] of providersByEnvironment) {
    byEnvironment.set(environmentId, new Map());
  }
  return byEnvironment;
}
