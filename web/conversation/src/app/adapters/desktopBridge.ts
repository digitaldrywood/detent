declare global {
  interface Window {
    readonly desktopBridge?: {
      readonly preview?: {
        setAudioMuted(tabId: string, muted: boolean): Promise<void>;
      };
      readonly getWindowFullscreenState?: () => boolean;
      readonly onWindowFullscreenStateChange?: (listener: (value: boolean) => void) => () => void;
      readonly onMenuAction?: (listener: (action: string) => void) => (() => void) | undefined;
      readonly contextMenu?: {
        show(items: readonly unknown[], at: { x: number; y: number }): Promise<string | null>;
      };
    };
  }
}

export {};
