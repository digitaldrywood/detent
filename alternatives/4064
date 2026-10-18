import React from "react";

import { Button } from "../../../components/ui/button.tsx";
import { Label } from "../../../components/ui/label.tsx";
import { AttachmentEditor } from "./AttachmentEditor.tsx";

export type VerificationResult = "verified" | "not_fixed";

export function VerificationControls({
  projectId,
  saving,
  onVerify,
}: {
  readonly projectId: string;
  readonly saving: boolean;
  readonly onVerify: (result: VerificationResult, comment?: string) => void;
}): React.ReactElement {
  const [reporting, setReporting] = React.useState(false);
  const [comment, setComment] = React.useState("");
  const [uploading, setUploading] = React.useState(false);
  const missing = comment.trim().length === 0;
  return (
    <div role="group" aria-label="Verify the fix" data-testid="issue-verification" className="mt-3 flex flex-col gap-2">
      {reporting ? (
        <form
          className="flex flex-col gap-1.5"
          onSubmit={(event) => {
            event.preventDefault();
            if (saving || uploading || missing) return;
            onVerify("not_fixed", comment.trim());
          }}
        >
          <Label htmlFor="verification-comment">What is still broken?</Label>
          <AttachmentEditor id="verification-comment" projectId={projectId} value={comment} onChange={setComment}
            onUploadingChange={setUploading} disabled={saving} required />
          <div className="flex gap-2">
            <Button type="submit" size="sm" variant="destructive-outline" data-testid="verification-still-broken-submit"
              disabled={saving || uploading || missing}>
              Still broken
            </Button>
            <Button type="button" size="sm" variant="ghost" disabled={saving} onClick={() => setReporting(false)}>
              Cancel
            </Button>
          </div>
        </form>
      ) : (
        <div className="flex gap-2">
          <Button type="button" size="sm" data-testid="verification-verified" disabled={saving} onClick={() => onVerify("verified")}>
            Verified
          </Button>
          <Button type="button" size="sm" variant="outline" data-testid="verification-still-broken" disabled={saving}
            onClick={() => setReporting(true)}>
            Still broken
          </Button>
        </div>
      )}
    </div>
  );
}
