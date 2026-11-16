import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { apiClient, ChildProfile, SentenceToken, StorySummary } from '../api/client';
import { audioPlayer } from '../audio/player';
import { SentenceDisplay } from '../components/SentenceDisplay';

interface GuardianPageProps {
  activeChild: ChildProfile | null;
  onClose: () => void;
  onChildDeleted: () => void;
}

interface PlanTarget {
  character: string;
  recommended_words?: string[];
  reason?: string;
}

interface ChildStoryPlan {
  review_targets?: PlanTarget[];
  observe_targets?: PlanTarget[];
  new_candidates?: PlanTarget[];
  story_generation_prompt?: string;
}

interface PageImageSlot {
  filename: string;
  previewUrl: string;
  base64Data: string;
}

const PRESET_THEMES = [
  '勇气与探索（森林过河）',
  '方块矿洞与红石机关',
  '奇妙自然与小动物',
  '机械工坊与小小发明家',
  '星空夜晚与睡前秘密',
  '生活小侦探与日常推理'
];

const POLYPHONIC_CHARS = new Set([
  '长', '着', '行', '重', '觉', '得', '乐', '数', '只', '发',
  '地', '还', '看', '好', '空', '背', '转', '没', '为', '种',
  '干', '分', '少', '都', '难', '教', '切', '落', '漂', '结'
]);

const PUNCTUATION = new Set(['，', '。', '！', '？', '、', '：', '；', '“', '”', '‘', '’', '（', '）']);

function splitSentencesClient(raw: string): string[] {
  const lines = raw
    .split(/\r?\n/)
    .map((l) => l.trim())
    .filter(Boolean);
  const result: string[] = [];
  for (const line of lines) {
    const parts = line
      .split(/(?<=[。！？!?])/)
      .map((s) => s.trim())
      .filter(Boolean);
    if (parts.length > 0) {
      result.push(...parts);
    }
  }
  return result;
}

function buildPreviewTokens(sentence: string, pageIndex: number): SentenceToken[] {
  const chars = Array.from(sentence);
  let wordIndex = 0;
  return chars.map((ch, idx) => {
    const isPunct = PUNCTUATION.has(ch);
    if (isPunct && idx > 0) {
      wordIndex += 1;
    }
    return {
      id: `preview_p${pageIndex}_c${idx}`,
      token_index: idx,
      character: ch,
      pinyin: isPunct ? '' : POLYPHONIC_CHARS.has(ch) ? '多音待校' : '自动注音',
      word: ch,
      word_index: wordIndex,
      audio_url: '',
      is_polyphone: !isPunct && POLYPHONIC_CHARS.has(ch)
    };
  });
}

function fileToBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const res = String(reader.result || '');
      const commaIdx = res.indexOf(',');
      resolve(commaIdx >= 0 ? res.slice(commaIdx + 1) : res);
    };
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  });
}

export function GuardianPage({ activeChild, onClose, onChildDeleted }: GuardianPageProps) {
  const [pin, setPin] = useState<string>(() => sessionStorage.getItem('guardian_pin') || '1234');
  const [activeTab, setActiveTab] = useState<'quick' | 'autogen' | 'library'>('quick');

  // 自适应识字计划
  const [plan, setPlan] = useState<ChildStoryPlan | null>(null);
  const [copiedPrompt, setCopiedPrompt] = useState<boolean>(false);

  // 一键纯净正文导入与逐句校验状态
  const [title, setTitle] = useState<string>('');
  const [theme, setTheme] = useState<string>(PRESET_THEMES[0]);
  const [description, setDescription] = useState<string>('');
  const [rawText, setRawText] = useState<string>('');
  const [pageImages, setPageImages] = useState<Record<number, PageImageSlot>>({});

  // 右侧实时阅读器模拟器状态
  const [previewPageIndex, setPreviewPageIndex] = useState<number>(0);
  const [previewOrientation, setPreviewOrientation] = useState<'landscape' | 'portrait'>('landscape');
  const [previewShowPinyin, setPreviewShowPinyin] = useState<boolean>(true);
  const [previewActiveTokenId, setPreviewActiveTokenId] = useState<string | null>(null);

  // 全自动生成配置
  const [autoTheme, setAutoTheme] = useState<string>(PRESET_THEMES[1]);
  const [llmApiUrl, setLlmApiUrl] = useState<string>('');
  const [llmApiKey, setLlmApiKey] = useState<string>('');
  const [llmModel, setLlmModel] = useState<string>('');
  const [imageApiUrl, setImageApiUrl] = useState<string>('');
  const [imageApiKey, setImageApiKey] = useState<string>('');
  const [imageModel, setImageModel] = useState<string>('');
  const [showApiConfig, setShowApiConfig] = useState<boolean>(false);

  // 书架管理与状态提示
  const [stories, setStories] = useState<StorySummary[]>([]);
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [statusMsg, setStatusMsg] = useState<string>('');
  const [errorMsg, setErrorMsg] = useState<string>('');

  const batchImgInputRef = useRef<HTMLInputElement | null>(null);
  const zipInputRef = useRef<HTMLInputElement | null>(null);

  const sentences = useMemo(() => splitSentencesClient(rawText), [rawText]);

  const reviewCharSet = useMemo(() => {
    const s = new Set<string>();
    plan?.review_targets?.forEach((t) => s.add(t.character));
    plan?.new_candidates?.forEach((t) => s.add(t.character));
    return s;
  }, [plan]);

  useEffect(() => {
    sessionStorage.setItem('guardian_pin', pin);
  }, [pin]);

  useEffect(() => {
    if (previewPageIndex >= sentences.length && sentences.length > 0) {
      setPreviewPageIndex(sentences.length - 1);
    }
  }, [sentences.length, previewPageIndex]);

  const loadGuardianData = async () => {
    try {
      const list = await apiClient.listStories(activeChild?.id);
      setStories(list);
      if (activeChild) {
        const res = await fetch(`/api/v1/children/${encodeURIComponent(activeChild.id)}/story-plan`);
        if (res.ok) {
          const data = await res.json();
          setPlan(data);
        }
      }
    } catch {
      // 忽略次要加载错误，保留手动导入可用
    }
  };

  useEffect(() => {
    void loadGuardianData();
  }, [activeChild?.id]);

  // 计算顶部微型流水线进度状态
  const pipelineStates = useMemo(() => {
    const hasText = sentences.length > 0;
    const hasImages = Object.keys(pageImages).length > 0;
    return {
      step1: hasText ? 'done' : 'active',
      step2: hasText ? 'done' : 'idle',
      step3: hasImages ? 'done' : hasText ? 'active' : 'idle',
      step4: submitting ? 'active' : 'idle'
    };
  }, [sentences.length, pageImages, submitting]);

  const handleCopyPrompt = async () => {
    if (!plan?.story_generation_prompt) return;
    try {
      await navigator.clipboard.writeText(plan.story_generation_prompt);
      setCopiedPrompt(true);
      setTimeout(() => setCopiedPrompt(false), 2000);
    } catch {
      setErrorMsg('剪贴板复制失败，请手动全选文本框内容复制。');
    }
  };

  const handleSingleImageUpload = async (pageIdx: number, file?: File) => {
    if (!file) return;
    const base64Data = await fileToBase64(file);
    const previewUrl = URL.createObjectURL(file);
    setPageImages((prev) => ({
      ...prev,
      [pageIdx]: {
        filename: file.name,
        previewUrl,
        base64Data
      }
    }));
    setPreviewPageIndex(pageIdx);
  };

  const handleBatchImageUpload = async (fileList: FileList | null) => {
    if (!fileList || fileList.length === 0) return;
    const sortedFiles = Array.from(fileList).sort((a, b) =>
      a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' })
    );
    const updates: Record<number, PageImageSlot> = {};
    for (let i = 0; i < sortedFiles.length; i++) {
      const f = sortedFiles[i];
      const match = f.name.match(/(\d+)/);
      const targetIdx = match ? Math.max(0, parseInt(match[1], 10) - 1) : i;
      const base64Data = await fileToBase64(f);
      updates[targetIdx] = {
        filename: f.name,
        previewUrl: URL.createObjectURL(f),
        base64Data
      };
    }
    setPageImages((prev) => ({ ...prev, ...updates }));
    setStatusMsg(`已按序号自动绑定 ${sortedFiles.length} 张配图到对应句序槽位。`);
  };

  const handlePublishQuickStory = async () => {
    setErrorMsg('');
    setStatusMsg('');
    if (!title.trim() || sentences.length === 0) {
      setErrorMsg('请先填写故事标题，并粘贴至少一句故事正文。');
      return;
    }

    setSubmitting(true);
    try {
      const customImages = Object.entries(pageImages)
        .map(([idxStr, slot]) => ({
          page_index: Number(idxStr),
          filename: slot.filename,
          base64_data: slot.base64Data
        }))
        .sort((a, b) => a.page_index - b.page_index);

      const res = await fetch('/api/v1/admin/quick-story', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Guardian-Pin': pin
        },
        body: JSON.stringify({
          title: title.trim(),
          theme: theme.trim() || PRESET_THEMES[0],
          description: description.trim() || `共 ${sentences.length} 页单句分级读物`,
          raw_text: sentences.join('\n'),
          custom_images: customImages,
          image_api_url: imageApiUrl.trim(),
          image_api_key: imageApiKey.trim(),
          image_model: imageModel.trim()
        })
      });

      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || `HTTP ${res.status}`);
      }

      setStatusMsg(
        `✅ 《${title.trim()}》已成功上架（共 ${data.sentence_count || sentences.length} 页），后台已自动完成分词注音与点读音频预缓存！`
      );
      await loadGuardianData();
    } catch (err: any) {
      setErrorMsg(`发布失败：${err.message || String(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  const handleAutoGenerate = async () => {
    setErrorMsg('');
    setStatusMsg('');
    setSubmitting(true);
    try {
      const res = await fetch('/api/v1/admin/auto-generate-story', {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Guardian-Pin': pin
        },
        body: JSON.stringify({
          child_id: activeChild?.id || '',
          theme: autoTheme,
          auto_publish: true,
          llm_api_url: llmApiUrl.trim(),
          llm_api_key: llmApiKey.trim(),
          llm_model: llmModel.trim(),
          image_api_url: imageApiUrl.trim(),
          image_api_key: imageApiKey.trim(),
          image_model: imageModel.trim()
        })
      });
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || `HTTP ${res.status}`);
      }
      setStatusMsg(`🎉 全自动故事《${data.title || '新故事'}》已生成并上架至书架！`);
      await loadGuardianData();
    } catch (err: any) {
      setErrorMsg(`自动生成失败：${err.message || String(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  const handleImportZip = async (file?: File) => {
    if (!file) return;
    setErrorMsg('');
    setStatusMsg('');
    setSubmitting(true);
    try {
      const form = new FormData();
      form.append('file', file);
      const res = await fetch('/api/v1/admin/import', {
        method: 'POST',
        headers: { 'X-Guardian-Pin': pin },
        body: form
      });
      const data = await res.json();
      if (!res.ok) {
        throw new Error(data.error || `HTTP ${res.status}`);
      }
      setStatusMsg(`📦 离线故事包导入成功，共 ${data.sentence_count} 句！`);
      await loadGuardianData();
    } catch (err: any) {
      setErrorMsg(`导入失败：${err.message || String(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  const handleDeleteStory = async (story: StorySummary) => {
    if (!window.confirm(`确定要从书架下架并删除《${story.title}》吗？`)) return;
    try {
      const res = await fetch(`/api/v1/admin/stories/${encodeURIComponent(story.id)}`, {
        method: 'DELETE',
        headers: { 'X-Guardian-Pin': pin }
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        throw new Error(data.error || `HTTP ${res.status}`);
      }
      setStatusMsg(`已删除故事《${story.title}》。`);
      await loadGuardianData();
    } catch (err: any) {
      setErrorMsg(`删除失败：${err.message || String(err)}`);
    }
  };

  const handleDeleteChild = async () => {
    if (!activeChild) return;
    if (!window.confirm(`确定删除儿童档案“${activeChild.name}”及其独立学习记录吗？`)) return;
    try {
      const res = await fetch(`/api/v1/children/${encodeURIComponent(activeChild.id)}`, {
        method: 'DELETE',
        headers: { 'X-Guardian-Pin': pin }
      });
      if (!res.ok) {
        const data = await res.json().catch(() => ({}));
        throw new Error(data.error || `HTTP ${res.status}`);
      }
      onChildDeleted();
    } catch (err: any) {
      setErrorMsg(`删除档案失败：${err.message || String(err)}`);
    }
  };

  const activeSentenceText = sentences[previewPageIndex] || '请在左侧粘贴故事正文，这里将实时预览儿童平板端效果。';
  const activePreviewTokens = useMemo(
    () => buildPreviewTokens(activeSentenceText, previewPageIndex),
    [activeSentenceText, previewPageIndex]
  );
  const activePreviewImage = pageImages[previewPageIndex]?.previewUrl || '';

  return (
    <div style={{ paddingBottom: '40px' }}>
      {/* 顶部控制栏 */}
      <header
        class="guardian-header"
        style={{ maxWidth: '1440px', margin: '16px auto 0', borderRadius: 'var(--radius-card)' }}
      >
        <div style={{ display: 'flex', alignItems: 'center', gap: '12px' }}>
          <button type="button" class="btn-secondary" onClick={onClose}>
            ← 返回书架
          </button>
          <div>
            <h2 style={{ margin: 0, fontSize: '1.25rem' }}>🔒 监护人工作台</h2>
            <span style={{ fontSize: '0.85rem', color: 'var(--text-pinyin)' }}>
              当前读者：{activeChild ? `${activeChild.avatar || '🐻'} ${activeChild.name}` : '未选择'}
            </span>
          </div>
        </div>

        <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '10px' }}>
          <label style={{ fontSize: '0.9rem', fontWeight: 600 }}>
            管理 PIN：
            <input
              type="password"
              value={pin}
              onInput={(e) => setPin((e.target as HTMLInputElement).value)}
              class="input-field"
              style={{ width: '92px' }}
            />
          </label>
          <a
            href={`/api/v1/admin/export?pin=${encodeURIComponent(pin)}`}
            target="_blank"
            rel="noreferrer"
            class="btn-secondary"
            style={{ textDecoration: 'none' }}
          >
            📥 导出学习数据
          </a>
        </div>
      </header>

      {/* 全局状态通知 */}
      <div style={{ maxWidth: '1440px', margin: '12px auto 0', padding: '0 24px' }}>
        {statusMsg && <div class="status-notice">{statusMsg}</div>}
        {errorMsg && (
          <div
            class="status-notice"
            style={{ background: '#fef2f2', borderColor: '#ef4444', color: '#991b1b' }}
          >
            ⚠️ {errorMsg}
          </div>
        )}
      </div>

      {/* 左右分栏工作台（左侧编辑校验 + 右侧实时儿童阅读器模拟器） */}
      <div class="admin-workspace">
        {/* 左侧栏 */}
        <section class="admin-panel">
          {/* 微型流水线进度看板 */}
          <div class="pipeline-bar" aria-label="故事导入流水线状态">
            <span class="pipeline-step" data-state={pipelineStates.step1}>
              1. 纯净正文 ({sentences.length} 句)
            </span>
            <span>➔</span>
            <span class="pipeline-step" data-state={pipelineStates.step2}>
              2. 单句分页 & 多音字扫描
            </span>
            <span>➔</span>
            <span class="pipeline-step" data-state={pipelineStates.step3}>
              3. 配图绑定 ({Object.keys(pageImages).length}/{sentences.length || 0})
            </span>
            <span>➔</span>
            <span class="pipeline-step" data-state={pipelineStates.step4}>
              4. 一键上架
            </span>
          </div>

          {/* 功能切换标签 */}
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: '8px', marginBottom: '18px' }}>
            <button
              type="button"
              class={`mode-btn ${activeTab === 'quick' ? 'active' : ''}`}
              onClick={() => setActiveTab('quick')}
            >
              ✍️ 纯净正文一键导入 & 校对
            </button>
            <button
              type="button"
              class={`mode-btn ${activeTab === 'autogen' ? 'active' : ''}`}
              onClick={() => setActiveTab('autogen')}
            >
              ✨ 全自动 AI 生成故事
            </button>
            <button
              type="button"
              class={`mode-btn ${activeTab === 'library' ? 'active' : ''}`}
              onClick={() => setActiveTab('library')}
            >
              📚 故事包 & 档案管理 ({stories.length})
            </button>
          </div>

          {/* 艾宾浩斯待复习字提示条 */}
          {plan && (plan.review_targets?.length || plan.new_candidates?.length) ? (
            <div
              style={{
                padding: '12px 14px',
                borderRadius: '14px',
                background: 'var(--bg-canvas)',
                border: '1px solid var(--border-subtle)',
                marginBottom: '18px'
              }}
            >
              <div
                style={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  alignItems: 'center',
                  marginBottom: '8px'
                }}
              >
                <strong style={{ fontSize: '0.92rem' }}>
                  🧠 本次艾宾浩斯遗忘曲线靶向字库
                </strong>
                {plan.story_generation_prompt && (
                  <button type="button" class="btn-small" onClick={handleCopyPrompt}>
                    {copiedPrompt ? '✅ 已复制提示词' : '📋 复制 AI 改写提示词'}
                  </button>
                )}
              </div>
              <div>
                {plan.review_targets?.map((t) => (
                  <span key={t.character} class="word-badge badge-review" title={t.reason}>
                    复习：{t.character}
                  </span>
                ))}
                {plan.new_candidates?.map((t) => (
                  <span key={t.character} class="word-badge badge-new" title={t.reason}>
                    新字：{t.character}
                  </span>
                ))}
              </div>
            </div>
          ) : null}

          {activeTab === 'quick' && (
            <div>
              <div class="form-row">
                <label style={{ flex: '1 1 220px', fontWeight: 600 }}>
                  故事标题：
                  <input
                    type="text"
                    placeholder="例如：小猪鲁本找红石"
                    value={title}
                    onInput={(e) => setTitle((e.target as HTMLInputElement).value)}
                    class="input-field full-width"
                  />
                </label>
                <label style={{ flex: '1 1 220px', fontWeight: 600 }}>
                  故事主题：
                  <select
                    value={theme}
                    onChange={(e) => setTheme((e.target as HTMLSelectElement).value)}
                    class="input-field full-width"
                  >
                    {PRESET_THEMES.map((th) => (
                      <option key={th} value={th}>
                        {th}
                      </option>
                    ))}
                  </select>
                </label>
              </div>

              <div class="import-box">
                <div
                  style={{
                    display: 'flex',
                    justifyContent: 'space-between',
                    alignItems: 'center',
                    flexWrap: 'wrap',
                    gap: '8px'
                  }}
                >
                  <label style={{ fontWeight: 600 }}>
                    一键导入纯净正文（每行一句，严禁带拼音或括号注释）：
                  </label>
                  <button
                    type="button"
                    class="btn-small"
                    onClick={() => batchImgInputRef.current?.click()}
                  >
                    🖼️ 批量绑定配图 (1.png..N.png)
                  </button>
                  <input
                    ref={batchImgInputRef}
                    type="file"
                    accept="image/*"
                    multiple
                    style={{ display: 'none' }}
                    onChange={(e) =>
                      void handleBatchImageUpload((e.target as HTMLInputElement).files)
                    }
                  />
                </div>

                <textarea
                  class="story-textarea"
                  placeholder="在此粘贴由 children-story-rewriter 生成的纯净分页正文，每行 1 句话（支持 Ctrl/Cmd + Enter 快捷上架）..."
                  value={rawText}
                  onInput={(e) => setRawText((e.target as HTMLTextAreaElement).value)}
                  onKeyDown={(e) => {
                    if ((e.metaKey || e.ctrlKey) && e.key === 'Enter' && !e.isComposing) {
                      void handlePublishQuickStory();
                    }
                  }}
                />
              </div>

              {/* 逐句分页校验列表（点击任意行联动右侧模拟器） */}
              {sentences.length > 0 && (
                <div style={{ marginBottom: '20px' }}>
                  <div
                    style={{
                      display: 'flex',
                      justifyContent: 'space-between',
                      alignItems: 'center',
                      marginBottom: '10px'
                    }}
                  >
                    <strong style={{ fontSize: '0.95rem' }}>
                      逐页分词与多音字预检（点击任意页在右侧预览）
                    </strong>
                    <span style={{ fontSize: '0.82rem', color: 'var(--text-pinyin)' }}>
                      蓝底=多音字词境检查 · 绿底线=靶向复习字
                    </span>
                  </div>

                  <div class="page-inspector-list">
                    {sentences.map((sent, idx) => {
                      const chars = Array.from(sent);
                      const imgSlot = pageImages[idx];
                      return (
                        <div
                          key={idx}
                          class="page-row"
                          data-active={previewPageIndex === idx}
                          onClick={() => setPreviewPageIndex(idx)}
                        >
                          <span class="page-badge">第 {String(idx + 1).padStart(2, '0')} 页</span>

                          <div class="seg-list">
                            {chars.map((ch, cIdx) => {
                              const isPunct = PUNCTUATION.has(ch);
                              const isPoly = !isPunct && POLYPHONIC_CHARS.has(ch);
                              const isRev = !isPunct && reviewCharSet.has(ch);
                              return (
                                <span
                                  key={cIdx}
                                  class="seg-chip"
                                  data-polyphonic={isPoly}
                                  data-review={isRev}
                                  title={
                                    isPoly
                                      ? `多音字“${ch}”，导入时将按词组自动匹配读音`
                                      : isRev
                                      ? `艾宾浩斯靶向复现字“${ch}”`
                                      : undefined
                                  }
                                >
                                  <span>{ch}</span>
                                </span>
                              );
                            })}
                          </div>

                          <label
                            class="img-slot-mini"
                            title="点击上传或替换本页插画"
                            onClick={(e) => e.stopPropagation()}
                            style={{ cursor: 'pointer' }}
                          >
                            {imgSlot ? (
                              <img src={imgSlot.previewUrl} alt={`第${idx + 1}页插图`} />
                            ) : (
                              <span>+图{idx + 1}</span>
                            )}
                            <input
                              type="file"
                              accept="image/*"
                              style={{ display: 'none' }}
                              onChange={(e) =>
                                void handleSingleImageUpload(
                                  idx,
                                  (e.target as HTMLInputElement).files?.[0]
                                )
                              }
                            />
                          </label>
                        </div>
                      );
                    })}
                  </div>
                </div>
              )}

              <div class="guardian-actions">
                <button
                  type="button"
                  class="btn-primary"
                  disabled={submitting || !title.trim() || sentences.length === 0}
                  onClick={handlePublishQuickStory}
                  style={{ minHeight: '48px', padding: '0 24px', fontSize: '1rem' }}
                >
                  {submitting
                    ? '⏳ 正在分词注音并缓存音频...'
                    : `🚀 一键上架故事（共 ${sentences.length} 页）`}
                </button>
              </div>
            </div>
          )}

          {activeTab === 'autogen' && (
            <div>
              <label class="block-label">
                选择全自动故事主题：
                <select
                  value={autoTheme}
                  onChange={(e) => setAutoTheme((e.target as HTMLSelectElement).value)}
                  class="input-field full-width"
                >
                  {PRESET_THEMES.map((th) => (
                    <option key={th} value={th}>
                      {th}
                    </option>
                  ))}
                </select>
              </label>

              <div style={{ marginBottom: '14px' }}>
                <button
                  type="button"
                  class="btn-small"
                  onClick={() => setShowApiConfig((v) => !v)}
                >
                  {showApiConfig ? '收起自定义模型 API 配置' : '⚙️ 自定义大模型 / 生图 API（可选）'}
                </button>
              </div>

              {showApiConfig && (
                <div
                  style={{
                    padding: '14px',
                    borderRadius: '14px',
                    background: 'var(--bg-canvas)',
                    marginBottom: '16px'
                  }}
                >
                  <div class="form-row">
                    <input
                      type="text"
                      placeholder="LLM API URL (可选)"
                      value={llmApiUrl}
                      onInput={(e) => setLlmApiUrl((e.target as HTMLInputElement).value)}
                      class="input-field"
                      style={{ flex: 1, marginLeft: 0 }}
                    />
                    <input
                      type="password"
                      placeholder="LLM API Key"
                      value={llmApiKey}
                      onInput={(e) => setLlmApiKey((e.target as HTMLInputElement).value)}
                      class="input-field"
                      style={{ flex: 1 }}
                    />
                    <input
                      type="text"
                      placeholder="LLM Model"
                      value={llmModel}
                      onInput={(e) => setLlmModel((e.target as HTMLInputElement).value)}
                      class="input-field"
                      style={{ flex: 1 }}
                    />
                  </div>
                  <div class="form-row" style={{ marginBottom: 0 }}>
                    <input
                      type="text"
                      placeholder="生图 API URL (可选，默认内置 SVG 场景)"
                      value={imageApiUrl}
                      onInput={(e) => setImageApiUrl((e.target as HTMLInputElement).value)}
                      class="input-field"
                      style={{ flex: 1, marginLeft: 0 }}
                    />
                    <input
                      type="password"
                      placeholder="生图 API Key"
                      value={imageApiKey}
                      onInput={(e) => setImageApiKey((e.target as HTMLInputElement).value)}
                      class="input-field"
                      style={{ flex: 1 }}
                    />
                    <input
                      type="text"
                      placeholder="生图 Model"
                      value={imageModel}
                      onInput={(e) => setImageModel((e.target as HTMLInputElement).value)}
                      class="input-field"
                      style={{ flex: 1 }}
                    />
                  </div>
                </div>
              )}

              <button
                type="button"
                class="btn-primary"
                disabled={submitting}
                onClick={handleAutoGenerate}
                style={{ minHeight: '48px', padding: '0 24px', fontSize: '1rem' }}
              >
                {submitting ? '⏳ 正在生成故事与配图...' : '✨ 一键全自动生成并上架'}
              </button>
            </div>
          )}

          {activeTab === 'library' && (
            <div>
              <div style={{ marginBottom: '20px' }}>
                <button
                  type="button"
                  class="btn-secondary"
                  disabled={submitting}
                  onClick={() => zipInputRef.current?.click()}
                >
                  📦 导入离线故事包 (.zip)
                </button>
                <input
                  ref={zipInputRef}
                  type="file"
                  accept=".zip"
                  style={{ display: 'none' }}
                  onChange={(e) => void handleImportZip((e.target as HTMLInputElement).files?.[0])}
                />
              </div>

              <div class="page-inspector-list" style={{ marginBottom: '24px' }}>
                {stories.map((st) => (
                  <div key={st.id} class="page-row" style={{ cursor: 'default' }}>
                    <span class="page-badge">{st.sentence_count} 句</span>
                    <div>
                      <strong>{st.title}</strong>
                      <div style={{ fontSize: '0.82rem', color: 'var(--text-pinyin)' }}>
                        {st.theme} · {st.description}
                      </div>
                    </div>
                    <button
                      type="button"
                      class="btn-small"
                      style={{ color: '#b91c1c' }}
                      onClick={() => void handleDeleteStory(st)}
                    >
                      删除
                    </button>
                  </div>
                ))}
              </div>

              {activeChild && (
                <div
                  style={{
                    paddingTop: '16px',
                    borderTop: '1px solid var(--border-subtle)',
                    display: 'flex',
                    justifyContent: 'space-between',
                    alignItems: 'center'
                  }}
                >
                  <span style={{ fontSize: '0.9rem', color: 'var(--text-pinyin)' }}>
                    危险操作：删除当前儿童学习档案
                  </span>
                  <button
                    type="button"
                    class="btn-small"
                    style={{ color: '#b91c1c' }}
                    onClick={() => void handleDeleteChild()}
                  >
                    删除档案“{activeChild.name}”
                  </button>
                </div>
              )}
            </div>
          )}
        </section>

        {/* 右侧栏：Artifacts 式实时儿童阅读器模拟器 */}
        <aside class="preview-sticky" aria-label="儿童阅读器实时预览画布">
          <div
            class="admin-panel"
            style={{
              padding: '12px 16px',
              display: 'flex',
              flexWrap: 'wrap',
              alignItems: 'center',
              justifyContent: 'space-between',
              gap: '10px'
            }}
          >
            <strong style={{ fontSize: '0.95rem' }}>📱 儿童平板端实时预览</strong>

            <div style={{ display: 'flex', alignItems: 'center', gap: '8px', flexWrap: 'wrap' }}>
              <button
                type="button"
                class={`btn-small ${previewOrientation === 'landscape' ? 'active' : ''}`}
                onClick={() => setPreviewOrientation('landscape')}
              >
                横屏 4:3
              </button>
              <button
                type="button"
                class={`btn-small ${previewOrientation === 'portrait' ? 'active' : ''}`}
                onClick={() => setPreviewOrientation('portrait')}
              >
                竖屏 3:4
              </button>
              <label style={{ fontSize: '0.85rem', display: 'inline-flex', alignItems: 'center', gap: '4px' }}>
                <input
                  type="checkbox"
                  checked={previewShowPinyin}
                  onChange={(e) => setPreviewShowPinyin((e.target as HTMLInputElement).checked)}
                />
                拼音
              </label>
            </div>
          </div>

          <div class="preview-viewport" data-orientation={previewOrientation}>
            <div
              class="story-card"
              style={{
                width: '100%',
                height: '100%',
                display: 'flex',
                flexDirection: 'column',
                justifyContent: 'space-between',
                gap: '12px',
                padding: '16px'
              }}
            >
              <div class="illustration-box" style={{ flex: '1 1 56%', minHeight: 0 }}>
                {activePreviewImage ? (
                  <img src={activePreviewImage} alt={`第 ${previewPageIndex + 1} 页插画预览`} />
                ) : (
                  <div class="story-image-placeholder">
                    <span style={{ fontSize: '2rem' }}>🎨</span>
                    <span>
                      {title.trim() || '新故事'} · 第 {previewPageIndex + 1} 页配图槽位
                    </span>
                  </div>
                )}
              </div>

              <div style={{ '--size-hanzi': 'clamp(1.45rem, 2.2vw, 2.1rem)', '--size-pinyin': '0.78rem' } as any}>
                <SentenceDisplay
                  tokens={activePreviewTokens}
                  activeWordIndex={null}
                  activeTokenId={previewActiveTokenId}
                  showPinyinHint={previewShowPinyin}
                  onTokenClick={(tok) => {
                    setPreviewActiveTokenId(tok.id);
                    void audioPlayer.playUrlOrFallback('', tok.character);
                    window.setTimeout(() => setPreviewActiveTokenId(null), 900);
                  }}
                />
              </div>
            </div>
          </div>

          {/* 预览翻页器 */}
          <div
            class="admin-panel"
            style={{
              padding: '10px 16px',
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between'
            }}
          >
            <button
              type="button"
              class="btn-small"
              disabled={previewPageIndex <= 0}
              onClick={() => setPreviewPageIndex((i) => Math.max(0, i - 1))}
            >
              ← 上一页
            </button>
            <span style={{ fontSize: '0.9rem', fontWeight: 600 }}>
              模拟页码：{sentences.length > 0 ? `${previewPageIndex + 1} / ${sentences.length}` : '0 / 0'}
            </span>
            <button
              type="button"
              class="btn-small"
              disabled={previewPageIndex >= sentences.length - 1}
              onClick={() => setPreviewPageIndex((i) => Math.min(sentences.length - 1, i + 1))}
            >
              下一页 →
            </button>
          </div>
        </aside>
      </div>
    </div>
  );
}
