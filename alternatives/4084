import React from "react";

import type { BootstrapProject } from "../../contracts/index.ts";
import {
  Menu,
  MenuPopup,
  MenuRadioGroup,
  MenuRadioItem,
  MenuTrigger,
} from "../../components/ui/menu.tsx";
import { Tooltip, TooltipPopup, TooltipTrigger } from "../../components/ui/tooltip.tsx";
import { ProjectGlyph } from "./ProjectGlyph.tsx";

export interface DraftHeroHeadlineProps {
  readonly projects: readonly BootstrapProject[];
  readonly activeProjectId: string | null;
  readonly activeProjectName: string | null;
  readonly onProjectChange: (projectId: string) => void;
}

export function DraftHeroHeadline(props: DraftHeroHeadlineProps): React.ReactElement {
  const activeProjectKey = props.activeProjectId ?? "";
  const activeProjectDisplayName = props.activeProjectName;
  const hasResolvedProject = props.activeProjectName !== null;
  const canChooseProject = props.projects.length > 0;
  const shouldShowProjectMenu = canChooseProject;

  const projectSelector = shouldShowProjectMenu ? (
    <Menu>
      <Tooltip>
        <TooltipTrigger
          render={
            <MenuTrigger
              aria-label={hasResolvedProject ? "Change project" : "Choose a project"}
              className="pointer-events-auto inline-block max-w-64 truncate border-foreground/60 border-b border-dotted align-baseline text-foreground transition-colors hover:border-foreground/80 focus-visible:rounded-sm focus-visible:outline-hidden focus-visible:ring-2 focus-visible:ring-ring"
            />
          }
        >
          {activeProjectDisplayName ?? "Choose a project"}
        </TooltipTrigger>
        {activeProjectDisplayName ? (
          <TooltipPopup side="top" className="max-w-80">
            {activeProjectDisplayName}
          </TooltipPopup>
        ) : null}
      </Tooltip>
      <MenuPopup align="center" className="max-h-80 min-w-40! w-max max-w-64 overflow-y-auto">
        <MenuRadioGroup
          value={activeProjectKey}
          onValueChange={(value) => {
            if (value === activeProjectKey) {
              return;
            }
            props.onProjectChange(value as string);
          }}
        >
          {props.projects.map((project) => {
            return (
              <MenuRadioItem
                key={project.id}
                value={project.id}
                closeOnClick
                className="[&>span:last-child]:flex [&>span:last-child]:min-w-0 [&>span:last-child]:items-center [&>span:last-child]:gap-2"
              >
                <ProjectGlyph
                  className="size-4 shrink-0"
                  projectId={props.activeProjectId}
                  projectName={props.activeProjectName}
                />
                <Tooltip>
                  <TooltipTrigger render={<span className="block min-w-0 truncate" />}>
                    {project.name}
                  </TooltipTrigger>
                  <TooltipPopup side="top" className="max-w-80">
                    {project.name}
                  </TooltipPopup>
                </Tooltip>
              </MenuRadioItem>
            );
          })}
        </MenuRadioGroup>
      </MenuPopup>
    </Menu>
  ) : null;

  return (
    <h1
      className="mx-auto w-full max-w-5xl text-center font-normal text-2xl text-foreground tracking-tight sm:text-3xl"
      data-testid="hero-headline"
    >
      {hasResolvedProject ? (
        <>What should we build in {projectSelector}?</>
      ) : canChooseProject ? (
        <>{projectSelector} to start</>
      ) : (
        <>Add a project to start</>
      )}
    </h1>
  );
}
