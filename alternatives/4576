import * as Schema from "effect/Schema";

export const ProjectIcon = Schema.Union([
  Schema.Struct({ kind: Schema.Literal("emoji"), emoji: Schema.String }),
  Schema.Struct({ kind: Schema.Literal("lucide"), name: Schema.String, color: Schema.Literals(["gray", "red", "orange", "amber", "yellow", "lime", "green", "emerald", "teal", "cyan", "sky", "blue", "indigo", "violet", "purple", "fuchsia", "pink", "rose"]) }),
  Schema.Struct({ kind: Schema.Literal("image"), url: Schema.String, image_key: Schema.optional(Schema.String), image_sha256: Schema.optional(Schema.String) }),
]);

export type ProjectIcon = typeof ProjectIcon.Type;
export type ProjectIconChoice = Exclude<ProjectIcon, { kind: "image" }> | { readonly kind: "image" } | { readonly kind: "automatic" };
