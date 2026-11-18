import React from "react";

import type { MembersResponse } from "../../contracts/account.ts";
import { useAccountApi } from "./context.ts";
import { useResource, type Resource } from "./useResource.ts";

const OrganizationMembersContext =
  React.createContext<Resource<MembersResponse> | null>(null);

export function OrganizationMembersProvider({
  children,
  enabled = true,
}: {
  readonly children: React.ReactNode;
  readonly enabled?: boolean;
}): React.ReactElement {
  const api = useAccountApi();
  const members = useResource<MembersResponse>(
    () =>
      enabled
        ? api.members()
        : Promise.resolve({ members: [], invitations: [] }),
    [api, enabled],
  );
  return (
    <OrganizationMembersContext.Provider value={members}>
      {children}
    </OrganizationMembersContext.Provider>
  );
}

export function useOrganizationMembers(): Resource<MembersResponse> {
  const members = React.useContext(OrganizationMembersContext);
  if (members === null)
    throw new Error("Organization members are not mounted.");
  return members;
}
