import * as Schema from "effect/Schema";

export const KeyOrganization = Schema.Struct({
  organization_id: Schema.String,
  project_access: Schema.Literals(["all", "selected", "project"]),
  project_ids: Schema.Array(Schema.String),
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
