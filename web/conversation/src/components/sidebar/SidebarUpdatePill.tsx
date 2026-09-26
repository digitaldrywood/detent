import { memo, useCallback, useEffect, useState, useSyncExternalStore } from "react";

import { useSidebarData } from "../../app/adapters/sidebarData.tsx";
import {
  checkForUpdates,
  readUpdateCheckState,
  startUpdateWatch,
  subscribeToUpdateCheck,
} from "../../app/adapters/detentUpdates.ts";
import {
  RUNNER_SETTINGS_PATH,
  getUpdateButtonTooltip,
  isUpdateButtonDisabled,
  resolveUpdateButtonAction,
  showsUpdateDetail,
  updateIconStatus,
} from "../../app/lib/detentUpdates.ts";
import { useMediaQuery } from "../../hooks/useMediaQuery.ts";
import { cn } from "../../lib/utils.ts";
import { SidebarMenuItem } from "../ui/sidebar.tsx";
import { stackedThreadToast, toastManager } from "../ui/toast.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../ui/tooltip.tsx";
import {
  DesktopUpdateStatusIcon,
  shouldContinueDesktopUpdateCheckAnimation,
  shouldShowDesktopUpdateCheckIcon,
} from "./DesktopUpdateStatusIcon.tsx";

export const SidebarUpdatePill = memo(function SidebarUpdatePill() {
  const state = useSyncExternalStore(subscribeToUpdateCheck, readUpdateCheckState);
  const navigate = useSidebarData()?.navigation?.onNavigate;
  const [checkAnimationKey, setCheckAnimationKey] = useState(0);
  const [isCheckAnimationLatched, setIsCheckAnimationLatched] = useState(false);
  const prefersReducedMotion = useMediaQuery("(prefers-reduced-motion: reduce)");

  useEffect(() => startUpdateWatch(), []);

  useEffect(() => {
    if (prefersReducedMotion) {
      setIsCheckAnimationLatched(false);
    } else if (state.status === "checking") {
      setIsCheckAnimationLatched(true);
    }
  }, [prefersReducedMotion, state.status]);

  const action = resolveUpdateButtonAction(state);
  const showCheckIcon = shouldShowDesktopUpdateCheckIcon({
    isAnimationLatched: isCheckAnimationLatched,
    isChecking: state.status === "checking",
    prefersReducedMotion,
  });
  const showUpdateDetails = showsUpdateDetail(state);
  const showUpdateIconState = showUpdateDetails && !showCheckIcon;
  const tooltip = getUpdateButtonTooltip(state);
  const disabled = isUpdateButtonDisabled(state);
  const iconStatus = updateIconStatus(state);

  const handleAction = useCallback(() => {
    if (action === "none") return;
    if (action === "show") {
      // Nothing about a runner's binary can be changed from here. The screen
      // that names the hosts and the command is the whole of what a press can
      // usefully do.
      if (navigate === undefined) globalThis.location?.assign(RUNNER_SETTINGS_PATH);
      else navigate(RUNNER_SETTINGS_PATH);
      return;
    }
    if (!prefersReducedMotion) {
      setIsCheckAnimationLatched(true);
      setCheckAnimationKey((key) => key + 1);
    }
    void checkForUpdates().then(() => {
      const settled = readUpdateCheckState();
      if (settled.status === "error") {
        toastManager.add(
          stackedThreadToast({
            type: "error",
            title: "Could not check for updates",
            description: settled.message ?? "The update check failed.",
          }),
        );
        return;
      }

      if (settled.status === "idle") {
        toastManager.add(stackedThreadToast({ type: "success", title: "All runners up to date" }));
      }
    });
  }, [action, navigate, prefersReducedMotion]);

  const handleCheckAnimationIteration = useCallback(() => {
    setIsCheckAnimationLatched(
      shouldContinueDesktopUpdateCheckAnimation({
        isChecking: state.status === "checking",
        prefersReducedMotion,
      }),
    );
  }, [prefersReducedMotion, state.status]);

  return (
    <SidebarMenuItem className="ml-auto shrink-0">
      <Tooltip>
        <TooltipTrigger
          render={
            <button
              type="button"
              aria-label={tooltip}
              aria-disabled={disabled || undefined}
              className={cn(
                "inline-flex size-8 items-center justify-center rounded-full outline-hidden ring-ring transition-colors focus-visible:ring-2",
                disabled ? "cursor-not-allowed" : "cursor-pointer",
                showUpdateIconState
                  ? cn(
                      "bg-sidebar-control-surface text-sidebar-foreground",
                      !disabled && "hover:bg-sidebar-row-hover",
                    )
                  : cn(
                      "text-[var(--sidebar-icon-color)]",
                      !disabled && "hover:bg-sidebar-row-hover hover:text-sidebar-foreground",
                    ),
                disabled && !showUpdateIconState && "opacity-60",
              )}
              onClick={handleAction}
            >
              <DesktopUpdateStatusIcon
                key={showCheckIcon ? checkAnimationKey : iconStatus}
                downloadPercent={null}
                isCheckAnimating={showCheckIcon && !prefersReducedMotion}
                onCheckAnimationIteration={handleCheckAnimationIteration}
                status={showCheckIcon ? "checking" : iconStatus}
              />
            </button>
          }
        />
        <TooltipPopup align="center" side="top" variant={showUpdateDetails ? "glass" : "default"}>
          {tooltip}
        </TooltipPopup>
      </Tooltip>
    </SidebarMenuItem>
  );
});

export const SidebarUpdateArchitectureWarning = memo(
  function SidebarUpdateArchitectureWarning() {
    return null;
  },
);
