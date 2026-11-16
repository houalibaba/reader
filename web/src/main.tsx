import { render } from 'preact';
import { useEffect, useState } from 'preact/hooks';
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

  useEffect(() => {
    document.body.dataset.mode = showGuardian ? 'admin' : 'reader';
  }, [showGuardian]);

  const loadInitialData = async (selectedChildId?: string) => {
    try {
      setErrorMsg('');
      let kids = await apiClient.listChildren();
      // Auto-initialize a default child profile on fresh install so "开始阅读" works immediately
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
      setStories(storyList);
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
      void apiClient.listStories(activeChild.id).then(setStories).catch(() => {});
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
          <button type="button" class="btn-secondary" onClick={() => setShowGuardian(true)}>
            🔒 监护人管理
          </button>
        </div>
      </header>

      {errorMsg && (
        <div
          class="status-notice"
          style={{
            background: '#fef2f2',
            borderColor: '#ef4444',
            color: '#991b1b',
            marginBottom: '16px'
          }}
        >
          ⚠️ {errorMsg}
        </div>
      )}

      <section class="profile-bar">
        <div class="child-switcher">
          <span class="section-label">当前读者：</span>
          {children.map((c) => (
            <button
              key={c.id}
              type="button"
              class={`child-chip ${activeChild?.id === c.id ? 'active' : ''}`}
              onClick={() => setActiveChild(c)}
            >
              <span>{c.avatar || '🐻'}</span>
              <span>{c.name}</span>
            </button>
          ))}

          <form onSubmit={handleCreateChild} class="inline-add-child">
            <input
              type="text"
              placeholder="新增儿童昵称..."
              value={newChildName}
              onInput={(e) => setNewChildName((e.target as HTMLInputElement).value)}
              class="input-compact"
            />
            <button type="submit" class="btn-small">
              + 创建档案
            </button>
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

          <label class="pinyin-toggle">
            <input
              type="checkbox"
              checked={showPinyinHint}
              onChange={(e) => setShowPinyinHint((e.target as HTMLInputElement).checked)}
            />
            显示全句拼音提示
          </label>
        </div>
      </section>

      <main class="bookshelf">
        <h2>故事书架</h2>
        {stories.length === 0 ? (
          <div class="empty-shelf">
            <p>书架上还没有故事，请点击右上角“🔒 监护人管理”一键生成或导入故事包。</p>
          </div>
        ) : (
          <div class="story-grid">
            {stories.map((st) => (
              <article
                key={st.id}
                class="story-card"
                style={{ height: 'auto', containerType: 'normal' }}
              >
                <div class="story-card-badge">{st.theme || '童话故事'}</div>
                <h3>{st.title}</h3>
                <p class="story-desc">{st.description}</p>
                <div class="story-meta">
                  <span>共 {st.sentence_count} 句</span>
                  {st.recommended_reason && (
                    <span class="recommend-tag">✨ {st.recommended_reason}</span>
                  )}
                </div>

                <div class="story-actions">
                  {st.active_session_id ? (
                    <>
                      <button
                        type="button"
                        class="btn-primary"
                        disabled={openingStoryId === st.id}
                        onClick={() => handleOpenStory(st, false)}
                      >
                        {openingStoryId === st.id
                          ? '正在打开...'
                          : `继续读（第 ${st.resume_sentence_index + 1} 句）`}
                      </button>
                      <button
                        type="button"
                        class="btn-secondary"
                        disabled={openingStoryId === st.id}
                        onClick={() => handleOpenStory(st, true)}
                      >
                        从头读
                      </button>
                    </>
                  ) : (
                    <button
                      type="button"
                      class="btn-primary"
                      disabled={openingStoryId === st.id}
                      onClick={() => handleOpenStory(st, false)}
                    >
                      {openingStoryId === st.id ? '正在打开...' : '开始阅读'}
                    </button>
                  )}
                </div>
              </article>
            ))}
          </div>
        )}
      </main>
    </div>
  );
}

render(<App />, document.getElementById('app')!);
