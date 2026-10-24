import React from "react";
import type { FleetRunner } from "../../contracts/account.ts";
import { Button } from "../../components/ui/button.tsx";
import { Dialog, DialogDescription, DialogFooter, DialogHeader, DialogPanel, DialogPopup, DialogTitle } from "../../components/ui/dialog.tsx";

export function RunnerRemoval({ runner, onRemove, onClose }: {
  readonly runner: FleetRunner;
  readonly onRemove: (runner: FleetRunner) => Promise<void>;
  readonly onClose: () => void;
}): React.ReactElement {
  const [removing, setRemoving] = React.useState(false);
  const [error, setError] = React.useState("");
  async function remove(): Promise<void> {
    if (removing) return;
    setRemoving(true);
    setError("");
    try {
      await onRemove(runner);
      onClose();
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not remove the runner.");
    } finally {
      setRemoving(false);
    }
  }
  return <Dialog open onOpenChange={(open) => { if (!open && !removing) onClose(); }}>
    <DialogPopup>
      <DialogHeader>
        <DialogTitle>Remove runner {runner.display_name}?</DialogTitle>
        <DialogDescription>This revokes its credentials and removes it from the fleet. Historical runs keep the runner's name. Runners with active work must finish their attempts before removal.</DialogDescription>
      </DialogHeader>
      {error ? <DialogPanel><p role="alert" className="text-sm text-destructive">{error}</p></DialogPanel> : null}
      <DialogFooter>
        <Button variant="outline" disabled={removing} onClick={onClose}>Cancel</Button>
        <Button variant="destructive" disabled={removing} onClick={() => void remove()}>{removing ? "Removing…" : "Remove runner"}</Button>
      </DialogFooter>
    </DialogPopup>
  </Dialog>;
}
