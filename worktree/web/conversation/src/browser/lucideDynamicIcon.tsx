import { Icon, FolderCodeIcon, type IconNode, type LucideProps } from "lucide-react";
import dynamicIconImports from "lucide-react/dynamicIconImports";
import { useEffect, useState, type ComponentType } from "react";

export type IconName = keyof typeof dynamicIconImports;
export const iconNames = Object.keys(dynamicIconImports) as IconName[];

export function DynamicIcon({ name, fallback: Fallback = FolderCodeIcon, ...props }: LucideProps & { name: IconName; fallback?: ComponentType }) {
  const [loaded, setLoaded] = useState<{ name: IconName; node: IconNode } | null>(null);
  useEffect(() => {
    let current = true;
    const load = dynamicIconImports[name];
    if (load) void load().then((module) => {
      if (current) setLoaded({ name, node: module.__iconNode });
    }).catch(() => { if (current) setLoaded(null); });
    return () => { current = false; };
  }, [name]);
  return loaded?.name === name ? <Icon {...props} iconNode={loaded.node} /> : <Fallback />;
}
