import { HUB_ENVIRONMENT_ID } from "../contracts/index.ts";
import type { EnvironmentId } from "../contracts/index.ts";
import { DETENT_SERVER_CONFIG, type ServerConfig } from "./server.ts";

export interface EnvironmentPresentation {
  readonly environmentId: EnvironmentId;
  readonly label: string;
  readonly displayUrl: string | null;
  readonly relayManaged: boolean;
  readonly serverConfig: ServerConfig | null;
}

const HUB_PRESENTATION: EnvironmentPresentation = {
  environmentId: HUB_ENVIRONMENT_ID,

  label: "Detent Cloud",
  displayUrl: null,
  relayManaged: false,
  serverConfig: DETENT_SERVER_CONFIG,
};

const ENVIRONMENTS: readonly EnvironmentPresentation[] = [HUB_PRESENTATION];

const PRESENTATION_BY_ID: ReadonlyMap<EnvironmentId, EnvironmentPresentation> = new Map([
  [HUB_ENVIRONMENT_ID, HUB_PRESENTATION],
]);

export function useEnvironments(): {
  readonly isReady: boolean;
  readonly networkStatus: "online" | "offline";
  readonly environments: readonly EnvironmentPresentation[];
  readonly presentationById: ReadonlyMap<EnvironmentId, EnvironmentPresentation>;
} {
  return {
    isReady: true,
    networkStatus: "online",
    environments: ENVIRONMENTS,
    presentationById: PRESENTATION_BY_ID,
  };
}

export function usePrimaryEnvironmentId(): EnvironmentId | null {
  return HUB_ENVIRONMENT_ID;
}

export function useEnvironment(
  environmentId: EnvironmentId | null,
): EnvironmentPresentation | null {
  return environmentId === null ? null : (PRESENTATION_BY_ID.get(environmentId) ?? null);
}

export function usePrimaryEnvironment(): EnvironmentPresentation | null {
  return HUB_PRESENTATION;
}
