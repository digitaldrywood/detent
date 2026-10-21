import type { MicVAD } from "@ricky0123/vad-web";

import { LocalTranscription } from "./localTranscription.ts";

const wasmSimdProbe = new Uint8Array([0, 97, 115, 109, 1, 0, 0, 0, 1, 4, 1, 96, 0, 0, 3, 2, 1, 0, 10, 9, 1, 7, 0, 65, 0, 253, 15, 26, 11]);

export function supportsHandsFreeInput(): boolean {
  return typeof window !== "undefined" && window.isSecureContext &&
    typeof navigator.mediaDevices?.getUserMedia === "function" &&
    typeof AudioContext === "function" && typeof AudioWorkletNode === "function" &&
    typeof Worker === "function" && typeof WebAssembly === "object" &&
    typeof WebAssembly.validate === "function" && WebAssembly.validate(wasmSimdProbe);
}

export class HandsFreeInput {
  private active = true;
  private busy = false;
  private stream: MediaStream | undefined;
  private context: AudioContext | undefined;
  private vad: MicVAD | undefined;
  private readonly transcription = new LocalTranscription();

  constructor(private readonly callbacks: {
    onReady: () => void;
    onSpeechStart: () => void;
    onTranscribing: () => void;
    onTranscript: (text: string) => Promise<void> | void;
    onError: (message: string) => void;
  }) {}

  async start(): Promise<void> {
    try {
      const context = new AudioContext();
      this.context = context;
      const streamPromise = navigator.mediaDevices.getUserMedia({ audio: {
        channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl: true,
      } }).then((stream) => {
        if (!this.active) stream.getTracks().forEach((track) => track.stop());
        else {
          this.stream = stream;
          stream.getAudioTracks().forEach((track) => { track.enabled = false; });
        }
        return stream;
      });
      const [stream] = await Promise.all([streamPromise, context.resume()]);
      if (!this.active) return;
      const [vadModule] = await Promise.all([import("./voiceActivity.ts"), this.transcription.load()]);
      if (!this.active) return;
      const vad = await vadModule.createVoiceActivity(context, stream, {
        onSpeechRealStart: () => {
          if (this.active && !this.busy) this.callbacks.onSpeechStart();
        },
        onSpeechEnd: (audio) => { void this.transcribe(audio); },
      });
      if (!this.active) {
        await vad.destroy();
        return;
      }
      this.vad = vad;
      this.callbacks.onReady();
    } catch (error) {
      if (!this.active) return;
      this.dispose();
      this.callbacks.onError(error instanceof DOMException && error.name === "NotAllowedError"
        ? "Microphone access is blocked. Allow microphone access in your browser settings, or type your message."
        : "Hands-free voice could not start. Check your microphone and model support, or type your message.");
    }
  }

  private async transcribe(audio: Float32Array) {
    if (!this.active || this.busy) return;
    this.busy = true;
    this.callbacks.onTranscribing();
    try {
      const text = await this.transcription.transcribe(audio);
      if (!this.active) return;
      if (text) await this.callbacks.onTranscript(text);
      else this.callbacks.onReady();
      this.busy = false;
    } catch {
      if (!this.active) return;
      this.dispose();
      this.callbacks.onError("Transcription failed. Start hands-free again, or type your message.");
    }
  }

  dispose(): void {
    if (!this.active) return;
    this.active = false;
    this.stream?.getTracks().forEach((track) => track.stop());
    this.transcription.dispose();
    void this.vad?.destroy().catch(() => {});
    if (this.context && this.context.state !== "closed") void this.context.close();
  }
}
