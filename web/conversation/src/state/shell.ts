export const shellEnvironment = {
  /** Open a workspace path in the reader's editor. Never available here. */
  openInEditor: "shell.openInEditor",
} as const;

export type ShellCommand = (typeof shellEnvironment)[keyof typeof shellEnvironment];
