import { env, pipeline, type AutomaticSpeechRecognitionPipeline } from "@huggingface/transformers";
import runtimeModule from "onnxruntime-web/ort-wasm-simd-threaded.mjs?url";
import runtimeWasm from "onnxruntime-web/ort-wasm-simd-threaded.wasm?url";

env.allowLocalModels = false;
env.useWasmCache = false;
env.backends.onnx.wasm!.numThreads = 1;
env.backends.onnx.wasm!.wasmPaths = { mjs: runtimeModule, wasm: runtimeWasm };

let transcriber: Promise<AutomaticSpeechRecognitionPipeline> | undefined;

self.onmessage = async (event: MessageEvent<{ audio?: Float32Array }>) => {
  try {
    transcriber ??= pipeline<"automatic-speech-recognition">("automatic-speech-recognition", "onnx-community/whisper-tiny", {
      revision: "ff4177021cc41f7db950912b73ea4fdf7d01d8e7",
      device: "wasm",
      dtype: "q8",
    });
    const model = await transcriber;
    if (event.data.audio === undefined) {
      self.postMessage({ type: "ready" });
      return;
    }
    const output = await model(event.data.audio, { chunk_length_s: 30, stride_length_s: 5 });
    const text = Array.isArray(output) ? output.map((part) => part.text).join(" ") : output.text;
    self.postMessage({ type: "result", text });
  } catch {
    transcriber = undefined;
    self.postMessage({ type: "error" });
  }
};
