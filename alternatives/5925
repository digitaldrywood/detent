import { createContext, useContext, useEffect, useId } from "react";

export const ProjectSettingsDirtyContext = createContext<((id: string, dirty: boolean) => void) | null>(null);

export function useProjectSettingsDirty(dirty: boolean): void {
  const register = useContext(ProjectSettingsDirtyContext);
  const id = useId();
  useEffect(() => {
    register?.(id, dirty);
    return () => register?.(id, false);
  }, [dirty, id, register]);
}
