import type { ProviderDriverKind } from "../../contracts/index.ts";
import { formatProviderDriverKindLabel } from "../../providerModels.ts";
import { PROVIDER_PRESENTATION } from "../../app/usage/usageProviders.ts";

export interface ProviderClientDefinition {
  readonly value: ProviderDriverKind;
  readonly label: string;
}

const PROVIDER_CLIENT_DEFINITIONS: readonly ProviderClientDefinition[] = Object.entries(
  PROVIDER_PRESENTATION,
).map(([value, presentation]) => ({ value, label: presentation.label }));

const PROVIDER_CLIENT_DEFINITION_BY_VALUE: Record<string, ProviderClientDefinition> =
  Object.fromEntries(
    PROVIDER_CLIENT_DEFINITIONS.map((definition) => [definition.value, definition]),
  );

export const DRIVER_OPTIONS = PROVIDER_CLIENT_DEFINITIONS;
export const DRIVER_OPTION_BY_VALUE = PROVIDER_CLIENT_DEFINITION_BY_VALUE;
export type DriverOption = ProviderClientDefinition;

/**
 * Look up the driver metadata for an account's `driver` field. Returns
 * `undefined` for an unknown driver so callers can decide how to render it —
 * typically by falling back to the raw name.
 */
export function getDriverOption(driver: ProviderDriverKind | undefined): DriverOption | undefined {
  if (driver === undefined) return undefined;
  return (
    PROVIDER_CLIENT_DEFINITION_BY_VALUE[driver] ??
    (driver.length === 0
      ? undefined
      : { value: driver, label: formatProviderDriverKindLabel(driver) })
  );
}
