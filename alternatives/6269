import * as Schema from "effect/Schema";

export const WorkAttachment = Schema.Struct({
  id: Schema.String,
  project_id: Schema.String,
  name: Schema.String,
  content_type: Schema.String,
  size: Schema.Number,
  width: Schema.Number,
  height: Schema.Number,
});

export type WorkAttachment = typeof WorkAttachment.Type;

export function attachmentMarkdown(file: WorkAttachment): string {
  const name = file.name.replace(/[\\\[\]]/g, "\\$&").replace(/[\r\n]/g, " ");
  return `${file.content_type.startsWith("image/") ? "!" : ""}[${name}](attachment:${file.id})`;
}
