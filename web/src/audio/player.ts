// Audio player supporting:
// 1. Static media files (/media/audio/...) when real audio (>512B) exists
// 2. Router-cached neural/dictionary TTS stream (/api/v1/tts?text=...) so Linux/iOS/Android devices without local TTS voices always have clear Chinese pronunciation
// 3. Browser Web Speech API (window.speechSynthesis) offline fallback with cancel-race protection

export interface PlayResult {
  durationMs: number;
  completed: boolean;
}

class ReaderAudioPlayer {
  private currentAudio: HTMLAudioElement | null = null;

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

  private tryPlayAudioUrl(url: string, startTime: number): Promise<PlayResult> {
    return new Promise<PlayResult>((resolve, reject) => {
      const audio = new Audio(url);
      this.currentAudio = audio;
      let settled = false;

      const finishOk = () => {
        if (settled) return;
        const elapsed = Math.round(performance.now() - startTime);
        // Guard against silent/empty placeholder audio files (<120ms duration)
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
        settled = true;
        reject(err instanceof Error ? err : new Error('audio playback failed'));
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

    // 1. Try explicit static audio URL if present
    if (audioUrl && audioUrl.trim() !== '') {
      try {
        return await this.tryPlayAudioUrl(audioUrl, startTime);
      } catch {
        // Fall through to backend cached TTS & Web Speech
      }
    }

    // 2. Try backend cached TTS endpoint (/api/v1/tts?text=...) which caches real MP3 audio on /mnt/sda1/reader/media/audio/
    if (cleanText) {
      try {
        const ttsUrl = `/api/v1/tts?text=${encodeURIComponent(cleanText)}`;
        return await this.tryPlayAudioUrl(ttsUrl, startTime);
      } catch {
        // Offline or upstream unavailable -> fall through to local Web Speech synthesis
      }
    }

    // 3. Offline browser Web Speech synthesis fallback
    return this.speakWithWebSpeech(cleanText, startTime);
  }
}

export const audioPlayer = new ReaderAudioPlayer();
