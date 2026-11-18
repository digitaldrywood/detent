import React from "react";
import { Button } from "../../components/ui/button.tsx";
import { Dialog, DialogPopup, DialogHeader, DialogTitle, DialogDescription, DialogPanel, DialogFooter } from "../../components/ui/dialog.tsx";
import { GitHubIssueIntake } from "../account/IssueIntake.tsx";
import { ControlError, NativeSelect } from "../account/controls.tsx";
import { useAccountApi } from "../account/context.ts";
import { useResource } from "../account/useResource.ts";

const sources = [{ value: "github", label: "GitHub" }];

export function ImportIssuesDialog({ projectId, onClose }: {
  readonly projectId: string;
  readonly onClose: () => void;
}): React.ReactElement {
  const api = useAccountApi();
  const [source, setSource] = React.useState("github");
  const context = useResource(async () => {
    const [integration, onboarding] = await Promise.all([api.integration(projectId), api.onboarding(projectId)]);
    return { integration, onboarding };
  }, [api, projectId]);
  const repository = context.value?.integration.checkout_repository || context.value?.integration.repository;
  return <Dialog open onOpenChange={open => { if (!open) onClose(); }}>
    <DialogPopup className="sm:max-w-2xl">
      <DialogHeader>
        <DialogTitle>Import issues</DialogTitle>
        <DialogDescription>Bring existing work into this project's Cloud lanes.</DialogDescription>
      </DialogHeader>
      <DialogPanel>
        <label className="mb-3 block text-sm">Source
          <NativeSelect aria-label="Import source" value={source} onValueChange={setSource} options={sources} />
        </label>
        {context.loading ? <p role="status">Loading import options…</p> : null}
        <ControlError message={context.error?.message ?? null} />
        {context.error ? <Button variant="outline" onClick={() => void context.refresh()}>Retry import options</Button> : null}
        {repository && source === "github" ? <GitHubIssueIntake projectId={projectId} repository={repository} runners={(context.value?.onboarding.runners ?? []).map(entry => ({ id: entry.runner.runner_id, name: entry.runner.display_name }))} /> : null}
      </DialogPanel>
      <DialogFooter><Button variant="outline" onClick={onClose}>Done</Button></DialogFooter>
    </DialogPopup>
  </Dialog>;
}
