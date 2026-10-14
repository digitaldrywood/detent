import { describe, expect, it } from "vitest";

import { classifyMarkdownImageSource } from "../src/runtime/markdownImages.ts";

describe("Cloud attachment image sources", () => {
  const attachment = "/organizations/org_alpha/api/v2/projects/prj_example/attachments/att_0123456789abcdef0123456789abcdef";

  it.each([
    [attachment, "Direct"],
    ["attachment:att_0123456789abcdef0123456789abcdef", "Attachment"],
    ["attachment:../../secret.png", "Blocked"],
    [attachment.replace("att_", "local_"), "WorkspaceFile"],
    [attachment + "/../../secret.png", "WorkspaceFile"],
    ["/etc/secret.png", "WorkspaceFile"],
    [".detent/validation/test.png", "Blocked"],
  ])("classifies %s as %s without a checkout", (source, tag) => {
    expect(classifyMarkdownImageSource(source)._tag).toBe(tag);
  });
});
