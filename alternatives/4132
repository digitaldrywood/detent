import React from "react";
import { UserProfile, UserSecurity, WorkOsWidgets } from "@workos-inc/widgets";
import { ShieldIcon, UserIcon } from "lucide-react";

import { useAccountApi } from "../account/context.ts";
import { useTheme } from "../adapters/theme.ts";
import { SettingsSection } from "./settingsLayout.tsx";
import { ControlError } from "../account/controls.tsx";

export function AccountWidgets(): React.ReactElement {
  const api = useAccountApi();
  const [failed, setFailed] = React.useState(false);
  const { resolvedTheme } = useTheme();
  const authToken = React.useCallback(async () => {
    try {
      return (await api.widgetToken()).token;
    } catch {
      setFailed(true);
      throw new Error("Account settings are temporarily unavailable");
    }
  }, [api]);
  const widgets = [
    { id: "profile", title: "Profile", icon: UserIcon, Widget: UserProfile },
    { id: "security", title: "Security", icon: ShieldIcon, Widget: UserSecurity },
  ] as const;
  return <WorkOsWidgets className="account-widgets" theme={{ appearance: resolvedTheme, radius: "medium", fontFamily: "var(--font-sans)" }}>
    <div className="space-y-6">
      {widgets.map(({ id, title, icon: Icon, Widget }) => <SettingsSection key={id} id={`settings-${id}`} title={title} icon={<Icon className="size-3.5" />}>
        <div className="p-3 sm:p-4">
          {failed ? <ControlError message={`${title} settings are temporarily unavailable. Please try again.`} />
            : <Widget authToken={authToken} />}
        </div>
      </SettingsSection>)}
    </div>
  </WorkOsWidgets>;
}
