export type RemoteOpenMode = "local-exec" | "remote-links" | "remote-unavailable";

/**
 * Upstream this is a union carrying the SSH alias or advertised hostname a
 * deep link would be built from. There is no such host here — a Detent turn
 * runs on the customer's own runner, reachable only outbound through the hub's
 * relay (decisions.md §1, §18) — so the mode is all there is to say.
 */
export interface RemoteOpenState {
  readonly mode: RemoteOpenMode;
}

export interface RemoteOpenResolution {
  readonly state: RemoteOpenState;
  readonly isResolved: boolean;
}

const REMOTE_LINKS: RemoteOpenResolution = {
  state: { mode: "remote-links" },
  isResolved: true,
};

export function useRemoteOpenResolution(_environmentId: string | null): RemoteOpenResolution {
  return REMOTE_LINKS;
}

export function useRemoteOpenState(environmentId: string | null): RemoteOpenState {
  return useRemoteOpenResolution(environmentId).state;
}
