import type { AccessKey, OAuthGrant } from "../../contracts/accessKeys.ts";
import type { AccountApi } from "../account/api.ts";

export const MCP_URL = "https://cloud.detent.build/mcp";

export function keyInWorkspace(key: AccessKey, organization: string): boolean {
  return (
    !key.revoked_at &&
    (!key.expires_at || Date.parse(key.expires_at) > Date.now()) &&
    (key.access_context === "global" ||
      key.organizations?.some(
        (entry) => entry.organization_id === organization,
      ) === true)
  );
}

export async function readConnections(api: AccountApi, organization: string) {
  const [keys, grants] = await Promise.all([
    (async () => {
      const result: AccessKey[] = [];
      let cursor: string | undefined;
      do {
        const page = await api.personalKeys(cursor);
        result.push(...page.keys);
        cursor = page.next_cursor;
      } while (cursor);
      return result.filter((key) => keyInWorkspace(key, organization));
    })(),
    (async () => {
      const result: OAuthGrant[] = [];
      let cursor: string | undefined;
      do {
        const page = await api.oauthGrants(cursor);
        result.push(...page.grants);
        cursor = page.next_cursor;
      } while (cursor);
      return result.filter(
        (grant) => grant.organization_id === organization && !grant.revoked_at,
      );
    })(),
  ]);
  return { keys, grants };
}

export function landingPath(
  role: string,
  connections: { keys: readonly AccessKey[]; grants: readonly OAuthGrant[] },
) {
  return role === "reporter"
    ? connections.keys.length || connections.grants.length
      ? "/my-issues"
      : "/connect-ai"
    : "/work";
}

export function reporterInstructions(token: string): string {
  return `Set up Detent for me.
1. Save this key as the environment variable DETENT_API_KEY: ${token}
   Keep it out of files you commit and out of chat logs.
2. Add an MCP server named detent: Streamable HTTP,
   URL ${MCP_URL},
   header Authorization: Bearer $DETENT_API_KEY.
3. Call connection_info and tell me which workspace you reached.
When I describe a problem or a request, file it in Detent with file_issue.`;
}
