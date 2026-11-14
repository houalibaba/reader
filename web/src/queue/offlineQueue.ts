// IndexedDB Offline Event Queue for LAN-only HTTP (without requiring HTTPS Service Worker)

export interface QueuedSentenceView {
  id: string;
  sentence_id: string;
  entered_at: string;
  exited_at?: string;
  total_elapsed_ms: number;
  is_revisit: boolean;
  client_version: string;
}

export interface QueuedCharacterEvent {
  id: string;
  sentence_view_id: string;
  token_id: string;
  character: string;
  pinyin_played: string;
  clicked_at: string;
}

export interface QueuedAudioEvent {
  id: string;
  sentence_view_id: string;
  audio_type: 'sentence' | 'character' | 'word';
  played_at: string;
  play_duration_ms: number;
  completed: boolean;
}

export interface QueuedVisibilityEvent {
  id: string;
  sentence_view_id: string;
  state: 'hidden' | 'visible';
  occurred_at: string;
  background_duration_ms: number;
}

export interface QueuedBatchEnvelope {
  envelope_id: string;
  session_id: string;
  current_sentence_index?: number;
  session_status?: 'active' | 'completed' | 'abandoned';
  end_reason?: string;
  sentence_views: QueuedSentenceView[];
  character_events: QueuedCharacterEvent[];
  audio_events: QueuedAudioEvent[];
  visibility_events: QueuedVisibilityEvent[];
  created_at: number;
}

const DB_NAME = 'reader_offline_queue_db';
const DB_VERSION = 1;
const STORE_NAME = 'event_batches';

function openDB(): Promise<IDBDatabase> {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open(DB_NAME, DB_VERSION);
    req.onupgradeneeded = () => {
      const db = req.result;
      if (!db.objectStoreNames.contains(STORE_NAME)) {
        db.createObjectStore(STORE_NAME, { keyPath: 'envelope_id' });
      }
    };
    req.onsuccess = () => resolve(req.result);
    req.onerror = () => reject(req.error);
  });
}

export class OfflineEventQueue {
  private timer: number | null = null;
  private flushing = false;
  private listeners: Array<(pendingCount: number) => void> = [];

  startAutoFlush(intervalMs = 5000): void {
    if (this.timer !== null) return;
    this.timer = window.setInterval(() => {
      void this.flush();
    }, intervalMs);
    window.addEventListener('online', () => {
      void this.flush();
    });
  }

  onPendingChange(cb: (pendingCount: number) => void): () => void {
    this.listeners.push(cb);
    return () => {
      this.listeners = this.listeners.filter((fn) => fn !== cb);
    };
  }

  private async notifyCount(): Promise<void> {
    const count = await this.getPendingCount();
    for (const cb of this.listeners) cb(count);
  }

  async enqueue(envelope: QueuedBatchEnvelope): Promise<void> {
    const db = await openDB();
    await new Promise<void>((resolve, reject) => {
      const tx = db.transaction(STORE_NAME, 'readwrite');
      tx.objectStore(STORE_NAME).put(envelope);
      tx.oncomplete = () => resolve();
      tx.onerror = () => reject(tx.error);
    });
    await this.notifyCount();
    void this.flush();
  }

  async getPendingCount(): Promise<number> {
    try {
      const db = await openDB();
      return await new Promise<number>((resolve, reject) => {
        const tx = db.transaction(STORE_NAME, 'readonly');
        const req = tx.objectStore(STORE_NAME).count();
        req.onsuccess = () => resolve(req.result);
        req.onerror = () => reject(req.error);
      });
    } catch {
      return 0;
    }
  }

  async flush(): Promise<number> {
    if (this.flushing) return 0;
    this.flushing = true;
    let sent = 0;
    try {
      const db = await openDB();
      const batches = await new Promise<QueuedBatchEnvelope[]>((resolve, reject) => {
        const tx = db.transaction(STORE_NAME, 'readonly');
        const req = tx.objectStore(STORE_NAME).getAll();
        req.onsuccess = () => resolve(req.result || []);
        req.onerror = () => reject(req.error);
      });

      batches.sort((a, b) => a.created_at - b.created_at);

      for (const batch of batches) {
        try {
          const res = await fetch(`/api/v1/sessions/${encodeURIComponent(batch.session_id)}/events`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              current_sentence_index: batch.current_sentence_index,
              session_status: batch.session_status,
              end_reason: batch.end_reason,
              sentence_views: batch.sentence_views,
              character_events: batch.character_events,
              audio_events: batch.audio_events,
              visibility_events: batch.visibility_events
            })
          });
          if (res.ok) {
            await new Promise<void>((resolve, reject) => {
              const tx = db.transaction(STORE_NAME, 'readwrite');
              tx.objectStore(STORE_NAME).delete(batch.envelope_id);
              tx.oncomplete = () => resolve();
              tx.onerror = () => reject(tx.error);
            });
            sent++;
          } else {
            break;
          }
        } catch {
          // Network offline or unreachable; keep remaining in IndexedDB for next retry
          break;
        }
      }
    } finally {
      this.flushing = false;
      await this.notifyCount();
    }
    return sent;
  }
}

export const eventQueue = new OfflineEventQueue();
