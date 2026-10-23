import type { ReactNode } from "react";
import { WorkspacePageHeader } from "../../components/WorkspacePageHeader.tsx";
import { WorkspacePageContainer } from "../../components/WorkspacePageContainer.tsx";

export function ReporterPage({
  title,
  parent,
  actions,
  children,
  wide = false,
}: {
  title: string;
  parent?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  wide?: boolean;
}) {
  return (
    <div className="flex min-h-0 min-w-0 flex-1 flex-col">
      <WorkspacePageHeader className="border-b border-border">
        <div className="flex min-w-0 flex-1 items-center gap-2 text-sm">
          {parent ? (
            <>
              <span className="hidden truncate text-muted-foreground sm:block">
                {parent}
              </span>
              <span
                aria-hidden
                className="hidden text-muted-foreground sm:block"
              >
                /
              </span>
            </>
          ) : null}
          <span className="truncate font-medium">{title}</span>
        </div>
        {actions}
      </WorkspacePageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <WorkspacePageContainer
          width={wide ? "expanded" : "readable"}
          className={wide ? "max-w-none px-3 sm:px-5" : undefined}
        >
          {children}
        </WorkspacePageContainer>
      </div>
    </div>
  );
}
