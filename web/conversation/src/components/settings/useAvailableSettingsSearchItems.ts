import { useMemo } from "react";

import { useAccountBootstrap } from "../../app/account/context.ts";
import {
  isSettingsSectionId,
  settingsNavItems,
  type SettingsNavItem,
  type SettingsSectionId,
} from "../../app/settings/sections.tsx";
import {
  settingsSearchItems,
  settingsSectionPath,
  type SettingsPath,
  type SettingsSearchItem,
} from "./settingsSearch.ts";

export interface SettingsNavEntry extends SettingsNavItem {
  readonly to: SettingsPath;
}

function useActor(): { readonly canManage: boolean; readonly supporting: boolean; readonly selfHosted: boolean } {
  const bootstrap = useAccountBootstrap();
  return {
    canManage: bootstrap?.actor.can_manage ?? false,
    supporting: bootstrap?.support != null,
    selfHosted: bootstrap?.self_hosted ?? false,
  };
}

export function useSettingsNavItems(): readonly SettingsNavEntry[] {
  const { canManage, supporting, selfHosted } = useActor();
  return useMemo(
    () =>
      settingsNavItems({ canManage, supporting, selfHosted }).map((item) => ({
        ...item,
        to: (item.to ?? settingsSectionPath(item.id as SettingsSectionId)) as SettingsPath,
      })),
    [canManage, supporting, selfHosted],
  );
}

export function useAvailableSettingsSearchItems(): readonly SettingsSearchItem[] {
  const { canManage, supporting, selfHosted } = useActor();
  return useMemo(() => {
    const reachable = settingsNavItems({ canManage, supporting, selfHosted })
      .filter((item) => item.disabled !== true && isSettingsSectionId(item.id))
      .map((item) => item.id as SettingsSectionId);
    return settingsSearchItems(reachable).filter((item) => !selfHosted || item.targetId !== "settings-general-plan");
  }, [canManage, supporting, selfHosted]);
}
