export const VOICE_MODEL_FAILURE = "The speech model could not load. Check your connection and try again, or type your message.";
export const VOICE_MODEL_DOWNLOADED = "detent.voice-model-downloaded";

export class LocalTranscription {
  private worker: Worker | undefined;
  downloaded = false;
  private model: Promise<void> | undefined;
  private rejectWorker: ((error: Error) => void) | undefined;

  load() {
    this.model ??= new Promise<void>((resolve, reject) => {
      const worker = new Worker(new URL("./voiceTranscription.worker.ts", import.meta.url), { type: "module" });
      this.worker = worker;
      this.rejectWorker = reject;
      worker.onmessage = (event: MessageEvent<{ type: string }>) => {
        if (event.data.type === "ready") {
          this.downloaded = true;
          try { localStorage.setItem(VOICE_MODEL_DOWNLOADED, "true"); } catch {}
          this.rejectWorker = undefined;
          resolve();
        } else {
          this.dispose();
        }
      };
      worker.onerror = () => this.dispose();
      worker.postMessage({});
    });
    return this.model;
  }

  dispose() {
    this.rejectWorker?.(new Error(VOICE_MODEL_FAILURE));
    this.rejectWorker = undefined;
    if (this.worker) {
      this.worker.onmessage = null;
      this.worker.onerror = null;
      this.worker.terminate();
    }
    this.worker = undefined;
    this.model = undefined;
  }

  async transcribe(audio: Float32Array): Promise<string> {
    await this.load();
    return new Promise<string>((resolve, reject) => {
      this.rejectWorker = reject;
      this.worker!.onmessage = (event: MessageEvent<{ type: string; text?: string }>) => {
        if (event.data.type === "result") {
          this.rejectWorker = undefined;
          resolve(event.data.text?.trim() ?? "");
        } else {
          this.dispose();
        }
      };
      this.worker!.postMessage({ audio }, [audio.buffer]);
    });
  }
}
