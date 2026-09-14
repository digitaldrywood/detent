import { FolderCodeIcon } from "lucide-react";
import type { ComponentPropsWithoutRef } from "react";

export function DynamicIcon({
  name: _name,
  ...props
}: ComponentPropsWithoutRef<typeof FolderCodeIcon> & { name: string }) {
  return <FolderCodeIcon {...props} />;
}
