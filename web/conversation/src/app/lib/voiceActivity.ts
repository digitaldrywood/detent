import { MicVAD, type RealTimeVADOptions } from "@ricky0123/vad-web";
import modelUrl from "@ricky0123/vad-web/dist/silero_vad_v5.onnx?url";
import workletUrl from "@ricky0123/vad-web/dist/vad.worklet.bundle.min.js?url";
import runtimeModule from "onnxruntime-web/ort-wasm-simd-threaded.mjs?url";
import runtimeWasm from "onnxruntime-web/ort-wasm-simd-threaded.wasm?url";

export async function createVoiceActivity(
  context: AudioContext,
  stream: MediaStream,
  callbacks: Pick<RealTimeVADOptions, "onSpeechRealStart" | "onSpeechEnd">,
): Promise<MicVAD> {
  const baseAssetPath = new URL(".", new URL(modelUrl, window.location.href)).href;
  if (new URL(".", new URL(workletUrl, window.location.href)).href !== baseAssetPath) {
    throw new Error("Voice activity assets must share a directory");
  }
  const vad = await MicVAD.new({
    ...callbacks,
    model: "v5",
    startOnLoad: false,
    processorType: "AudioWorklet",
    audioContext: context,
    baseAssetPath,
    ortConfig: (ort) => {
      ort.env.wasm.numThreads = 1;
      ort.env.wasm.wasmPaths = { mjs: runtimeModule, wasm: runtimeWasm };
    },
    getStream: async () => stream,
    pauseStream: async () => {},
    resumeStream: async () => stream,
    minSpeechMs: 250,
    redemptionMs: 900,
    submitUserSpeechOnPause: false,
  });
  try {
    await vad.start();
    stream.getAudioTracks().forEach((track) => { track.enabled = true; });
    return vad;
  } catch (error) {
    await vad.destroy().catch(() => {});
    throw error;
  }
}
