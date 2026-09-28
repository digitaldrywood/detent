import {
  ActivityIcon,
  BuildingIcon,
  CreditCardIcon,
  GaugeIcon,
  LifeBuoyIcon,
  ScrollTextIcon,
  ServerIcon,
  SettingsIcon,
  TicketIcon,
  UsersIcon,
  type LucideIcon,
} from "lucide-react";

export type PlatformPageId =
  | "overview"
  | "organizations"
  | "users"
  | "runners"
  | "provisioning"
  | "plans"
  | "billing"
  | "support"
  | "audit"
  | "settings";

export interface PlatformNavItem {
  readonly id: PlatformPageId;
  readonly to: string;
  readonly label: string;
  readonly icon: LucideIcon;
  readonly group: "operate" | "commercial" | "trust" | "configure";
}

export const PLATFORM_ROOT = "/platform";

export const PLATFORM_NAV: readonly PlatformNavItem[] = [
  { id: "overview", to: "/platform", label: "Overview", icon: GaugeIcon, group: "operate" },
  { id: "organizations", to: "/platform/organizations", label: "Organizations", icon: BuildingIcon, group: "operate" },
  { id: "users", to: "/platform/users", label: "Users", icon: UsersIcon, group: "operate" },
  { id: "runners", to: "/platform/runners", label: "Runners & capacity", icon: ServerIcon, group: "operate" },
  { id: "provisioning", to: "/platform/provisioning", label: "Provisioning & health", icon: ActivityIcon, group: "operate" },
  { id: "plans", to: "/platform/plans", label: "Plans & grants", icon: TicketIcon, group: "commercial" },
  { id: "billing", to: "/platform/billing", label: "Billing", icon: CreditCardIcon, group: "commercial" },
  { id: "support", to: "/platform/support", label: "Support sessions", icon: LifeBuoyIcon, group: "trust" },
  { id: "audit", to: "/platform/audit", label: "Audit log", icon: ScrollTextIcon, group: "trust" },
  { id: "settings", to: "/platform/settings", label: "Settings", icon: SettingsIcon, group: "configure" },
];

export const PLATFORM_NAV_GROUPS: readonly { readonly id: PlatformNavItem["group"]; readonly label: string }[] = [
  { id: "operate", label: "Operate" },
  { id: "commercial", label: "Commercial" },
  { id: "trust", label: "Trust" },
  { id: "configure", label: "Configure" },
];

/** The nav item a pathname belongs to; detail pages light up their list. */
export function activePlatformPage(pathname: string): PlatformPageId {
  const path = pathname.replace(/\/+$/, "") || "/";
  let best: PlatformNavItem | undefined;
  for (const item of PLATFORM_NAV) {
    const matches = item.to === PLATFORM_ROOT ? path === PLATFORM_ROOT : path === item.to || path.startsWith(`${item.to}/`);
    if (matches && (best === undefined || item.to.length > best.to.length)) best = item;
  }
  return best?.id ?? "overview";
}
