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

// 拼音与词语提示持续显示 10.5 秒
const PINYIN_DISPLAY_DURATION_MS = 10500;

export function ReaderPage({ session, story, showPinyinHint, onExit }: ReaderPageProps) {
  const [sentenceIndex, setSentenceIndex] = useState<number>(session.current_sentence_index || 0);
  const [activeWordIndex, setActiveWordIndex] = useState<number | null>(null);
  const [activeToken, setActiveToken] = useState<SentenceToken | null>(null);
  const [playingAudio, setPlayingAudio] = useState<boolean>(false);
  const [isImageFullscreen, setIsImageFullscreen] = useState<boolean>(false);
  const [isFinishedCelebration, setIsFinishedCelebration] = useState<boolean>(false);
  const [localPinyinHint, setLocalPinyinHint] = useState<boolean>(showPinyinHint);
  const [showSettingsSheet, setShowSettingsSheet] = useState<boolean>(false);

  // 当前读者的复习字与新字集合（用于阅读页底色标记）
  const [reviewCharSet, setReviewCharSet] = useState<Set<string>>(() => new Set());
  const [newCharSet, setNewCharSet] = useState<Set<string>>(() => new Set());

  const viewIdRef = useRef<string>(makeUUID('sv'));
  const enteredAtIsoRef = useRef<string>(new Date().toISOString());
  const enteredAtPerfRef = useRef<number>(performance.now());
  const hiddenStartedAtRef = useRef<number | null>(null);
  const lifecycleExitFlushedRef = useRef<boolean>(false);
  const visitedIndicesRef = useRef<Set<number>>(new Set([session.current_sentence_index || 0]));
  const highlightTimerRef = useRef<number | null>(null);
  const imageDialogRef = useRef<HTMLDialogElement | null>(null);
  const settingsDialogRef = useRef<HTMLDialogElement | null>(null);

  const charEventsRef = useRef<QueuedCharacterEvent[]>([]);
  const audioEventsRef = useRef<QueuedAudioEvent[]>([]);
  const visibilityEventsRef = useRef<QueuedVisibilityEvent[]>([]);

  const currentSentence = story.sentences[sentenceIndex] || story.sentences[0];
  const totalSentences = Math.max(1, story.sentences.length);
  const progressPct = Math.min(100, Math.max(5, Math.round(((sentenceIndex + 1) / totalSentences) * 100)));

  useEffect(() => {
    setLocalPinyinHint(showPinyinHint);
  }, [showPinyinHint]);

  // 静默拉取当前儿童的艾宾浩斯复习字与新字表，用于阅读页标记
  useEffect(() => {
    let cancelled = false;
    if (!session.child_id) return;
    apiClient
      .getStoryPlan(session.child_id)
      .then((plan) => {
        if (cancelled || !plan) return;
        const rev = new Set<string>();
        const nw = new Set<string>();
        plan.review_targets?.forEach((t) => rev.add(t.character));
        plan.observe_targets?.forEach((t) => rev.add(t.character));
        plan.new_candidates?.forEach((t) => {
          if (!rev.has(t.character)) {
            nw.add(t.character);
          }
        });
        setReviewCharSet(rev);
        setNewCharSet(nw);
      })
      .catch(() => {
        // 离线或失败时静默降级
      });
    return () => {
      cancelled = true;
    };
  }, [session.child_id]);

  const flushCurrentSentenceView = async (
    nextIndex: number,
    status: 'active' | 'completed' | 'abandoned' = 'active',
    endReason = '',
    keepalive = false
  ) => {
    if (!currentSentence) return;
    const totalElapsedMs = Math.max(0, Math.round(performance.now() - enteredAtPerfRef.current));
    const exitedIso = new Date().toISOString();
    const isRevisit = visitedIndicesRef.current.has(sentenceIndex) && charEventsRef.current.length === 0;

    await eventQueue.enqueue(
      {
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
            client_version: '0.5.0'
          }
        ],
        character_events: [...charEventsRef.current],
        audio_events: [...audioEventsRef.current],
        visibility_events: [...visibilityEventsRef.current],
        created_at: Date.now()
      },
      keepalive
    );
  };

  useEffect(() => {
    const dialog = imageDialogRef.current;
    if (!dialog) return;
    if (isImageFullscreen) {
      if (!dialog.open && typeof dialog.showModal === 'function') {
        dialog.showModal();
      }
    } else if (dialog.open) {
      dialog.close();
    }
  }, [isImageFullscreen]);

  useEffect(() => {
    const dialog = settingsDialogRef.current;
    if (!dialog) return;
    if (showSettingsSheet) {
      if (!dialog.open && typeof dialog.showModal === 'function') {
        dialog.showModal();
      }
    } else if (dialog.open) {
      dialog.close();
    }
  }, [showSettingsSheet]);

  useEffect(() => {
    lifecycleExitFlushedRef.current = false;
    viewIdRef.current = makeUUID('sv');
    enteredAtIsoRef.current = new Date().toISOString();
    enteredAtPerfRef.current = performance.now();
    charEventsRef.current = [];
    audioEventsRef.current = [];
    visibilityEventsRef.current = [];
    setActiveWordIndex(null);
    setActiveToken(null);
    setIsImageFullscreen(false);
    if (highlightTimerRef.current !== null) {
      window.clearTimeout(highlightTimerRef.current);
      highlightTimerRef.current = null;
    }

    // 预取当前句与单字 TTS 音频，实现点击零延迟发音
    if (currentSentence) {
      const punct = new Set(['，', '。', '！', '？', '、', '：', '；', '“', '”', '‘', '’', '（', '）']);
      const chars = currentSentence.tokens
        .map((t) => t.character)
        .filter((ch) => !punct.has(ch));
      audioPlayer.prefetchTexts([currentSentence.text, ...chars]);
    }

    // 静默预加载下一句插图
    const nextSentence = story.sentences[sentenceIndex + 1];
    if (nextSentence && nextSentence.image_url) {
      const preloadImg = new Image();
      preloadImg.src = nextSentence.image_url;
    }

    const flushOnLifecycleExit = () => {
      if (lifecycleExitFlushedRef.current) return;
      lifecycleExitFlushedRef.current = true;
      void flushCurrentSentenceView(sentenceIndex, 'active', 'user_exit', true);
    };

    const handleVisibilityChange = () => {
      const nowIso = new Date().toISOString();
      if (document.visibilityState === 'hidden') {
        if (hiddenStartedAtRef.current !== null) return;
        hiddenStartedAtRef.current = performance.now();
        visibilityEventsRef.current.push({
          id: makeUUID('ve'),
          sentence_view_id: viewIdRef.current,
          state: 'hidden',
          occurred_at: nowIso,
          background_duration_ms: 0
        });
        flushOnLifecycleExit();
      } else if (document.visibilityState === 'visible' && hiddenStartedAtRef.current !== null) {
        const bgMs = Math.max(0, Math.round(performance.now() - hiddenStartedAtRef.current));
        hiddenStartedAtRef.current = null;
        lifecycleExitFlushedRef.current = false;
        visibilityEventsRef.current.push({
          id: makeUUID('ve'),
          sentence_view_id: viewIdRef.current,
          state: 'visible',
          occurred_at: nowIso,
          background_duration_ms: bgMs
        });
      }
    };

    const handlePageHide = flushOnLifecycleExit;

    document.addEventListener('visibilitychange', handleVisibilityChange);
    window.addEventListener('pagehide', handlePageHide);
    return () => {
      document.removeEventListener('visibilitychange', handleVisibilityChange);
      window.removeEventListener('pagehide', handlePageHide);
      if (highlightTimerRef.current !== null) {
        window.clearTimeout(highlightTimerRef.current);
      }
      audioPlayer.stop();
    };
  }, [sentenceIndex]);

  const handleTokenClick = async (token: SentenceToken) => {
    setActiveToken(token);
    setActiveWordIndex(token.word_index);

    if (highlightTimerRef.current !== null) {
      window.clearTimeout(highlightTimerRef.current);
    }
    highlightTimerRef.current = window.setTimeout(() => {
      setActiveWordIndex(null);
      setActiveToken(null);
      highlightTimerRef.current = null;
    }, PINYIN_DISPLAY_DURATION_MS);

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

  const handlePlayWholeSentence = async () => {
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

  const handlePrevSentence = async () => {
    if (sentenceIndex <= 0) return;
    const prevIdx = sentenceIndex - 1;
    await flushCurrentSentenceView(prevIdx, 'active');
    visitedIndicesRef.current.add(prevIdx);
    setSentenceIndex(prevIdx);
  };

  const handleNextSentence = async () => {
    if (sentenceIndex < story.sentences.length - 1) {
      const nextIdx = sentenceIndex + 1;
      await flushCurrentSentenceView(nextIdx, 'active');
      visitedIndicesRef.current.add(nextIdx);
      setSentenceIndex(nextIdx);
    } else {
      lifecycleExitFlushedRef.current = true;
      await flushCurrentSentenceView(sentenceIndex, 'completed', 'finished', true);
      setIsFinishedCelebration(true);
    }
  };

  const handleSaveAndExit = async () => {
    lifecycleExitFlushedRef.current = true;
    await flushCurrentSentenceView(sentenceIndex, 'active', 'user_exit');
    onExit();
  };

  if (!currentSentence) {
    return null;
  }

  const bgImageUrl = currentSentence.image_url || story.cover_url || '';

  return (
    <div class="reader-stage-immersive">
      {/* 最底层：当前插图全屏高斯模糊氛围背景 */}
      <div
        class="reader-ambient-bg"
        style={bgImageUrl ? { backgroundImage: `url("${bgImageUrl}")` } : undefined}
        aria-hidden="true"
      />
      <div class="reader-ambient-scrim" aria-hidden="true" />

      <div class="reader-stage">
        {/* 极简顶部导航：左返回箭头 + 中间进度条 + 页码 + 右侧设置齿轮 */}
        <header class="reader-topbar-minimal">
          <button
            type="button"
            class="reader-icon-btn"
            onClick={handleSaveAndExit}
            aria-label="保存并返回书架"
            title="保存并返回书架"
          >
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M19 12H5" />
              <path d="M12 19l-7-7 7-7" />
            </svg>
          </button>

          <div class="reader-progress-track" role="progressbar" aria-valuenow={sentenceIndex + 1} aria-valuemin={1} aria-valuemax={totalSentences}>
            <div class="reader-progress-fill" style={{ width: `${progressPct}%` }} />
          </div>

          <span class="reader-page-counter">
            {sentenceIndex + 1} / {totalSentences}
          </span>

          <button
            type="button"
            class="reader-icon-btn"
            onClick={() => setShowSettingsSheet(true)}
            aria-label="阅读设置"
            title="阅读设置"
          >
            <svg width="22" height="22" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <circle cx="12" cy="12" r="3" />
              <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z" />
            </svg>
          </button>
        </header>

        {/* 紧凑型插图区：约占屏幕 1/4 高度，保证下方整句话一屏内完整显示 */}
        <div key={currentSentence.id} class="reader-illustration-compact">
          {currentSentence.image_url ? (
            <img
              src={currentSentence.image_url}
              alt={story.title}
              class="story-image"
              fetchpriority="high"
              style={{ cursor: 'zoom-in' }}
              title="点击放大查看插图"
              onClick={() => setIsImageFullscreen(true)}
            />
          ) : (
            <div class="story-image-placeholder">
              <span>插图（第 {sentenceIndex + 1} 页 · {story.title}）</span>
            </div>
          )}
        </div>

        {/* 毛玻璃文字卡片：按词语换行 + 复习字/新字高亮 + 点一下出拼音 */}
        <SentenceDisplay
          tokens={currentSentence.tokens}
          activeWordIndex={activeWordIndex}
          activeTokenId={activeToken?.id || null}
          showPinyinHint={localPinyinHint}
          reviewChars={reviewCharSet}
          newChars={newCharSet}
          showLegend={true}
          onTokenClick={handleTokenClick}
        />

        {/* 底部操控栏：左白框上一页 + 中间圆形喇叭 + 右黑底下一页 */}
        <footer class="reader-controls-modern">
          <button
            type="button"
            class="btn-page-prev"
            disabled={sentenceIndex === 0}
            onClick={handlePrevSentence}
          >
            ‹ 上一页
          </button>

          <button
            type="button"
            class={`btn-audio-circle ${playingAudio ? 'is-playing' : ''}`}
            onClick={handlePlayWholeSentence}
            disabled={playingAudio}
            aria-label={playingAudio ? '正在朗读整句' : '听整句朗读'}
            title="听整句朗读"
          >
            <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5" />
              <path d="M15.54 8.46a5 5 0 0 1 0 7.07" />
              <path d="M19.07 4.93a10 10 0 0 1 0 14.14" />
            </svg>
          </button>

          <button
            type="button"
            class="btn-page-next"
            onClick={handleNextSentence}
          >
            {sentenceIndex < story.sentences.length - 1 ? '下一页 ›' : '读完啦 ›'}
          </button>
        </footer>
      </div>

      {/* 原生 HTML <dialog> 全屏插图查看器 */}
      <dialog
        ref={imageDialogRef}
        class="fullscreen-image-dialog"
        onClick={() => setIsImageFullscreen(false)}
        onCancel={() => setIsImageFullscreen(false)}
      >
        {currentSentence.image_url && (
          <div class="dialog-image-wrapper">
            <img
              src={currentSentence.image_url}
              alt={story.title}
              class="dialog-fullscreen-img"
            />
            <span class="dialog-close-hint">点击任意位置返回阅读</span>
          </div>
        )}
      </dialog>

      {/* 原生 HTML <dialog> 阅读设置弹窗 */}
      <dialog
        ref={settingsDialogRef}
        class="reader-settings-dialog"
        onClick={(e) => {
          if (e.target === settingsDialogRef.current) {
            setShowSettingsSheet(false);
          }
        }}
        onCancel={() => setShowSettingsSheet(false)}
      >
        <div class="reader-settings-sheet">
          <div class="settings-sheet-header">
            <strong>阅读设置 · 《{story.title}》</strong>
            <button
              type="button"
              class="reader-icon-btn"
              onClick={() => setShowSettingsSheet(false)}
              aria-label="关闭设置"
            >
              ✕
            </button>
          </div>
          <label class="settings-sheet-row">
            <span>常驻显示全句拼音</span>
            <input
              type="checkbox"
              checked={localPinyinHint}
              onChange={(e) => setLocalPinyinHint((e.target as HTMLInputElement).checked)}
            />
          </label>
          <div class="settings-sheet-row">
            <span>当前阅读模式</span>
            <span>{session.mode === 'accompanied' ? '👨‍👧 家长陪读' : '🧒 独立阅读'}</span>
          </div>
          <div class="settings-sheet-actions">
            <button
              type="button"
              class="btn-page-prev"
              onClick={() => {
                setShowSettingsSheet(false);
                void handleSaveAndExit();
              }}
            >
              保存进度并返回书架
            </button>
          </div>
        </div>
      </dialog>

      {/* 读完本篇庆祝激励弹窗 */}
      {isFinishedCelebration && (
        <div
          role="dialog"
          aria-modal="true"
          aria-label="阅读完成庆祝"
          class="celebration-backdrop"
        >
          <div class="celebration-card">
            <div style={{ fontSize: '56px', lineHeight: 1 }}>🎉🌟🏆</div>
            <h2>太棒啦！读完《{story.title}》！</h2>
            <p>
              共坚持读完 <strong>{story.sentences.length}</strong> 页故事，今天又是爱阅读的一天！
            </p>
            <div class="celebration-actions">
              <button
                type="button"
                class="btn-page-prev"
                onClick={() => {
                  setIsFinishedCelebration(false);
                  setSentenceIndex(0);
                }}
              >
                再读一遍
              </button>
              <button
                type="button"
                class="btn-page-next"
                onClick={() => {
                  setIsFinishedCelebration(false);
                  onExit();
                }}
              >
                返回书架
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  );
}
