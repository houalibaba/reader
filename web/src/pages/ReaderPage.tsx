import { useEffect, useRef, useState } from 'preact/hooks';
import { apiClient, ReadingSession, SentenceToken, StoryVersionDetail } from '../api/client';
import { audioPlayer } from '../audio/player';
import { SentenceDisplay } from '../components/SentenceDisplay';
import {
  eventQueue,
  QueuedAudioEvent,
  QueuedCharacterEvent,
  QueuedVisibilityEvent
} from '../queue/offlineQueue';

interface ReaderPageProps {
  session: ReadingSession;
  story: StoryVersionDetail;
  showPinyinHint: boolean;
  onExit: () => void;
}

function makeUUID(prefix: string): string {
  return `${prefix}_${Date.now()}_${Math.random().toString(36).slice(2, 9)}`;
}

export function ReaderPage({ session, story, showPinyinHint, onExit }: ReaderPageProps) {
  const [sentenceIndex, setSentenceIndex] = useState<number>(session.current_sentence_index || 0);
  const [activeWordIndex, setActiveWordIndex] = useState<number | null>(null);
  const [activeToken, setActiveToken] = useState<SentenceToken | null>(null);
  const [playingAudio, setPlayingAudio] = useState<boolean>(false);

  const viewIdRef = useRef<string>(makeUUID('sv'));
  const enteredAtIsoRef = useRef<string>(new Date().toISOString());
  const enteredAtPerfRef = useRef<number>(performance.now());
  const hiddenStartedAtRef = useRef<number | null>(null);
  const visitedIndicesRef = useRef<Set<number>>(new Set([session.current_sentence_index || 0]));

  const charEventsRef = useRef<QueuedCharacterEvent[]>([]);
  const audioEventsRef = useRef<QueuedAudioEvent[]>([]);
  const visibilityEventsRef = useRef<QueuedVisibilityEvent[]>([]);

  const currentSentence = story.sentences[sentenceIndex] || story.sentences[0];

  const flushCurrentSentenceView = async (
    nextIndex: number,
    status: 'active' | 'completed' | 'abandoned' = 'active',
    endReason = ''
  ) => {
    if (!currentSentence) return;
    const totalElapsedMs = Math.max(0, Math.round(performance.now() - enteredAtPerfRef.current));
    const exitedIso = new Date().toISOString();
    const isRevisit = visitedIndicesRef.current.has(sentenceIndex) && charEventsRef.current.length === 0;

    await eventQueue.enqueue({
      envelope_id: makeUUID('env'),
      session_id: session.id,
      current_sentence_index: nextIndex,
      session_status: status,
      end_reason: endReason,
      sentence_views: [
        {
          id: viewIdRef.current,
          sentence_id: currentSentence.id,
          entered_at: enteredAtIsoRef.current,
          exited_at: exitedIso,
          total_elapsed_ms: totalElapsedMs,
          is_revisit: isRevisit,
          client_version: '0.1.0-mvp'
        }
      ],
      character_events: [...charEventsRef.current],
      audio_events: [...audioEventsRef.current],
      visibility_events: [...visibilityEventsRef.current],
      created_at: Date.now()
    });
  };

  useEffect(() => {
    viewIdRef.current = makeUUID('sv');
    enteredAtIsoRef.current = new Date().toISOString();
    enteredAtPerfRef.current = performance.now();
    charEventsRef.current = [];
    audioEventsRef.current = [];
    visibilityEventsRef.current = [];
    setActiveWordIndex(null);
    setActiveToken(null);

    if (currentSentence) {
      const punct = new Set(['，', '。', '！', '？', '、', '：', '；', '“', '”', '‘', '’', '（', '）']);
      const chars = currentSentence.tokens
        .map((t) => t.character)
        .filter((ch) => !punct.has(ch));
      audioPlayer.prefetchTexts([currentSentence.text, ...chars]);
    }

    const handleVisibilityChange = () => {
      const nowIso = new Date().toISOString();
      if (document.visibilityState === 'hidden') {
        hiddenStartedAtRef.current = performance.now();
        visibilityEventsRef.current.push({
          id: makeUUID('ve'),
          sentence_view_id: viewIdRef.current,
          state: 'hidden',
          occurred_at: nowIso,
          background_duration_ms: 0
        });
      } else if (document.visibilityState === 'visible' && hiddenStartedAtRef.current !== null) {
        const bgMs = Math.max(0, Math.round(performance.now() - hiddenStartedAtRef.current));
        hiddenStartedAtRef.current = null;
        visibilityEventsRef.current.push({
          id: makeUUID('ve'),
          sentence_view_id: viewIdRef.current,
          state: 'visible',
          occurred_at: nowIso,
          background_duration_ms: bgMs
        });
      }
    };

    document.addEventListener('visibilitychange', handleVisibilityChange);
    return () => {
      document.removeEventListener('visibilitychange', handleVisibilityChange);
    };
  }, [sentenceIndex]);

  const handleTokenClick = async (token: SentenceToken) => {
    setActiveToken(token);
    setActiveWordIndex(token.word_index);

    const clickedIso = new Date().toISOString();
    charEventsRef.current.push({
      id: makeUUID('ce'),
      sentence_view_id: viewIdRef.current,
      token_id: token.id,
      character: token.character,
      pinyin_played: token.pinyin,
      clicked_at: clickedIso
    });

    await audioPlayer.playUrlOrFallback(token.audio_url, token.character);
  };

  const handlePlaySentenceAudio = async () => {
    if (!currentSentence) return;
    setPlayingAudio(true);
    const playedIso = new Date().toISOString();
    const res = await audioPlayer.playUrlOrFallback(currentSentence.audio_url, currentSentence.text);
    setPlayingAudio(false);

    audioEventsRef.current.push({
      id: makeUUID('ae'),
      sentence_view_id: viewIdRef.current,
      audio_type: 'sentence',
      played_at: playedIso,
      play_duration_ms: res.durationMs,
      completed: res.completed
    });
  };

  const handlePrev = async () => {
    if (sentenceIndex <= 0) return;
    const prevIdx = sentenceIndex - 1;
    await flushCurrentSentenceView(prevIdx, 'active');
    visitedIndicesRef.current.add(prevIdx);
    setSentenceIndex(prevIdx);
  };

  const handleNext = async () => {
    if (sentenceIndex < story.sentences.length - 1) {
      const nextIdx = sentenceIndex + 1;
      await flushCurrentSentenceView(nextIdx, 'active');
      visitedIndicesRef.current.add(nextIdx);
      setSentenceIndex(nextIdx);
    } else {
      await flushCurrentSentenceView(sentenceIndex, 'completed', 'finished');
      onExit();
    }
  };

  const handleUserExit = async () => {
    await flushCurrentSentenceView(sentenceIndex, 'active', 'user_exit');
    onExit();
  };

  if (!currentSentence) return null;

  return (
    <div class="reader-stage">
      <header class="reader-topbar">
        <button type="button" class="btn-secondary" onClick={handleUserExit}>
          ← 保存并返回书架
        </button>
        <div class="reader-title-group">
          <h2>{story.title}</h2>
          <span class="badge-mode">{session.mode === 'solo' ? '独立阅读' : '家长陪读'}</span>
        </div>
        <div class="reader-progress">
          第 {sentenceIndex + 1} / {story.sentences.length} 页
        </div>
      </header>

      <div class="reader-illustration-card">
        {currentSentence.image_url ? (
          <img src={currentSentence.image_url} alt={story.title} class="story-image" />
        ) : (
          <div class="story-image-placeholder">
            <span class="placeholder-icon">🏞️</span>
            <span>{story.title} · 第 {sentenceIndex + 1} 幕插图</span>
          </div>
        )}
      </div>

      <SentenceDisplay
        tokens={currentSentence.tokens}
        activeWordIndex={activeWordIndex}
        activeTokenId={activeToken?.id || null}
        showPinyinHint={showPinyinHint}
        onTokenClick={handleTokenClick}
      />

      {activeToken && (
        <div class="help-feedback-banner" aria-live="polite">
          <span>
            点字发音：<strong>{activeToken.character}</strong>（<strong>{activeToken.pinyin}</strong>）
          </span>
          <span>
            所属词语：<strong>{activeToken.word}</strong>
          </span>
        </div>
      )}

      <footer class="reader-controls">
        <button
          type="button"
          class="btn-nav"
          disabled={sentenceIndex === 0}
          onClick={handlePrev}
        >
          上一句
        </button>

        <button
          type="button"
          class="btn-audio"
          onClick={handlePlaySentenceAudio}
          disabled={playingAudio}
        >
          🔊 {playingAudio ? '正在朗读...' : '整句朗读'}
        </button>

        <button type="button" class="btn-nav btn-primary" onClick={handleNext}>
          {sentenceIndex === story.sentences.length - 1 ? '读完啦 🎉' : '下一句 →'}
        </button>
      </footer>
    </div>
  );
}
