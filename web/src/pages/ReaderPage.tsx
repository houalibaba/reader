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
import {
  evaluateAndUnlockStoryAnimation,
  ReadingUnlockOutcome,
  resolveStoryAnimationUrl,
  StoryRewardAnimation
} from '../utils/rewardAnimation';

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

  // 本篇故事 1 对 1 专属动画解锁状态（无专属动画时静默跳过）
  const [unlockOutcome, setUnlockOutcome] = useState<ReadingUnlockOutcome | null>(null);
  const [playingRewardAnim, setPlayingRewardAnim] = useState<StoryRewardAnimation | null>(null);

  // 当前读者的复习字与新字集合（用于阅读页底色标记）
  const [reviewCharSet, setReviewCharSet] = useState<Set<string>>(() => new Set());
  const [newCharSet, setNewCharSet] = useState<Set<string>>(() => new Set());

  const viewIdRef = useRef<string>(makeUUID('sv'));
  const enteredAtIsoRef = useRef<string>(new Date().toISOString());
  const enteredAtPerfRef = useRef<number>(performance.now());
  const hiddenStartedAtRef = useRef<number | null>(null);
  const pageBgHiddenMsRef = useRef<number>(0);
  const lifecycleExitFlushedRef = useRef<boolean>(false);
  const visitedIndicesRef = useRef<Set<number>>(new Set());
  const highlightTimerRef = useRef<number | null>(null);
  const settingsDialogRef = useRef<HTMLDialogElement | null>(null);
  const rewardDialogRef = useRef<HTMLDialogElement | null>(null);

  // 防刷读追踪：累计每页的真实前台活跃停留时长与整篇互动次数
  const pageActiveMsRef = useRef<Record<number, number>>({});
  const interactionCountRef = useRef<number>(0);

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

  const recordCurrentPageActiveDuration = () => {
    const nowPerf = performance.now();
    const ongoingHiddenMs =
      hiddenStartedAtRef.current !== null ? Math.max(0, nowPerf - hiddenStartedAtRef.current) : 0;
    const rawElapsed = Math.max(0, nowPerf - enteredAtPerfRef.current);
    const activeMs = Math.max(0, Math.round(rawElapsed - pageBgHiddenMsRef.current - ongoingHiddenMs));
    pageActiveMsRef.current[sentenceIndex] =
      (pageActiveMsRef.current[sentenceIndex] || 0) + activeMs;
  };

  const flushCurrentSentenceView = async (
    nextIndex: number,
    status: 'active' | 'completed' | 'abandoned' = 'active',
    endReason = '',
    keepalive = false
  ) => {
    if (!currentSentence) return;
    const nowPerf = performance.now();
    const ongoingHiddenMs =
      hiddenStartedAtRef.current !== null ? Math.max(0, nowPerf - hiddenStartedAtRef.current) : 0;
    const totalElapsedMs = Math.max(
      0,
      Math.round(nowPerf - enteredAtPerfRef.current - ongoingHiddenMs)
    );
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
    const dialog = rewardDialogRef.current;
    if (!dialog) return;
    if (playingRewardAnim) {
      if (!dialog.open && typeof dialog.showModal === 'function') {
        dialog.showModal();
      }
    } else if (dialog.open) {
      dialog.close();
    }
  }, [playingRewardAnim]);

  useEffect(() => {
    lifecycleExitFlushedRef.current = false;
    viewIdRef.current = makeUUID('sv');
    enteredAtIsoRef.current = new Date().toISOString();
    enteredAtPerfRef.current = performance.now();
    pageBgHiddenMsRef.current = 0;
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
        pageBgHiddenMsRef.current += bgMs;
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

  // 全屏看图支持 Esc 键退出
  useEffect(() => {
    if (!isImageFullscreen) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setIsImageFullscreen(false);
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [isImageFullscreen]);

  const handleToggleFullscreen = (nextState?: boolean) => {
    setIsImageFullscreen((prev) => (typeof nextState === 'boolean' ? nextState : !prev));
  };

  const handleTokenClick = async (token: SentenceToken) => {
    interactionCountRef.current += 1;
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
    interactionCountRef.current += 1;
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
    recordCurrentPageActiveDuration();
    const prevIdx = sentenceIndex - 1;
    await flushCurrentSentenceView(prevIdx, 'active');
    visitedIndicesRef.current.add(sentenceIndex);
    setSentenceIndex(prevIdx);
  };

  const handleNextSentence = async () => {
    recordCurrentPageActiveDuration();
    if (sentenceIndex < story.sentences.length - 1) {
      const nextIdx = sentenceIndex + 1;
      await flushCurrentSentenceView(nextIdx, 'active');
      visitedIndicesRef.current.add(sentenceIndex);
      setSentenceIndex(nextIdx);
    } else {
      lifecycleExitFlushedRef.current = true;
      await flushCurrentSentenceView(sentenceIndex, 'completed', 'finished', true);

      // 1 对 1 防刷校验：仅当本篇故事配置了专属动画时才触发解锁
      const pageCharCounts: Record<number, number> = {};
      story.sentences.forEach((s, idx) => {
        pageCharCounts[idx] = Array.from(s.text || '').length;
      });
      const outcome = evaluateAndUnlockStoryAnimation({
        childId: session.child_id,
        storyId: story.story_id,
        storyTitle: story.title,
        animationUrl: resolveStoryAnimationUrl(story),
        posterUrl: story.cover_url,
        totalSentences: story.sentences.length,
        pageActiveMs: pageActiveMsRef.current,
        pageCharCounts,
        interactionCount: interactionCountRef.current
      });
      setUnlockOutcome(outcome);
      setIsFinishedCelebration(true);
    }
  };

  const handleSaveAndExit = async () => {
    recordCurrentPageActiveDuration();
    lifecycleExitFlushedRef.current = true;
    await flushCurrentSentenceView(sentenceIndex, 'active', 'user_exit');
    onExit();
  };

  if (!currentSentence) {
    return null;
  }

  const bgImageUrl = currentSentence.image_url || story.cover_url || '';

  return (
    <div
      class="reader-stage-immersive"
      data-image-only={isImageFullscreen ? 'true' : 'false'}
    >
      <div
        class="reader-ambient-bg"
        style={bgImageUrl ? { backgroundImage: `url("${bgImageUrl}")` } : undefined}
        aria-hidden="true"
      />
      <div class="reader-ambient-scrim" aria-hidden="true" />

      <div
        key={currentSentence.id}
        class="reader-hero-canvas"
        onClick={() => {
          if (bgImageUrl) {
            handleToggleFullscreen();
          }
        }}
        role={bgImageUrl ? 'button' : undefined}
        tabIndex={bgImageUrl ? 0 : undefined}
        aria-label={isImageFullscreen ? '点击退出全屏看图' : '点击插画进入全屏看图'}
        onKeyDown={(e) => {
          if ((e.key === 'Enter' || e.key === ' ') && bgImageUrl) {
            e.preventDefault();
            handleToggleFullscreen();
          }
        }}
      >
        {bgImageUrl ? (
          <img
            src={bgImageUrl}
            alt={story.title}
            class="reader-hero-img"
            fetchpriority="high"
          />
        ) : (
          <div class="story-image-placeholder">
            <span>插图（第 {sentenceIndex + 1} 页 · {story.title}）</span>
          </div>
        )}
      </div>

      <div class="reader-stage">
        <header class="reader-topbar-glass">
          <button
            type="button"
            class="reader-pill-btn"
            onClick={handleSaveAndExit}
            aria-label="保存并返回书架"
          >
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.3" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d="M9 14L4 9l5-5" />
              <path d="M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5v0a5.5 5.5 0 0 1-5.5 5.5H11" />
            </svg>
            <span>退出</span>
          </button>

          <div class="reader-topbar-center">
            <h1 class="reader-story-title">{story.title}</h1>
            <div
              class="reader-progress-track"
              role="progressbar"
              aria-valuenow={sentenceIndex + 1}
              aria-valuemin={1}
              aria-valuemax={totalSentences}
            >
              <div class="reader-progress-fill" style={{ width: `${progressPct}%` }} />
            </div>
          </div>

          <button
            type="button"
            class="reader-icon-btn"
            onClick={() => setShowSettingsSheet(true)}
            aria-label="阅读设置"
            title="阅读设置"
          >
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <circle cx="12" cy="12" r="3" />
              <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z" />
            </svg>
          </button>
        </header>

        <div
          class="reader-visual-spacer"
          onClick={() => {
            if (bgImageUrl) handleToggleFullscreen(true);
          }}
        />

        <section class="reader-floating-dock" aria-label="点读与翻页控制区">
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

          <footer class="reader-dock-controls">
            <span class="reader-page-pill" aria-label={`第 ${sentenceIndex + 1} 页，共 ${totalSentences} 页`}>
              {sentenceIndex + 1} / {totalSentences}
            </span>

            <div class="reader-nav-cluster" role="group" aria-label="翻页与整句朗读">
              <button
                type="button"
                class="cluster-btn"
                disabled={sentenceIndex === 0}
                onClick={handlePrevSentence}
                aria-label="上一页"
                title="上一页"
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
                  <path d="M15.5 19l-7-7 7-7v14z" />
                </svg>
              </button>

              <button
                type="button"
                class={`cluster-btn cluster-btn-audio ${playingAudio ? 'is-playing' : ''}`}
                onClick={handlePlayWholeSentence}
                disabled={playingAudio}
                aria-label={playingAudio ? '正在朗读整句' : '听整句朗读'}
                title="听整句朗读"
              >
                <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                  <polygon points="11 5 6 9 2 9 2 15 6 15 11 19 11 5" fill="currentColor" />
                  <path d="M15.54 8.46a5 5 0 0 1 0 7.07" />
                  <path d="M19.07 4.93a10 10 0 0 1 0 14.14" />
                </svg>
              </button>

              <button
                type="button"
                class="cluster-btn"
                onClick={handleNextSentence}
                aria-label={sentenceIndex < story.sentences.length - 1 ? '下一页' : '完成阅读'}
                title={sentenceIndex < story.sentences.length - 1 ? '下一页' : '完成阅读'}
              >
                <svg width="18" height="18" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
                  <path d="M8.5 5l7 7-7 7V5z" />
                </svg>
              </button>
            </div>

            <button
              type="button"
              class="reader-fullscreen-btn"
              onClick={handleNextSentence}
              aria-label={sentenceIndex < story.sentences.length - 1 ? '下一页' : '完成阅读'}
              title={sentenceIndex < story.sentences.length - 1 ? '下一页' : '完成阅读'}
            >
              <span class="fullscreen-btn-label">
                {sentenceIndex < story.sentences.length - 1 ? '下一页' : '完成阅读'}
              </span>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="M9 18l6-6-6-6" />
              </svg>
            </button>
          </footer>
        </section>
      </div>

      <button
        type="button"
        class="reader-exit-fullscreen-pill"
        aria-hidden={!isImageFullscreen}
        tabIndex={isImageFullscreen ? 0 : -1}
        onClick={() => handleToggleFullscreen(false)}
      >
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
          <path d="M4 14h6v6" />
          <path d="M20 10h-6V4" />
          <path d="M14 10l7-7" />
          <path d="M3 21l7-7" />
        </svg>
        <span>显示文字</span>
      </button>

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

      {/* 读完本篇庆祝激励弹窗（1 对 1 专属动画；无专属动画时静默隐藏） */}
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

            {unlockOutcome && unlockOutcome.reason !== 'no_animation' && (
              <div
                class={`reward-unlock-banner ${
                  unlockOutcome.qualified ? 'is-unlocked' : 'is-reminder'
                }`}
              >
                <p class="reward-unlock-msg">{unlockOutcome.message}</p>
                {unlockOutcome.qualified && unlockOutcome.animation && (
                  <button
                    type="button"
                    class="btn-reward-play"
                    onClick={() => setPlayingRewardAnim(unlockOutcome.animation)}
                  >
                    🎬 {unlockOutcome.newlyUnlocked ? '播放本篇专属动画' : '重温本篇专属动画'}
                  </button>
                )}
              </div>
            )}

            <div class="celebration-actions">
              <button
                type="button"
                class="btn-page-prev"
                onClick={() => {
                  setIsFinishedCelebration(false);
                  setUnlockOutcome(null);
                  pageActiveMsRef.current = {};
                  interactionCountRef.current = 0;
                  visitedIndicesRef.current = new Set();
                  enteredAtPerfRef.current = performance.now();
                  enteredAtIsoRef.current = new Date().toISOString();
                  pageBgHiddenMsRef.current = 0;
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

      {/* 原生 HTML <dialog> 本篇专属动画播放器 */}
      <dialog
        ref={rewardDialogRef}
        class="reward-player-dialog"
        onClick={(e) => {
          if (e.target === rewardDialogRef.current) {
            setPlayingRewardAnim(null);
          }
        }}
        onCancel={() => setPlayingRewardAnim(null)}
      >
        {playingRewardAnim && (
          <div class="reward-player-sheet">
            <div class="settings-sheet-header">
              <strong>🎬 {playingRewardAnim.title}</strong>
              <button
                type="button"
                class="reader-icon-btn"
                onClick={() => setPlayingRewardAnim(null)}
                aria-label="关闭动画播放"
              >
                ✕
              </button>
            </div>

            {playingRewardAnim.media_type === 'image' ? (
              <img
                src={playingRewardAnim.url}
                alt={playingRewardAnim.title}
                class="reward-player-media"
                onError={() => setPlayingRewardAnim(null)}
              />
            ) : (
              <video
                src={playingRewardAnim.url}
                poster={playingRewardAnim.poster}
                class="reward-player-media"
                controls
                autoPlay
                playsInline
                onError={() => setPlayingRewardAnim(null)}
              />
            )}

            <button
              type="button"
              class="btn-page-next"
              style={{ width: '100%' }}
              onClick={() => setPlayingRewardAnim(null)}
            >
              看完啦，返回
            </button>
          </div>
        )}
      </dialog>
    </div>
  );
}
