// Audio player supporting:
// 1. Static media files (/media/audio/...) when real audio (>512B) exists
// 2. Router-cached neural/dictionary TTS stream (/api/v1/tts?text=...) with 900ms fast-timeout + background prefetch
// 3. Browser Web Speech API (window.speechSynthesis) instant fallback

export interface PlayResult {
  durationMs: number;
  completed: boolean;
}

class ReaderAudioPlayer {
  private currentAudio: HTMLAudioElement | null = null;
  private prefetched = new Set<string>();

  stop(): void {
    if (this.currentAudio) {
      this.currentAudio.pause();
      this.currentAudio.currentTime = 0;
      this.currentAudio = null;
    }
    if ('speechSynthesis' in window && (window.speechSynthesis.speaking || window.speechSynthesis.pending)) {
      window.speechSynthesis.cancel();
    }
  }

  /**
   * Silently pre-warms server-side & browser-side cache for the current sentence and its characters
   * right when the user flips to a page, so tapping any character plays instantaneously.
   */
  prefetchTexts(texts: string[]): void {
    const unique = Array.from(new Set(texts.map((t) => t.trim()).filter(Boolean)));
    const pending = unique.filter((t) => !this.prefetched.has(t));
    if (pending.length === 0) return;

    // Limit background concurrency so we never saturate the router
    const queue = [...pending];
    const worker = async () => {
      while (queue.length > 0) {
        const text = queue.shift();
        if (!text || this.prefetched.has(text)) continue;
        this.prefetched.add(text);
        try {
          await fetch(`/api/v1/tts?text=${encodeURIComponent(text)}`);
        } catch {
          // Ignore prefetch errors
        }
      }
    };
    void Promise.all([worker(), worker()]);
  }

  private tryPlayAudioUrl(url: string, startTime: number, loadTimeoutMs = 900): Promise<PlayResult> {
    return new Promise<PlayResult>((resolve, reject) => {
      const audio = new Audio(url);
      this.currentAudio = audio;
      let settled = false;
      let startedPlaying = false;

      const loadTimer = setTimeout(() => {
        if (!settled && !startedPlaying) {
          settled = true;
          audio.pause();
          audio.src = '';
          reject(new Error('audio load timeout'));
        }
      }, loadTimeoutMs);

      const finishOk = () => {
        if (settled) return;
        clearTimeout(loadTimer);
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
        clearTimeout(loadTimer);
        settled = true;
        reject(err instanceof Error ? err : new Error('audio playback failed'));
      };

      audio.onplaying = () => {
        startedPlaying = true;
        clearTimeout(loadTimer);
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

    return new Promise<PlayResult>((resolve) => {
      let settled = false;
      const done = (completed: boolean) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        resolve({
          durationMs: Math.max(200, Math.round(performance.now() - startTime)),
          completed
        });
      };

      const timer = setTimeout(() => done(true), Math.max(3000, text.length * 450));

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
          voices.find((v) => v.lang.toLowerCase().startsWith('zh') || v.lang.toLowerCase().includes('cmn'));
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
        return await this.tryPlayAudioUrl(audioUrl, startTime, 1500);
      } catch {
        // Fall through
      }
    }

    if (cleanText) {
      try {
        const ttsUrl = `/api/v1/tts?text=${encodeURIComponent(cleanText)}`;
        // 900ms fast timeout: if not cached/ready quickly, fall back to instant local Web Speech immediately
        return await this.tryPlayAudioUrl(ttsUrl, startTime, 900);
      } catch {
        // Trigger background cache warm-up for next click while immediately speaking locally
        void fetch(`/api/v1/tts?text=${encodeURIComponent(cleanText)}`).catch(() => {});
      }
    }

    return this.speakWithWebSpeech(cleanText, startTime);
  }
}

export const audioPlayer = new ReaderAudioPlayer();
