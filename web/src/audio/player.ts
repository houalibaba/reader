// Audio player supporting:
// 1. Static media files (/media/audio/...) when real audio (>512B) exists
// 2. Router-cached neural/dictionary TTS stream (/api/v1/tts?text=...) with 900ms fast-timeout + bounded background prefetch
// 3. Browser Web Speech API (window.speechSynthesis) instant fallback

export interface PlayResult {
  durationMs: number;
  completed: boolean;
}

const STATIC_AUDIO_TIMEOUT_MS = 1500;
const TTS_AUDIO_TIMEOUT_MS = 900;
const PREFETCH_TIMEOUT_MS = 5000;
const MAX_PREFETCH_CONCURRENCY = 2;
const MAX_PREFETCH_CACHE_SIZE = 300;

class ReaderAudioPlayer {
  private currentAudio: HTMLAudioElement | null = null;
  private prefetched = new Set<string>();
  private pendingQueue: string[] = [];
  private activePrefetchWorkers = 0;

  stop(): void {
    if (this.currentAudio) {
      this.currentAudio.pause();
      this.currentAudio.currentTime = 0;
      this.currentAudio.src = '';
      this.currentAudio = null;
    }
    if (
      'speechSynthesis' in window &&
      (window.speechSynthesis.speaking || window.speechSynthesis.pending)
    ) {
      window.speechSynthesis.cancel();
    }
  }

  private async fetchWithTimeout(url: string, timeoutMs = PREFETCH_TIMEOUT_MS): Promise<void> {
    const controller = new AbortController();
    const timer = window.setTimeout(() => controller.abort(), timeoutMs);
    try {
      await fetch(url, { signal: controller.signal });
    } catch {
      // Ignore prefetch/timeout errors
    } finally {
      window.clearTimeout(timer);
    }
  }

  private pumpPrefetchQueue(): void {
    while (
      this.activePrefetchWorkers < MAX_PREFETCH_CONCURRENCY &&
      this.pendingQueue.length > 0
    ) {
      const text = this.pendingQueue.shift();
      if (!text) continue;
      this.activePrefetchWorkers += 1;
      void this.fetchWithTimeout(
        `/api/v1/tts?text=${encodeURIComponent(text)}`,
        PREFETCH_TIMEOUT_MS
      ).finally(() => {
        this.activePrefetchWorkers = Math.max(0, this.activePrefetchWorkers - 1);
        this.pumpPrefetchQueue();
      });
    }
  }

  /**
   * Silently pre-warms server-side & browser-side cache for the current sentence and its characters
   * with strict concurrency (2 workers) and per-request network timeout so the router is never saturated.
   */
  prefetchTexts(texts: string[]): void {
    const unique = Array.from(new Set(texts.map((t) => t.trim()).filter(Boolean)));
    if (this.prefetched.size > MAX_PREFETCH_CACHE_SIZE) {
      this.prefetched.clear();
    }
    for (const text of unique) {
      if (this.prefetched.has(text)) continue;
      this.prefetched.add(text);
      this.pendingQueue.push(text);
    }
    this.pumpPrefetchQueue();
  }

  private tryPlayAudioUrl(
    url: string,
    startTime: number,
    loadTimeoutMs = TTS_AUDIO_TIMEOUT_MS
  ): Promise<PlayResult> {
    return new Promise((resolve, reject) => {
      const audio = new Audio(url);
      this.currentAudio = audio;
      let settled = false;
      let startedPlaying = false;

      const loadTimer = window.setTimeout(() => {
        if (!settled && !startedPlaying) {
          settled = true;
          audio.pause();
          audio.src = '';
          if (this.currentAudio === audio) {
            this.currentAudio = null;
          }
          reject(new Error('audio load timeout'));
        }
      }, loadTimeoutMs);

      const finishOk = () => {
        if (settled) return;
        window.clearTimeout(loadTimer);
        const elapsed = Math.round(performance.now() - startTime);
        if ((audio.duration && audio.duration < 0.12) || elapsed < 80) {
          settled = true;
          reject(new Error('silent or empty audio'));
          return;
        }
        settled = true;
        resolve({ durationMs: elapsed, completed: true });
      };

      const finishErr = (err: unknown) => {
        if (settled) return;
        window.clearTimeout(loadTimer);
        settled = true;
        reject(err instanceof Error ? err : new Error('audio playback failed'));
      };

      audio.onplaying = () => {
        startedPlaying = true;
        window.clearTimeout(loadTimer);
      };
      audio.onended = finishOk;
      audio.onerror = () => finishErr(new Error('media load failed'));
      audio.play().catch(finishErr);
    });
  }

  private speakWithWebSpeech(text: string, startTime: number): Promise<PlayResult> {
    if (!('speechSynthesis' in window) || !text) {
      return Promise.resolve({ durationMs: 350, completed: true });
    }
    return new Promise((resolve) => {
      let settled = false;
      const done = (completed: boolean) => {
        if (settled) return;
        settled = true;
        window.clearTimeout(timer);
        resolve({
          durationMs: Math.max(200, Math.round(performance.now() - startTime)),
          completed
        });
      };

      const timer = window.setTimeout(() => done(true), Math.max(3000, text.length * 450));
      try {
        if (window.speechSynthesis.paused) {
          window.speechSynthesis.resume();
        }
        const utter = new SpeechSynthesisUtterance(text);
        utter.lang = 'zh-CN';
        utter.rate = 0.88;
        const voices = window.speechSynthesis.getVoices?.() || [];
        const zhVoice =
          voices.find((v) => v.lang === 'zh-CN' || v.lang === 'zh_CN') ||
          voices.find(
            (v) => v.lang.toLowerCase().startsWith('zh') || v.lang.toLowerCase().includes('cmn')
          );
        if (zhVoice) {
          utter.voice = zhVoice;
        }
        utter.onend = () => done(true);
        utter.onerror = () => done(false);
        window.speechSynthesis.speak(utter);
      } catch {
        done(false);
      }
    });
  }

  async playUrlOrFallback(audioUrl: string | undefined, fallbackText: string): Promise<PlayResult> {
    this.stop();
    const startTime = performance.now();
    const cleanText = (fallbackText || '').trim();

    if (audioUrl && audioUrl.trim() !== '') {
      try {
        return await this.tryPlayAudioUrl(audioUrl, startTime, STATIC_AUDIO_TIMEOUT_MS);
      } catch {
        // Fall through
      }
    }

    if (cleanText) {
      try {
        const ttsUrl = `/api/v1/tts?text=${encodeURIComponent(cleanText)}`;
        // 900ms fast timeout: if not cached/ready quickly, fall back to instant local Web Speech immediately
        return await this.tryPlayAudioUrl(ttsUrl, startTime, TTS_AUDIO_TIMEOUT_MS);
      } catch {
        // Trigger bounded background cache warm-up for next click while immediately speaking locally
        this.prefetchTexts([cleanText]);
      }
    }

    return this.speakWithWebSpeech(cleanText, startTime);
  }
}

export const audioPlayer = new ReaderAudioPlayer();
