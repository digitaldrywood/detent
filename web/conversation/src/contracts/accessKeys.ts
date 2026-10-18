import * as Schema from "effect/Schema";
import * as SchemaTransformation from "effect/SchemaTransformation";

export const KeyOrganization = Schema.Struct({
  organization_id: Schema.String,
  project_access: Schema.Literals(["all", "selected", "project"]),
  project_ids: Schema.NullOr(Schema.Array(Schema.String)).pipe(
    Schema.decodeTo(
      Schema.Array(Schema.String),
      SchemaTransformation.transform({
        decode: (projects) => projects ?? [],
        encode: (projects) => projects,
      }),
    ),
  ),
});
export type KeyOrganization = typeof KeyOrganization.Type;
export const KeyReach = Schema.Struct({
  organization_id: Schema.String,
  name: Schema.String,
  role: Schema.String,
  blocked: Schema.optional(Schema.Boolean),
  status: Schema.Literals(["allowed", "pending", "blocked", "inactive"]),
  projects: Schema.Array(
    Schema.Struct({
      id: Schema.String,
      name: Schema.String,
      can_write: Schema.Boolean,
    }),
  ),
  last_used_at: Schema.NullOr(Schema.String),
});
export type KeyReach = typeof KeyReach.Type;
export const AccessKey = Schema.Struct({
  id: Schema.String,
  name: Schema.String,
  kind: Schema.Literals(["personal", "service"]),
  owner: Schema.String,
  owner_email: Schema.String,
  permission: Schema.Literals(["read", "write", "admin"]),
  access_context: Schema.Literals(["global", "selected", "project"]),
  organizations: Schema.NullOr(Schema.Array(KeyOrganization)),
  created_at: Schema.String,
  expires_at: Schema.optional(Schema.String),
  revoked_at: Schema.optional(Schema.String),
  effective_reach: Schema.optional(Schema.Array(KeyReach)),
  last_used_at: Schema.optional(Schema.NullOr(Schema.String)),
});
export type AccessKey = typeof AccessKey.Type;
export const OAuthGrant = Schema.Struct({
  id: Schema.String,
  client_id: Schema.String,
  client_name: Schema.String,
  organization_id: Schema.String,
  connected_at: Schema.String,
  last_used_at: Schema.NullOr(Schema.String),
  revoked_at: Schema.NullOr(Schema.String),
});
export type OAuthGrant = typeof OAuthGrant.Type;
export const OAuthGrants = Schema.Struct({ grants: Schema.Array(OAuthGrant), next_cursor: Schema.optional(Schema.String) });
export const CreatedAccessKey = Schema.Struct({
  ...AccessKey.fields,
  token: Schema.String,
});
export type CreatedAccessKey = typeof CreatedAccessKey.Type;
export const AccessKeys = Schema.Struct({ keys: Schema.Array(AccessKey), next_cursor: Schema.optional(Schema.String) });
export const KeyContext = Schema.Struct({
  organizations: Schema.Array(KeyReach),
  mcp_endpoint: Schema.String,
});
export type KeyContext = typeof KeyContext.Type;
export const PersonalKeyPolicy = Schema.Literals([
  "allowed",
  "approval",
  "blocked",
]);
export type PersonalKeyPolicy = typeof PersonalKeyPolicy.Type;
export const KeyPolicy = Schema.Struct({ personal_keys: PersonalKeyPolicy });
export interface CreateAccessKey {
  name: string;
  permission: "read" | "write" | "admin";
  access_context: "global" | "selected" | "project";
  organizations: readonly KeyOrganization[];
  expires_days: number;
  never_expires: boolean;
}
