import { render } from 'preact';
import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import {
  apiClient,
  ChildProfile,
  ReadingSession,
  StorySummary,
  StoryVersionDetail
} from './api/client';
import { GuardianPage } from './pages/GuardianPage';
import { ReaderPage } from './pages/ReaderPage';
import { eventQueue } from './queue/offlineQueue';
import {
  buildStoryRewardAnimation,
  getUnlockedStoryIdSet,
  sortStoriesNewestFirst,
  StoryRewardAnimation
} from './utils/rewardAnimation';
import './styles.css';
import './reader-theme.css';
import './admin-theme.css';

export function App() {
  const [children, setChildren] = useState<ChildProfile[]>([]);
  const [activeChild, setActiveChild] = useState<ChildProfile | null>(null);
  const [readingMode, setReadingMode] = useState<'solo' | 'accompanied'>('solo');
  const [showPinyinHint, setShowPinyinHint] = useState<boolean>(false);
  const [stories, setStories] = useState<StorySummary[]>([]);
  const [activeSession, setActiveSession] = useState<ReadingSession | null>(null);
  const [activeStoryDetail, setActiveStoryDetail] = useState<StoryVersionDetail | null>(null);
  const [showGuardian, setShowGuardian] = useState<boolean>(false);
  const [pendingQueueCount, setPendingQueueCount] = useState<number>(0);
  const [newChildName, setNewChildName] = useState<string>('');
  const [errorMsg, setErrorMsg] = useState<string>('');
  const [openingStoryId, setOpeningStoryId] = useState<string | null>(null);
  const [failedCoverIds, setFailedCoverIds] = useState<Record<string, boolean>>({});

  // 1 对 1 专属动画播放弹窗（仅在故事卡片上对该篇已解锁动画展示入口）
  const [playingStoryAnim, setPlayingStoryAnim] = useState<StoryRewardAnimation | null>(null);
  const animDialogRef = useRef<HTMLDialogElement | null>(null);

  const sortedStories = useMemo(() => sortStoriesNewestFirst(stories), [stories]);
  const unlockedStoryIds = useMemo(
    () => getUnlockedStoryIdSet(activeChild?.id),
    [activeChild?.id, stories, activeSession]
  );

  useEffect(() => {
    document.body.dataset.mode = showGuardian ? 'admin' : 'reader';
  }, [showGuardian]);

  useEffect(() => {
    const dialog = animDialogRef.current;
    if (!dialog) return;
    if (playingStoryAnim) {
      if (!dialog.open && typeof dialog.showModal === 'function') {
        dialog.showModal();
      }
    } else if (dialog.open) {
      dialog.close();
    }
  }, [playingStoryAnim]);

  const loadInitialData = async (selectedChildId?: string) => {
    try {
      setErrorMsg('');
      let kids = await apiClient.listChildren();
      if (kids.length === 0) {
        const defaultChild = await apiClient.createChild({
          name: '六六',
          avatar: '🐻',
          learning_stage: '学龄前/一年级'
        });
        kids = [defaultChild];
      }
      setChildren(kids);
      const current = kids.find((k) => k.id === selectedChildId) || kids[0] || null;
      setActiveChild(current);

      const storyList = await apiClient.listStories(current?.id);
      setStories(sortStoriesNewestFirst(storyList));
    } catch (err: any) {
      setErrorMsg(`无法加载书架数据：${err.message || String(err)}`);
    }
  };

  useEffect(() => {
    eventQueue.startAutoFlush(4000);
    const unsub = eventQueue.onPendingChange(setPendingQueueCount);
    void loadInitialData();
    return unsub;
  }, []);

  useEffect(() => {
    if (activeChild) {
      void apiClient
        .listStories(activeChild.id)
        .then((list) => setStories(sortStoriesNewestFirst(list)))
        .catch(() => {});
    }
  }, [activeChild?.id]);

  const handleCreateChild = async (e: Event) => {
    e.preventDefault();
    if (!newChildName.trim()) return;
    try {
      const created = await apiClient.createChild({
        name: newChildName.trim(),
        avatar: '🐼',
        learning_stage: '学龄前/一年级'
      });
      setNewChildName('');
      await loadInitialData(created.id);
    } catch (err: any) {
      setErrorMsg(`创建儿童档案失败：${err.message || String(err)}`);
    }
  };

  const handleOpenStory = async (story: StorySummary, forceNew = false) => {
    try {
      setErrorMsg('');
      setOpeningStoryId(story.id);
      let readerChild = activeChild;
      if (!readerChild) {
        readerChild = await apiClient.createChild({
          name: '六六',
          avatar: '🐻',
          learning_stage: '学龄前/一年级'
        });
        setChildren((prev) => [...prev, readerChild!]);
        setActiveChild(readerChild);
      }

      const detail = await apiClient.getStoryVersion(story.id, story.latest_version_id);
      const sess = await apiClient.startOrResumeSession({
        child_id: readerChild.id,
        story_version_id: story.latest_version_id,
        mode: readingMode,
        pinyin_hint_enabled: showPinyinHint,
        force_new: forceNew
      });
      setActiveStoryDetail(detail);
      setActiveSession(sess);
    } catch (err: any) {
      setErrorMsg(`打开故事失败：${err.message || String(err)}`);
    } finally {
      setOpeningStoryId(null);
    }
  };

  if (showGuardian) {
    return (
      <GuardianPage
        activeChild={activeChild}
        onClose={() => {
          setShowGuardian(false);
          void loadInitialData(activeChild?.id);
        }}
        onChildDeleted={() => {
          setShowGuardian(false);
          void loadInitialData();
        }}
      />
    );
  }

  if (activeSession && activeStoryDetail) {
    return (
      <ReaderPage
        session={activeSession}
        story={activeStoryDetail}
        showPinyinHint={showPinyinHint}
        onExit={() => {
          setActiveSession(null);
          setActiveStoryDetail(null);
          void loadInitialData(activeChild?.id);
        }}
      />
    );
  }

  return (
    <div class="app-shell">
      <header class="app-header">
        <div class="brand">
          <span class="brand-logo">📖</span>
          <div>
            <h1>奇妙中文阅读屋</h1>
            <p class="subtitle">点字听音 · 趣味故事 · 自适应伴读</p>
          </div>
        </div>

        <div class="header-controls">
          <div class="queue-indicator" title="IndexedDB 离线事件队列">
            {pendingQueueCount > 0 ? `⏳ 待同步事件: ${pendingQueueCount}` : '✅ 数据已同步'}
          </div>
          <button
            type="button"
            class="btn-secondary"
            onClick={() => setShowGuardian(true)}
          >
            监护人工作台
          </button>
        </div>
      </header>

      {errorMsg && (
        <div class="guardian-alert guardian-alert-error" role="alert" style={{ marginBottom: '16px' }}>
          <span>⚠️ {errorMsg}</span>
          <button type="button" class="alert-close-btn" onClick={() => setErrorMsg('')}>✕</button>
        </div>
      )}

      <section class="profile-bar" aria-label="读者与阅读设置">
        <div class="child-switcher">
          {children.map((kid) => (
            <button
              key={kid.id}
              type="button"
              class={`child-chip ${activeChild?.id === kid.id ? 'active' : ''}`}
              onClick={() => setActiveChild(kid)}
            >
              <span>{kid.avatar || '🐻'}</span>
              <span>{kid.name}</span>
            </button>
          ))}

          <form class="inline-add-child" onSubmit={handleCreateChild}>
            <input
              type="text"
              class="input-compact"
              placeholder="新增小读者..."
              aria-label="新增小读者姓名"
              value={newChildName}
              onInput={(e) => setNewChildName((e.target as HTMLInputElement).value)}
            />
            <button type="submit" class="btn-small">添加</button>
          </form>
        </div>

        <div class="mode-toggles">
          <button
            type="button"
            class={`mode-btn ${readingMode === 'solo' ? 'active' : ''}`}
            onClick={() => setReadingMode('solo')}
          >
            🧒 独立阅读
          </button>
          <button
            type="button"
            class={`mode-btn ${readingMode === 'accompanied' ? 'active' : ''}`}
            onClick={() => setReadingMode('accompanied')}
          >
            👨‍👧 家长陪读
          </button>
          <button
            type="button"
            class={`mode-btn ${showPinyinHint ? 'active' : ''}`}
            onClick={() => setShowPinyinHint((v) => !v)}
          >
            拼音提示：{showPinyinHint ? '开' : '关'}
          </button>
        </div>
      </section>

      <main class="story-grid" aria-label="故事书架（新书排最上面）">
        {sortedStories.map((st, idx) => {
          const isOpening = openingStoryId === st.id;
          const hasResume = (st.resume_sentence_index || 0) > 0 && st.resume_sentence_index < st.sentence_count;
          const storyAnim = buildStoryRewardAnimation(st as any);
          const isAnimUnlocked = Boolean(storyAnim && unlockedStoryIds.has(st.id));
          const hasCover = Boolean(st.cover_url && !failedCoverIds[st.id]);

          return (
            <article key={st.id} class="story-card">
              <button
                type="button"
                class="story-cover-btn"
                disabled={isOpening}
                onClick={() => void handleOpenStory(st, false)}
                aria-label={`打开《${st.title}》`}
              >
                <div class="story-cover-frame">
                  {hasCover ? (
                    <img
                      src={st.cover_url}
                      alt={`《${st.title}》封面`}
                      class="story-cover-img"
                      loading={idx < 2 ? 'eager' : 'lazy'}
                      decoding={idx < 2 ? 'sync' : 'async'}
                      fetchpriority={idx === 0 ? 'high' : 'auto'}
                      onError={() =>
                        setFailedCoverIds((prev) => (prev[st.id] ? prev : { ...prev, [st.id]: true }))
                      }
                    />
                  ) : (
                    <div class="story-cover-placeholder" aria-hidden="true">
                      <span class="story-cover-placeholder-icon">📖</span>
                      <span class="story-cover-placeholder-title">{st.title}</span>
                    </div>
                  )}
                  <div class="story-cover-badges">
                    <span class="story-card-badge">{st.theme || '趣味故事'}</span>
                    {idx === 0 && <span class="story-new-badge">新书</span>}
                  </div>
                </div>
              </button>

              <div class="story-card-body">
                <h2 class="story-card-title">{st.title}</h2>
                {st.description && (
                  <p class="story-card-desc">{st.description}</p>
                )}
                <div class="story-meta">
                  <span>共 {st.sentence_count} 页 · v{st.version_number}</span>
                  {st.review_char_count > 0 && (
                    <span class="recommend-tag">含 {st.review_char_count} 个复习字</span>
                  )}
                </div>
                <div class="story-actions">
                  <button
                    type="button"
                    class="btn-primary"
                    disabled={isOpening}
                    onClick={() => void handleOpenStory(st, false)}
                  >
                    {isOpening ? '打开中...' : hasResume ? `继续读（第 ${st.resume_sentence_index + 1} 页）` : '开始阅读'}
                  </button>
                  {hasResume && (
                    <button
                      type="button"
                      class="btn-secondary"
                      disabled={isOpening}
                      onClick={() => void handleOpenStory(st, true)}
                    >
                      从头读
                    </button>
                  )}
                  {isAnimUnlocked && storyAnim && (
                    <button
                      type="button"
                      class="btn-secondary"
                      onClick={() => setPlayingStoryAnim(storyAnim)}
                    >
                      🎬 看动画
                    </button>
                  )}
                </div>
              </div>
            </article>
          );
        })}
      </main>

      {/* 原生 <dialog>：单篇故事 1 对 1 专属动画播放器 */}
      <dialog
        ref={animDialogRef}
        class="reward-player-dialog"
        onClick={(e) => {
          if (e.target === animDialogRef.current) {
            setPlayingStoryAnim(null);
          }
        }}
        onCancel={() => setPlayingStoryAnim(null)}
      >
        {playingStoryAnim && (
          <div class="reward-player-sheet">
            <div class="settings-sheet-header">
              <strong>🎬 {playingStoryAnim.title}</strong>
              <button
                type="button"
                class="reader-icon-btn"
                onClick={() => setPlayingStoryAnim(null)}
                aria-label="关闭动画播放"
              >
                ✕
              </button>
            </div>

            {playingStoryAnim.media_type === 'image' ? (
              <img
                src={playingStoryAnim.url}
                alt={playingStoryAnim.title}
                class="reward-player-media"
                onError={() => setPlayingStoryAnim(null)}
              />
            ) : (
              <video
                src={playingStoryAnim.url}
                poster={playingStoryAnim.poster}
                class="reward-player-media"
                controls
                autoPlay
                playsInline
                onError={() => setPlayingStoryAnim(null)}
              />
            )}

            <button
              type="button"
              class="btn-page-next"
              style={{ width: '100%' }}
              onClick={() => setPlayingStoryAnim(null)}
            >
              看完啦，返回书架
            </button>
          </div>
        )}
      </dialog>
    </div>
  );
}

const rootEl = document.getElementById('app') || document.body.firstElementChild;
if (rootEl) {
  render(<App />, rootEl);
}
