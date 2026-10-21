import { LocalTranscription, VOICE_MODEL_DOWNLOADED, VOICE_MODEL_FAILURE } from "./localTranscription.ts";

export type VoicePhase = "idle" | "starting" | "loading" | "recording" | "transcribing";

export interface VoiceState {
  phase: VoicePhase;
  transcript: string;
  seconds: number;
  level: number;
  notice: string | null;
  error: string | null;
}

interface Recognition {
  continuous: boolean;
  interimResults: boolean;
  lang: string;
  onstart: (() => void) | null;
  onend: (() => void) | null;
  onerror: ((event: { error: string }) => void) | null;
  onresult: ((event: { results: ArrayLike<{ isFinal: boolean; 0: { transcript: string } }> }) => void) | null;
  start(): void;
  stop(): void;
  abort(): void;
}

interface Session {
  stream?: MediaStream;
  context?: AudioContext;
  recognition?: Recognition;
  recorder?: MediaRecorder;
  interval?: ReturnType<typeof setInterval>;
  transcript: string;
}

export const idleVoiceState: VoiceState = {
  phase: "idle", transcript: "", seconds: 0, level: 0, notice: null, error: null,
};

const NO_SPEECH = "No speech detected. Try again, or type your message.";

function nativeRecognition() {
  const browser = window as unknown as {
    SpeechRecognition?: new () => Recognition;
    webkitSpeechRecognition?: new () => Recognition;
  };
  return browser.SpeechRecognition ?? browser.webkitSpeechRecognition;
}

export class VoiceInput {
  private session: Session | undefined;
  private state = idleVoiceState;
  private readonly transcription = new LocalTranscription();

  constructor(
    private readonly onState: (state: VoiceState) => void,
    private readonly onTranscript: (transcript: string) => void,
  ) {}

  private update(patch: Partial<VoiceState>) {
    this.state = { ...this.state, ...patch };
    this.onState(this.state);
  }

  async start() {
    if (this.session) return;
    const session: Session = { transcript: "" };
    this.session = session;
    this.update({ ...idleVoiceState, phase: "starting" });
    try {
      if (!navigator.mediaDevices?.getUserMedia) {
        throw new Error("Microphone access requires a secure browser connection. You can still type your message.");
      }
      session.context = new AudioContext();
      const streamPromise = navigator.mediaDevices.getUserMedia({ audio: true }).then((stream) => {
        if (this.session !== session) stream.getTracks().forEach((track) => track.stop());
        else session.stream = stream;
      });
      await Promise.all([streamPromise, session.context.resume()]);
      if (this.session !== session) return;
      const RecognitionAPI = nativeRecognition();
      if (RecognitionAPI) {
        this.startNative(session, new RecognitionAPI());
      } else {
        await this.startFallback(session);
      }
    } catch (error) {
      if (this.session !== session) return;
      const denied = error instanceof DOMException && (error.name === "NotAllowedError" || error.name === "SecurityError");
      this.finish(session, denied
        ? "Microphone access is blocked. Allow microphone access in your browser settings, or type your message."
        : error instanceof Error && error.message.startsWith("Microphone access requires")
          ? error.message
          : "Voice input could not start. Check your microphone and try again, or type your message.");
    }
  }

  private meter(session: Session) {
    if (!session.context || !session.stream) return;
    const source = session.context.createMediaStreamSource(session.stream);
    const analyser = session.context.createAnalyser();
    analyser.fftSize = 256;
    source.connect(analyser);
    const samples = new Float32Array(analyser.fftSize);
    const started = Date.now();
    this.update({ phase: "recording", notice: null });
    session.interval = setInterval(() => {
      analyser.getFloatTimeDomainData(samples);
      const rms = Math.sqrt(samples.reduce((sum, sample) => sum + sample * sample, 0) / samples.length);
      this.update({ seconds: Math.floor((Date.now() - started) / 1000), level: Math.min(1, rms * 8) });
    }, 100);
  }

  private startNative(session: Session, recognition: Recognition) {
    session.recognition = recognition;
    recognition.continuous = true;
    recognition.interimResults = true;
    recognition.lang = navigator.language || "en-US";
    recognition.onstart = () => {
      if (this.session === session && this.state.phase === "starting") this.meter(session);
    };
    recognition.onresult = (event) => {
      if (this.session !== session) return;
      const results = Array.from(event.results);
      session.transcript = results.map((result) => result[0].transcript).join(" ").trim();
      this.update({ transcript: session.transcript });
    };
    recognition.onend = () => this.finish(session, session.transcript ? null : NO_SPEECH);
    recognition.onerror = (event) => {
      if (this.session !== session) return;
      if (event.error === "network" || event.error === "service-not-allowed" || event.error === "language-not-supported") {
        recognition.onend = null;
        recognition.onerror = null;
        recognition.onresult = null;
        recognition.onstart = null;
        recognition.abort();
        session.recognition = undefined;
        clearInterval(session.interval);
        session.interval = undefined;
        void this.startFallback(session);
        return;
      }
      this.finish(session, event.error === "not-allowed"
        ? "Microphone access is blocked. Allow microphone access in your browser settings, or type your message."
        : event.error === "no-speech" ? NO_SPEECH
          : "Speech recognition stopped. Try again, or type your message.");
    };
    recognition.start();
  }

  private async startFallback(session: Session) {
    try {
      let downloaded = this.transcription.downloaded;
      try { downloaded ||= localStorage.getItem(VOICE_MODEL_DOWNLOADED) === "true"; } catch {}
      session.stream?.getAudioTracks().forEach((track) => { track.enabled = false; });
      this.update({
        phase: "loading",
        notice: downloaded ? "Preparing voice input…" :
          "Downloading the speech model for first use (about 75 MB). It is cached in this browser. Audio stays on your device.",
      });
      await this.transcription.load();
      if (this.session !== session) return;
      const chunks: Blob[] = [];
      const recorder = new MediaRecorder(session.stream!);
      session.recorder = recorder;
      recorder.ondataavailable = (event) => { if (event.data.size > 0) chunks.push(event.data); };
      recorder.onerror = () => this.finish(session, "Audio capture failed. Try again, or type your message.");
      recorder.onstop = () => { void this.transcribe(session, new Blob(chunks, { type: recorder.mimeType })); };
      session.stream?.getAudioTracks().forEach((track) => { track.enabled = true; });
      recorder.start();
      this.meter(session);
    } catch {
      if (this.session === session) {
        this.transcription.dispose();
        this.finish(session, VOICE_MODEL_FAILURE);
      }
    }
  }

  private async transcribe(session: Session, blob: Blob) {
    if (this.session !== session) return;
    this.releaseMicrophone(session);
    this.update({ phase: "transcribing", level: 0 });
    try {
      const decoded = await session.context!.decodeAudioData(await blob.arrayBuffer());
      if (this.session !== session) return;
      const offline = new OfflineAudioContext(1, Math.ceil(decoded.duration * 16000), 16000);
      const source = offline.createBufferSource();
      source.buffer = decoded;
      source.connect(offline.destination);
      source.start();
      const rendered = await offline.startRendering();
      if (this.session !== session) return;
      const audio = rendered.getChannelData(0);
      const rms = Math.sqrt(audio.reduce((sum, sample) => sum + sample * sample, 0) / audio.length);
      if (!Number.isFinite(rms) || rms < 0.001) {
        this.finish(session, NO_SPEECH);
        return;
      }
      const text = await this.transcription.transcribe(audio);
      if (this.session !== session) return;
      session.transcript = [session.transcript, text.trim()].filter(Boolean).join(" ");
      this.finish(session, session.transcript ? null : NO_SPEECH);
    } catch {
      if (this.session === session) this.finish(session, "Transcription failed. Try again, or type your message.");
    }
  }

  stop() {
    const session = this.session;
    if (!session) return;
    if (this.state.phase !== "recording") {
      this.cancel();
      return;
    }
    this.update({ phase: "transcribing", level: 0 });
    if (session.recognition) session.recognition.stop();
    else if (session.recorder?.state === "recording") session.recorder.stop();
    this.releaseMicrophone(session);
  }

  private releaseMicrophone(session: Session) {
    clearInterval(session.interval);
    session.stream?.getTracks().forEach((track) => track.stop());
  }

  private cleanup(session: Session) {
    if (session.recognition) {
      session.recognition.onstart = null;
      session.recognition.onend = null;
      session.recognition.onresult = null;
      session.recognition.onerror = null;
      session.recognition.abort();
    }
    if (session.recorder) {
      session.recorder.onstop = null;
      session.recorder.ondataavailable = null;
      session.recorder.onerror = null;
      if (session.recorder.state === "recording") session.recorder.stop();
    }
    this.releaseMicrophone(session);
    if (session.context && session.context.state !== "closed") void session.context.close();
  }

  private finish(session: Session, error: string | null) {
    if (this.session !== session) return;
    this.session = undefined;
    this.cleanup(session);
    if (session.transcript) this.onTranscript(session.transcript);
    this.update({ ...idleVoiceState, error });
  }

  cancel() {
    const session = this.session;
    this.session = undefined;
    if (session) this.cleanup(session);
    if (this.state.phase === "transcribing") this.transcription.dispose();
    this.update(idleVoiceState);
  }

  dispose() {
    this.cancel();
    this.transcription.dispose();
  }
}
