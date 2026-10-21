// Component contracts and compatibility exports for the conversation client.
export * from "./conversation.ts";
export * from "./ui.ts";
export * as Work from "./work.ts";

export type {
  SidebarProjectGroupingMode,
  SidebarProjectSortOrder,
  SidebarThreadSortOrder,
  TimestampFormat,
} from "../app/adapters/settings.ts";

import * as Schema from "effect/Schema";

export const EnvironmentId = Schema.String;
export type EnvironmentId = string;
export const DesktopSshEnvironmentTargetSchema = Schema.Struct({
  host: Schema.String,
});

/** The single environment this client connects to: the hub it is served from. */
export const HUB_ENVIRONMENT_ID: EnvironmentId = "hub";

export type { ScopedProjectRef, ScopedThreadRef } from "../environment/scoped.ts";
