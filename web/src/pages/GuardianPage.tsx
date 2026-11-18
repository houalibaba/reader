import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import {
  apiClient,
  ChildProfile,
  HealthStatusResponse,
  SentenceToken,
  StoryPlanResponse,
  StorySummary,
  UpdateCheckResponse
} from '../api/client';
import { audioPlayer } from '../audio/player';
import { SentenceDisplay } from '../components/SentenceDisplay';

interface GuardianPageProps {
  activeChild: ChildProfile | null;
  onClose: () => void;
  onChildDeleted: () => void;
}

interface PageImageSlot {
  filename: string;
  previewUrl: string;
  base64Data: string;
  groupIndex?: number;
  groupTotal?: number;
  isManualOverride?: boolean;
}

function distributeImagesEvenlyToPages(
  sourceSlots: PageImageSlot[],
  sentenceCount: number,
  existingMap: Record<number, PageImageSlot> = {}
): Record<number, PageImageSlot> {
  if (sourceSlots.length === 0) return {};
  const totalSentences = Math.max(sentenceCount, sourceSlots.length);
  const nextMap: Record<number, PageImageSlot> = {};
  for (let idx = 0; idx < totalSentences; idx++) {
    if (existingMap[idx]?.isManualOverride) {
      nextMap[idx] = existingMap[idx];
      continue;
    }
    const imgIdx = Math.min(
      sourceSlots.length - 1,
      Math.floor((idx * sourceSlots.length) / totalSentences)
    );
    const chosen = sourceSlots[imgIdx];
    nextMap[idx] = {
      ...chosen,
      groupIndex: imgIdx + 1,
      groupTotal: sourceSlots.length,
      isManualOverride: false
    };
  }
  return nextMap;
}

const PRESET_THEMES = [
  '勇气与探索（森林过河）',
  '我的世界·故事模式（方块探险）',
  '太空与星际（月亮奇遇）',
  '海洋与沙滩（寻宝之旅）',
  '恐龙与远古（高山探秘）',
  '友谊与分享（果园野餐）',
  '日常与好习惯（快乐成长）'
];

const POLYPHONIC_CHARS = new Set([
  '长', '着', '行', '重', '觉', '得', '乐', '数', '只', '发',
  '地', '还', '看', '好', '空', '背', '转', '没', '为', '种',
  '干', '分', '少', '都', '难', '教', '切', '落', '漂', '结'
]);

const PUNCTUATION = new Set(['，', '。', '！', '？', '、', '：', '；', '“', '”', '‘', '’', '（', '）']);

const PREVIEW_PINYIN_DURATION_MS = 10500;

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
      character: ch,
      pinyin: isPunct ? '' : POLYPHONIC_CHARS.has(ch) ? '多音待校' : '点字听音',
      word: ch,
      word_index: wordIndex,
      audio_url: ''
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
  const [activeTab, setActiveTab] = useState<'prompt' | 'quick' | 'autogen' | 'library' | 'system'>('prompt');

  // 自适应识字计划与 AI 提示词状态
  const [plan, setPlan] = useState<StoryPlanResponse | null>(null);
  const [loadingPlan, setLoadingPlan] = useState<boolean>(false);
  const [copiedPrompt, setCopiedPrompt] = useState<boolean>(false);

  // 一键纯净正文导入与逐句校验状态
  const [title, setTitle] = useState<string>('');
  const [theme, setTheme] = useState<string>(PRESET_THEMES[0]);
  const [description, setDescription] = useState<string>('');
  const [rawText, setRawText] = useState<string>('');
  const [batchSourceImages, setBatchSourceImages] = useState<PageImageSlot[]>([]);
  const [pageImages, setPageImages] = useState<Record<number, PageImageSlot>>({});
  const [pullingGist, setPullingGist] = useState<boolean>(false);

  // 右侧实时阅读器模拟器状态（默认不标拼音，点击才显示）
  const [previewPageIndex, setPreviewPageIndex] = useState<number>(0);
  const [previewOrientation, setPreviewOrientation] = useState<'landscape' | 'portrait'>('landscape');
  const [previewShowPinyin, setPreviewShowPinyin] = useState<boolean>(false);
  const [previewActiveTokenId, setPreviewActiveTokenId] = useState<string | null>(null);
  const previewTimerRef = useRef<number | null>(null);

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

  // 系统版本与 GitHub 在线升级状态
  const [healthInfo, setHealthInfo] = useState<HealthStatusResponse | null>(null);
  const [mirrorUrl, setMirrorUrl] = useState<string>(() => localStorage.getItem('github_mirror_url') || 'https://ghproxy.net/');
  const [updateCheckInfo, setUpdateCheckInfo] = useState<UpdateCheckResponse | null>(null);
  const [checkingUpdate, setCheckingUpdate] = useState<boolean>(false);
  const [applyingUpdate, setApplyingUpdate] = useState<boolean>(false);

  const batchImgInputRef = useRef<HTMLInputElement | null>(null);
  const zipInputRef = useRef<HTMLInputElement | null>(null);

  const sentences = useMemo(() => splitSentencesClient(rawText), [rawText]);

  const reviewCharSet = useMemo(() => {
    const s = new Set<string>();
    plan?.review_targets?.forEach((t) => s.add(t.character));
    plan?.new_candidates?.forEach((t) => s.add(t.character));
    return s;
  }, [plan]);

  const loadStories = async () => {
    try {
      const list = await apiClient.listStories(activeChild?.id);
      setStories(list);
    } catch {
      // ignore
    }
  };

  const loadStoryPlan = async (childId?: string) => {
    const targetId = childId || activeChild?.id;
    if (!targetId) return;
    setLoadingPlan(true);
    try {
      const res = await apiClient.getStoryPlan(targetId);
      setPlan(res);
    } catch (err: any) {
      setErrorMsg(`拉取自适应识字计划失败：${err.message || String(err)}`);
    } finally {
      setLoadingPlan(false);
    }
  };

  const loadHealth = async () => {
    try {
      const h = await apiClient.getHealth();
      setHealthInfo(h);
    } catch {
      // ignore
    }
  };

  useEffect(() => {
    sessionStorage.setItem('guardian_pin', pin);
  }, [pin]);

  useEffect(() => {
    localStorage.setItem('github_mirror_url', mirrorUrl);
  }, [mirrorUrl]);

  useEffect(() => {
    void loadStories();
    void loadHealth();
    if (activeChild) {
      void loadStoryPlan(activeChild.id);
    }
    return () => {
      if (previewTimerRef.current !== null) {
        window.clearTimeout(previewTimerRef.current);
      }
    };
  }, [activeChild?.id]);

  useEffect(() => {
    if (previewPageIndex >= sentences.length && sentences.length > 0) {
      setPreviewPageIndex(sentences.length - 1);
    }
  }, [sentences.length, previewPageIndex]);

  useEffect(() => {
    if (batchSourceImages.length > 0 && sentences.length > 0) {
      setPageImages((prev) => distributeImagesEvenlyToPages(batchSourceImages, sentences.length, prev));
    }
  }, [sentences.length, batchSourceImages]);

  const currentStep = useMemo(() => {
    if (statusMsg.includes('已发布') || statusMsg.includes('导入成功')) return 4;
    if (Object.keys(pageImages).length > 0) return 3;
    if (sentences.length > 0) return 2;
    return 1;
  }, [sentences.length, pageImages, statusMsg]);

  const handleCopyPrompt = async () => {
    if (!plan?.story_generation_prompt) return;
    try {
      await navigator.clipboard.writeText(plan.story_generation_prompt);
      setCopiedPrompt(true);
      setTimeout(() => setCopiedPrompt(false), 2500);
    } catch {
      setStatusMsg('提示词已选中，请长按或按 Ctrl+C 复制');
    }
  };

  const handleSinglePageImageUpload = async (pageIdx: number, file: File | undefined) => {
    if (!file) return;
    const base64Data = await fileToBase64(file);
    const previewUrl = URL.createObjectURL(file);
    setPageImages((prev) => ({
      ...prev,
      [pageIdx]: {
        filename: `s${String(pageIdx + 1).padStart(2, '0')}_${file.name}`,
        previewUrl,
        base64Data,
        isManualOverride: true
      }
    }));
  };

  const handleBatchImagesUpload = async (files: FileList | null) => {
    if (!files || files.length === 0) return;
    const sorted = Array.from(files).sort((a, b) =>
      a.name.localeCompare(b.name, undefined, { numeric: true })
    );
    const sourceSlots: PageImageSlot[] = [];
    for (let i = 0; i < sorted.length; i++) {
      const f = sorted[i];
      const base64Data = await fileToBase64(f);
      sourceSlots.push({
        filename: `ch${String(i + 1).padStart(2, '0')}_${f.name}`,
        previewUrl: URL.createObjectURL(f),
        base64Data,
        groupIndex: i + 1,
        groupTotal: sorted.length,
        isManualOverride: false
      });
    }
    setBatchSourceImages(sourceSlots);
    const totalLines = sentences.length || sourceSlots.length;
    const distributed = distributeImagesEvenlyToPages(sourceSlots, totalLines);
    setPageImages(distributed);
    const avgLines = (totalLines / sourceSlots.length).toFixed(1).replace(/\.0$/, '');
    setStatusMsg(
      `已按图片数 (${sourceSlots.length}张) 与总行数 (${totalLines}行) 平均分配绑定：每张图对应约 ${avgLines} 行故事内容`
    );
  };

  const handlePullFromGist = async (force = false) => {
    setPullingGist(true);
    setErrorMsg('');
    setStatusMsg('⏳ 正在从 GitHub Gist 零缓存拉取最新故事与配图并按行数平均分配绑定...');
    try {
      const res = await apiClient.pullStoryFromGist(pin, force);
      if (res.title) setTitle(res.title);
      if (res.theme) setTheme(res.theme);
      if (res.description) setDescription(res.description);
      if (res.raw_text) setRawText(res.raw_text);
      setPreviewPageIndex(0);

      // 始终先清空旧配图预览状态，防止已导入故事或无配图故事残留上一篇的旧图片
      setBatchSourceImages([]);
      if (res.image_urls && res.image_urls.length > 0) {
        const mappedFromGist: Record<number, PageImageSlot> = {};
        const uniqueUrls = Array.from(new Set(res.image_urls.filter(Boolean)));
        res.image_urls.forEach((url, idx) => {
          if (!url) return;
          const gIdx = uniqueUrls.indexOf(url);
          mappedFromGist[idx] = {
            filename: `gist_p${String(idx + 1).padStart(2, '0')}`,
            previewUrl: url,
            base64Data: '',
            groupIndex: gIdx >= 0 ? gIdx + 1 : 1,
            groupTotal: uniqueUrls.length
          };
        });
        setPageImages(mappedFromGist);
      } else {
        setPageImages({});
      }

      if (res.status === 'already_imported') {
        const shouldForce = window.confirm(
          `Gist 上的《${res.title}》（版本 ${res.version}）此前已在书架中。是否强制重新拉取并覆盖更新配图与版本？`
        );
        if (shouldForce) {
          await handlePullFromGist(true);
          return;
        }
        setStatusMsg(
          `ℹ️ Gist 最新故事《${res.title}》（版本 ${res.version}，共 ${res.sentence_count} 行）已在书架中，已为您加载到编辑器预览。`
        );
      } else {
        setStatusMsg(
          `🎉 从 Gist 零缓存拉取并导入成功：《${res.title}》（版本 v${res.version_number}，${res.lines_per_image}）！`
        );
        await loadStories();
      }
    } catch (err: any) {
      setErrorMsg(`从 Gist 拉取故事失败：${err.message || String(err)}`);
      setStatusMsg('');
    } finally {
      setPullingGist(false);
    }
  };

  const handlePublishQuickStory = async (e: Event) => {
    e.preventDefault();
    if (!title.trim() || sentences.length === 0) {
      setErrorMsg('请填写故事标题并输入至少一句故事正文');
      return;
    }
    setSubmitting(true);
    setErrorMsg('');
    setStatusMsg('');
    try {
      const sortedEntries = Object.entries(pageImages)
        .map(([k, slot]) => [Number(k), slot] as const)
        .filter(([idx, slot]) => idx >= 0 && idx < sentences.length && Boolean(slot.base64Data))
        .sort(([a], [b]) => a - b);

      // 组合参数：批量章节图不带 sentence_index（由后端 SaveUserPreparedImages 按 idx*N/M 平均分配），
      // 单独手动指定的行携带显式 sentence_index 精确覆盖对应行
      const customImagesPayload: { sentence_index?: number; filename: string; data_base64: string }[] = [];
      if (batchSourceImages.length > 0) {
        for (const slot of batchSourceImages) {
          if (slot.base64Data) {
            customImagesPayload.push({
              filename: slot.filename,
              data_base64: slot.base64Data
            });
          }
        }
        for (const [idx, slot] of sortedEntries) {
          if (slot.isManualOverride) {
            customImagesPayload.push({
              sentence_index: idx,
              filename: slot.filename,
              data_base64: slot.base64Data
            });
          }
        }
      } else {
        for (const [idx, slot] of sortedEntries) {
          customImagesPayload.push({
            sentence_index: idx,
            filename: slot.filename,
            data_base64: slot.base64Data
          });
        }
      }

      const res = await apiClient.quickCreateOrUpdateStory(pin, {
        title: title.trim(),
        theme: theme.trim(),
        description: description.trim() || `共 ${sentences.length} 页互动点读故事`,
        raw_text: sentences.join('\n'),
        custom_images: customImagesPayload.length > 0 ? customImagesPayload : undefined,
        image_api_url: imageApiUrl.trim() || undefined,
        image_api_key: imageApiKey.trim() || undefined,
        image_model: imageModel.trim() || undefined
      });
      setStatusMsg(
        `🎉 发布成功！《${title.trim()}》共生成 ${res.sentence_count} 页（版本 v${res.version_number}），已自动预缓存语音。`
      );
      await loadStories();
    } catch (err: any) {
      setErrorMsg(`发布失败：${err.message || String(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  const handleAutoGenerate = async (autoPublish: boolean) => {
    setSubmitting(true);
    setErrorMsg('');
    setStatusMsg('');
    try {
      const res = await apiClient.autoGenerateStory(pin, {
        child_id: activeChild?.id,
        theme: autoTheme,
        auto_publish: autoPublish,
        llm_api_url: llmApiUrl.trim() || undefined,
        llm_api_key: llmApiKey.trim() || undefined,
        llm_model: llmModel.trim() || undefined,
        image_api_url: imageApiUrl.trim() || undefined,
        image_api_key: imageApiKey.trim() || undefined,
        image_model: imageModel.trim() || undefined
      });

      setTitle(res.title);
      setTheme(res.theme);
      setDescription(res.description);
      setRawText(res.raw_text);
      setPreviewPageIndex(0);
      setBatchSourceImages([]);
      if (res.image_urls && res.image_urls.length > 0) {
        const nextImgs: Record<number, PageImageSlot> = {};
        const uniqueUrls = Array.from(new Set(res.image_urls.filter(Boolean)));
        res.image_urls.forEach((u, idx) => {
          if (!u) return;
          const gIdx = uniqueUrls.indexOf(u);
          nextImgs[idx] = {
            filename: `auto_p${String(idx + 1).padStart(2, '0')}`,
            previewUrl: u,
            base64Data: '',
            groupIndex: gIdx >= 0 ? gIdx + 1 : 1,
            groupTotal: uniqueUrls.length
          };
        });
        setPageImages(nextImgs);
      } else {
        setPageImages({});
      }

      if (autoPublish && res.published) {
        setStatusMsg(`✨ 全自动生成并上架成功：《${res.title}》（共 ${res.sentence_count} 页）`);
        await loadStories();
      } else {
        setActiveTab('quick');
        setStatusMsg(`✨ 已生成《${res.title}》草稿，请在左侧逐句检查并在右侧预览点读效果。`);
      }
    } catch (err: any) {
      setErrorMsg(`自动生成失败：${err.message || String(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  const handleZipImport = async (file: File | undefined) => {
    if (!file) return;
    setSubmitting(true);
    setErrorMsg('');
    setStatusMsg('');
    try {
      const res = await apiClient.importStoryZip(pin, file);
      setStatusMsg(`📦 离线 ZIP 故事包导入成功（共 ${res.sentence_count} 页）！`);
      await loadStories();
    } catch (err: any) {
      setErrorMsg(`ZIP 导入失败：${err.message || String(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  const handleDeleteStory = async (st: StorySummary) => {
    if (!window.confirm(`确定要从书架删除《${st.title}》吗？（六六的历史识字记录会安全保留）`)) {
      return;
    }
    try {
      await apiClient.deleteStory(pin, st.id, false);
      setStatusMsg(`已删除故事《${st.title}》`);
      await loadStories();
    } catch (err: any) {
      setErrorMsg(`删除失败：${err.message || String(err)}`);
    }
  };

  const handleDeleteCurrentChild = async () => {
    if (!activeChild) return;
    if (!window.confirm(`确定删除读者档案「${activeChild.name}」吗？此操作不可恢复。`)) {
      return;
    }
    try {
      await apiClient.deleteChild(pin, activeChild.id);
      onChildDeleted();
    } catch (err: any) {
      setErrorMsg(`删除档案失败：${err.message || String(err)}`);
    }
  };

  const handleCheckUpdate = async () => {
    setCheckingUpdate(true);
    setErrorMsg('');
    setStatusMsg('');
    try {
      const res = await apiClient.checkSystemUpdate(pin, mirrorUrl);
      setUpdateCheckInfo(res);
      if (res.has_update) {
        setStatusMsg(`🚀 发现 GitHub 新版本 ${res.latest_version}（当前版本 ${res.current_version}），可点击下方按钮一键升级！`);
      } else {
        setStatusMsg(`✅ 当前已是最新版本（${res.current_version}）。`);
      }
    } catch (err: any) {
      setErrorMsg(`检查 GitHub 更新失败：${err.message || String(err)}`);
    } finally {
      setCheckingUpdate(false);
    }
  };

  const handleApplyUpdate = async () => {
    if (!window.confirm('确认立即从 GitHub 下载新版本并自动重启服务吗？（升级期间会自动备份并在失败时回滚）')) {
      return;
    }
    setApplyingUpdate(true);
    setErrorMsg('');
    setStatusMsg('⏳ 正在从 GitHub 下载二进制更新包并校验 SHA256，请稍候...');
    try {
      const res = await apiClient.applySystemUpdate(pin, mirrorUrl);
      setStatusMsg(
        `🎉 升级成功（${res.previous_version || ''} -> ${res.new_version || '最新版'}）！服务正在后台平滑重启，约 3 秒后自动刷新页面...`
      );
      setTimeout(() => {
        window.location.reload();
      }, 3500);
    } catch (err: any) {
      setErrorMsg(`在线升级失败：${err.message || String(err)}`);
    } finally {
      setApplyingUpdate(false);
    }
  };

  const activePreviewSentence = sentences[previewPageIndex] || '小熊来到美丽的小河边，勇敢地走上木桥。';
  const activePreviewTokens = useMemo(
    () => buildPreviewTokens(activePreviewSentence, previewPageIndex),
    [activePreviewSentence, previewPageIndex]
  );
  const activePreviewImage = pageImages[previewPageIndex]?.previewUrl || '';

  return (
    <div class="admin-workspace">
      {/* 左栏：监护人核心工作台 */}
      <div class="admin-panel">
        <header class="guardian-header">
          <div>
            <h2>🛡️ 监护人工作台</h2>
            <p class="subtitle">
              当前读者：{activeChild ? `${activeChild.avatar || '🐻'} ${activeChild.name}` : '默认读者'}
              {healthInfo ? ` · 版本 ${healthInfo.version} (${healthInfo.arch})` : ''}
            </p>
          </div>
          <div class="guardian-header-controls">
            <label class="pin-label">
              PIN:
              <input
                type="password"
                value={pin}
                onInput={(e) => setPin((e.target as HTMLInputElement).value)}
                class="input-field pin-input"
              />
            </label>
            <button type="button" class="btn-secondary" onClick={onClose}>
              ← 返回书架
            </button>
          </div>
        </header>

        {/* 功能导航标签栏 */}
        <div class="admin-tabs">
          <button
            type="button"
            class={`mode-btn ${activeTab === 'prompt' ? 'active' : ''}`}
            onClick={() => setActiveTab('prompt')}
          >
            🧠 AI 提示词与识字计划
          </button>
          <button
            type="button"
            class={`mode-btn ${activeTab === 'quick' ? 'active' : ''}`}
            onClick={() => setActiveTab('quick')}
          >
            📝 纯净正文一键导入
          </button>
          <button
            type="button"
            class={`mode-btn ${activeTab === 'autogen' ? 'active' : ''}`}
            onClick={() => setActiveTab('autogen')}
          >
            🤖 AI 全自动生成
          </button>
          <button
            type="button"
            class={`mode-btn ${activeTab === 'library' ? 'active' : ''}`}
            onClick={() => setActiveTab('library')}
          >
            📚 书架与备份 ({stories.length})
          </button>
          <button
            type="button"
            class={`mode-btn ${activeTab === 'system' ? 'active' : ''}`}
            onClick={() => setActiveTab('system')}
          >
            ⬆️ GitHub 在线升级
          </button>
        </div>

        {errorMsg && (
          <div class="status-notice notice-error">
            ⚠️ {errorMsg}
          </div>
        )}
        {statusMsg && (
          <div class="status-notice notice-ok">
            {statusMsg}
          </div>
        )}

        {/* Tab 0: 根据历史记录自动生成的 AI 提示词与艾宾浩斯计划 */}
        {activeTab === 'prompt' && (
          <section class="admin-tab-content">
            <div class="section-header-row">
              <div>
                <h3 style={{ margin: 0 }}>📊 艾宾浩斯遗忘曲线识字计划 & AI 故事提示词</h3>
                <p class="subtitle">
                  每次退出阅读后自动根据点字记录重算，可直接复制给 Gemini / Claude / ChatGPT 生成新故事
                </p>
              </div>
              <div class="header-actions-inline">
                <button
                  type="button"
                  class="btn-secondary"
                  disabled={loadingPlan}
                  onClick={() => void loadStoryPlan()}
                >
                  {loadingPlan ? '🔄 计算中...' : '🔄 刷新最新记录'}
                </button>
                <button
                  type="button"
                  class="btn-primary"
                  disabled={!plan?.story_generation_prompt}
                  onClick={handleCopyPrompt}
                >
                  {copiedPrompt ? '✅ 已复制提示词！' : '📋 一键复制 AI 提示词'}
                </button>
                <button
                  type="button"
                  class="btn-secondary"
                  disabled={pullingGist}
                  onClick={() => void handlePullFromGist(false)}
                >
                  {pullingGist ? '⏳ 拉取中...' : '☁️ 从 Gist 拉取新故事（零缓存）'}
                </button>
              </div>
            </div>

            {plan ? (
              <>
                <div class="plan-badges">
                  <div>
                    <strong>🔥 急需复习字（{plan.review_targets?.length || 0}）：</strong>
                    {plan.review_targets?.length ? (
                      plan.review_targets.map((t) => (
                        <span key={t.character + t.pinyin} class="word-badge badge-review" title={t.reason}>
                          {t.character}({t.pinyin} · 保留率{t.retention_pct ?? '--'}%)
                        </span>
                      ))
                    ) : (
                      <span class="subtitle">当前无紧急遗忘字</span>
                    )}
                  </div>

                  <div>
                    <strong>⏳ 待巩固观察字（{plan.observe_targets?.length || 0}）：</strong>
                    {plan.observe_targets?.length ? (
                      plan.observe_targets.map((t) => (
                        <span key={t.character + t.pinyin} class="word-badge badge-observe" title={t.reason}>
                          {t.character}({t.pinyin})
                        </span>
                      ))
                    ) : (
                      <span class="subtitle">当前无待巩固字</span>
                    )}
                  </div>

                  <div>
                    <strong>🌱 本篇科学引入拓展新字（{plan.new_candidates?.length || 0}）：</strong>
                    {plan.new_candidates?.length ? (
                      plan.new_candidates.map((t) => (
                        <span key={t.character + t.pinyin} class="word-badge badge-new" title={t.reason}>
                          {t.character}({t.pinyin}：{(t.recommended_words || []).slice(0, 2).join('/')})
                        </span>
                      ))
                    ) : (
                      <span class="subtitle">无</span>
                    )}
                  </div>
                </div>

                <div class="prompt-box-wrap">
                  <div class="prompt-box-header">
                    <strong>📜 自动生成的完整 AI 提示词（含全部已掌握熟字 + 遗忘曲线生字 + 拓展新字）：</strong>
                    <button
                      type="button"
                      class="btn-secondary"
                      style={{ padding: '6px 12px', fontSize: '0.85rem' }}
                      onClick={() => setActiveTab('quick')}
                    >
                      去粘贴生成好的故事正文 →
                    </button>
                  </div>
                  <textarea
                    readOnly
                    class="story-textarea prompt-readonly-box"
                    rows={14}
                    value={plan.story_generation_prompt || '暂无提示词数据'}
                    onClick={(e) => (e.target as HTMLTextAreaElement).select()}
                  />
                </div>
              </>
            ) : (
              <p class="subtitle">{loadingPlan ? '正在加载六六的识字历史...' : '暂无识字计划数据'}</p>
            )}
          </section>
        )}

        {/* Tab 1: 一键纯净正文导入 */}
        {activeTab === 'quick' && (
          <form onSubmit={handlePublishQuickStory} class="admin-tab-content">
            <div class="pipeline-bar">
              <span class="pipeline-step" data-state={currentStep >= 1 ? (currentStep > 1 ? 'done' : 'active') : ''}>
                1. 粘贴纯净正文
              </span>
              <span>→</span>
              <span class="pipeline-step" data-state={currentStep >= 2 ? (currentStep > 2 ? 'done' : 'active') : ''}>
                2. 自动单句分页 ({sentences.length}页)
              </span>
              <span>→</span>
              <span class="pipeline-step" data-state={currentStep >= 3 ? (currentStep > 3 ? 'done' : 'active') : ''}>
                3. 绑定插画 ({Object.keys(pageImages).length}/{sentences.length || 0})
              </span>
              <span>→</span>
              <span class="pipeline-step" data-state={currentStep === 4 ? 'done' : ''}>
                4. 上架书架
              </span>
            </div>

            <div class="form-row">
              <label style={{ flex: '1 1 200px' }}>
                <span class="field-label">故事标题 *</span>
                <input
                  type="text"
                  required
                  placeholder="例如：小猪鲁本与木桥冒险"
                  value={title}
                  onInput={(e) => setTitle((e.target as HTMLInputElement).value)}
                  class="input-field full-width"
                />
              </label>

              <label style={{ flex: '1 1 200px' }}>
                <span class="field-label">故事主题</span>
                <input
                  type="text"
                  list="preset-themes-list"
                  value={theme}
                  onInput={(e) => setTheme((e.target as HTMLInputElement).value)}
                  class="input-field full-width"
                />
                <datalist id="preset-themes-list">
                  {PRESET_THEMES.map((t) => (
                    <option key={t} value={t} />
                  ))}
                </datalist>
              </label>
            </div>

            <label class="block-label">
              <div class="prompt-box-header">
                <span>故事正文（每行一句，或按句号自动拆页）*</span>
                <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap' }}>
                  <button
                    type="button"
                    class="btn-secondary"
                    style={{ padding: '4px 10px', fontSize: '0.82rem' }}
                    disabled={pullingGist}
                    onClick={() => void handlePullFromGist(false)}
                  >
                    {pullingGist ? '⏳ 正在从 Gist 拉取...' : '☁️ 从 Gist 拉取故事与配图（零缓存）'}
                  </button>
                  <button
                    type="button"
                    class="btn-secondary"
                    style={{ padding: '4px 10px', fontSize: '0.82rem' }}
                    onClick={() => setActiveTab('prompt')}
                  >
                    📋 查看/复制 AI 提示词
                  </button>
                </div>
              </div>
              <textarea
                class="story-textarea"
                placeholder={'请直接粘贴纯净故事正文，每行 1 句话：\n小熊来到哗哗流淌的小河边。\n鹿阿姨告诉他前面有一座安全的木桥。'}
                value={rawText}
                onInput={(e) => setRawText((e.target as HTMLTextAreaElement).value)}
              />
            </label>

            {sentences.length > 0 && (
              <div style={{ marginBottom: '18px' }}>
                <div class="prompt-box-header" style={{ marginBottom: '10px' }}>
                  <strong>🔍 逐句分页与多音字检查（点击任意行在右侧预览）</strong>
                  <div>
                    <input
                      ref={batchImgInputRef}
                      type="file"
                      accept="image/*"
                      multiple
                      style={{ display: 'none' }}
                      onChange={(e) => handleBatchImagesUpload((e.target as HTMLInputElement).files)}
                    />
                    <button
                      type="button"
                      class="btn-secondary"
                      style={{ padding: '6px 12px', fontSize: '0.85rem' }}
                      onClick={() => batchImgInputRef.current?.click()}
                    >
                      🖼️ 批量上传并按行数平均分配配图
                    </button>
                  </div>
                </div>

                <div class="page-inspector-list">
                  {sentences.map((sent, idx) => {
                    const chars = Array.from(sent);
                    const hasImg = Boolean(pageImages[idx]);
                    return (
                      <div
                        key={idx}
                        class="page-row"
                        data-active={previewPageIndex === idx ? 'true' : 'false'}
                        onClick={() => setPreviewPageIndex(idx)}
                      >
                        <span class="page-badge">P{String(idx + 1).padStart(2, '0')}</span>

                        <div class="seg-list">
                          {chars.map((ch, cIdx) => {
                            if (PUNCTUATION.has(ch)) {
                              return (
                                <span key={cIdx} style={{ alignSelf: 'center', color: 'var(--text-pinyin)' }}>
                                  {ch}
                                </span>
                              );
                            }
                            const isPoly = POLYPHONIC_CHARS.has(ch);
                            const isRev = reviewCharSet.has(ch);
                            return (
                              <span
                                key={cIdx}
                                class="seg-chip"
                                data-polyphonic={isPoly ? 'true' : 'false'}
                                data-review={isRev ? 'true' : 'false'}
                                title={
                                  isPoly
                                    ? '多音字（发布时由后端语境词典自动定音）'
                                    : isRev
                                    ? '本篇目标复习/拓展生字'
                                    : ''
                                }
                              >
                                <span>{ch}</span>
                              </span>
                            );
                          })}
                        </div>

                        <label
                          class="btn-secondary"
                          style={{ padding: '6px 10px', fontSize: '0.8rem', cursor: 'pointer', whiteSpace: 'nowrap' }}
                          onClick={(e) => e.stopPropagation()}
                        >
                          {hasImg
                            ? pageImages[idx]?.groupIndex
                              ? `✅ 图${pageImages[idx].groupIndex}/${pageImages[idx].groupTotal}`
                              : '✅ 已配图'
                            : '+ 配图'}
                          <input
                            type="file"
                            accept="image/*"
                            style={{ display: 'none' }}
                            onChange={(e) =>
                              handleSinglePageImageUpload(idx, (e.target as HTMLInputElement).files?.[0])
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
              <button type="submit" class="btn-primary" disabled={submitting || sentences.length === 0}>
                {submitting ? '⏳ 正在打包并预缓存语音...' : `🚀 立即发布到书架（共 ${sentences.length} 页）`}
              </button>
            </div>
          </form>
        )}

        {/* Tab 2: AI 全自动生成 */}
        {activeTab === 'autogen' && (
          <div class="admin-tab-content">
            <p style={{ marginTop: 0, color: 'var(--text-pinyin)', fontSize: '0.92rem' }}>
              根据六六的艾宾浩斯遗忘曲线计划，全自动生成 10 分钟阅读量分级故事与无字绘本插画。
            </p>

            <label class="block-label">
              <span class="field-label">选择探险主题</span>
              <input
                type="text"
                list="preset-themes-list"
                value={autoTheme}
                onInput={(e) => setAutoTheme((e.target as HTMLInputElement).value)}
                class="input-field full-width"
              />
            </label>

            <div style={{ marginBottom: '14px' }}>
              <button
                type="button"
                class="btn-secondary"
                style={{ padding: '6px 12px', fontSize: '0.85rem' }}
                onClick={() => setShowApiConfig(!showApiConfig)}
              >
                {showApiConfig ? '▼ 收起外部大模型/绘图 API 配置' : '▶ 展开外部大模型/绘图 API 配置（可选）'}
              </button>
            </div>

            {showApiConfig && (
              <div class="api-config-box">
                <div class="form-row">
                  <label style={{ flex: '1 1 200px' }}>
                    <span style={{ fontSize: '0.82rem' }}>LLM 接口地址 (OpenAI 兼容)</span>
                    <input
                      type="text"
                      placeholder="留空默认使用服务端配置"
                      value={llmApiUrl}
                      onInput={(e) => setLlmApiUrl((e.target as HTMLInputElement).value)}
                      class="input-field full-width"
                    />
                  </label>
                  <label style={{ flex: '1 1 160px' }}>
                    <span style={{ fontSize: '0.82rem' }}>LLM 模型名称</span>
                    <input
                      type="text"
                      placeholder="如 deepseek-chat"
                      value={llmModel}
                      onInput={(e) => setLlmModel((e.target as HTMLInputElement).value)}
                      class="input-field full-width"
                    />
                  </label>
                </div>
                <label class="block-label">
                  <span style={{ fontSize: '0.82rem' }}>LLM API Key</span>
                  <input
                    type="password"
                    placeholder="sk-..."
                    value={llmApiKey}
                    onInput={(e) => setLlmApiKey((e.target as HTMLInputElement).value)}
                    class="input-field full-width"
                  />
                </label>

                <div class="form-row">
                  <label style={{ flex: '1 1 200px' }}>
                    <span style={{ fontSize: '0.82rem' }}>绘图 API 地址 (可选)</span>
                    <input
                      type="text"
                      placeholder="留空自动生成清新矢量插画"
                      value={imageApiUrl}
                      onInput={(e) => setImageApiUrl((e.target as HTMLInputElement).value)}
                      class="input-field full-width"
                    />
                  </label>
                  <label style={{ flex: '1 1 160px' }}>
                    <span style={{ fontSize: '0.82rem' }}>绘图模型</span>
                    <input
                      type="text"
                      placeholder="如 flux / dall-e-3"
                      value={imageModel}
                      onInput={(e) => setImageModel((e.target as HTMLInputElement).value)}
                      class="input-field full-width"
                    />
                  </label>
                </div>
                <label class="block-label" style={{ marginBottom: 0 }}>
                  <span style={{ fontSize: '0.82rem' }}>绘图 API Key</span>
                  <input
                    type="password"
                    value={imageApiKey}
                    onInput={(e) => setImageApiKey((e.target as HTMLInputElement).value)}
                    class="input-field full-width"
                  />
                </label>
              </div>
            )}

            <div class="guardian-actions">
              <button
                type="button"
                class="btn-secondary"
                disabled={submitting}
                onClick={() => handleAutoGenerate(false)}
              >
                {submitting ? '生成中...' : '📝 生成草稿到编辑器检查'}
              </button>
              <button
                type="button"
                class="btn-primary"
                disabled={submitting}
                onClick={() => handleAutoGenerate(true)}
              >
                {submitting ? '生成并发布中...' : '⚡ 一键全自动生成并上架'}
              </button>
            </div>
          </div>
        )}

        {/* Tab 3: 书架管理、ZIP 导入与数据导出 */}
        {activeTab === 'library' && (
          <div class="admin-tab-content">
            <div class="guardian-actions" style={{ marginTop: 0, marginBottom: '18px' }}>
              <input
                ref={zipInputRef}
                type="file"
                accept=".zip"
                style={{ display: 'none' }}
                onChange={(e) => handleZipImport((e.target as HTMLInputElement).files?.[0])}
              />
              <button
                type="button"
                class="btn-primary"
                style={{ padding: '8px 14px', fontSize: '0.9rem' }}
                disabled={pullingGist}
                onClick={() => void handlePullFromGist(false)}
              >
                {pullingGist ? '⏳ 正在从 Gist 拉取...' : '☁️ 从 Gist 拉取新故事（零缓存）'}
              </button>
              <button
                type="button"
                class="btn-secondary"
                style={{ padding: '8px 14px', fontSize: '0.9rem' }}
                onClick={() => zipInputRef.current?.click()}
              >
                📦 导入离线 ZIP 故事包
              </button>
              <a
                href={`/api/v1/admin/export?format=json&pin=${encodeURIComponent(pin)}`}
                target="_blank"
                rel="noreferrer"
                class="btn-secondary"
                style={{ padding: '8px 14px', fontSize: '0.9rem' }}
              >
                ⬇️ 导出 JSON 学习记录
              </a>
              <a
                href={`/api/v1/admin/export?format=sqlite&pin=${encodeURIComponent(pin)}`}
                target="_blank"
                rel="noreferrer"
                class="btn-secondary"
                style={{ padding: '8px 14px', fontSize: '0.9rem' }}
              >
                💾 备份 SQLite 数据库
              </a>
              {activeChild && (
                <button
                  type="button"
                  class="btn-danger"
                  style={{ padding: '8px 14px', fontSize: '0.9rem' }}
                  onClick={handleDeleteCurrentChild}
                >
                  🗑️ 删除当前儿童档案
                </button>
              )}
            </div>

            <div class="page-inspector-list">
              {stories.map((st) => (
                <div key={st.id} class="page-row" style={{ cursor: 'default' }}>
                  <span class="page-badge">v{st.version_number}</span>
                  <div>
                    <strong>{st.title}</strong>
                    <div style={{ fontSize: '0.82rem', color: 'var(--text-pinyin)' }}>
                      {st.theme} · 共 {st.sentence_count} 句
                    </div>
                  </div>
                  <button
                    type="button"
                    class="btn-danger"
                    style={{ padding: '6px 12px', fontSize: '0.82rem' }}
                    onClick={() => handleDeleteStory(st)}
                  >
                    删除
                  </button>
                </div>
              ))}
            </div>
          </div>
        )}

        {/* Tab 4: GitHub 在线升级与系统状态 */}
        {activeTab === 'system' && (
          <div class="admin-tab-content">
            <h3 style={{ marginTop: 0 }}>⬆️ 从 GitHub 检查更新与一键在线升级</h3>
            <p class="subtitle" style={{ marginBottom: '16px' }}>
              支持从 GitHub Releases 自动拉取对应架构二进制包，校验 SHA256 后热替换重启，若启动异常自动回滚。
            </p>

            {healthInfo && (
              <div class="system-info-grid">
                <div class="sys-stat-card">
                  <span class="subtitle">当前运行版本</span>
                  <strong>{healthInfo.version}</strong>
                </div>
                <div class="sys-stat-card">
                  <span class="subtitle">系统架构</span>
                  <strong>{healthInfo.arch}</strong>
                </div>
                <div class="sys-stat-card">
                  <span class="subtitle">可用磁盘空间</span>
                  <strong>{healthInfo.free_disk_mb} MB</strong>
                </div>
              </div>
            )}

            <label class="block-label" style={{ marginTop: '16px' }}>
              <span class="field-label">GitHub 国内加速镜像前缀（可选，直连可留空）</span>
              <input
                type="text"
                placeholder="例如 https://ghproxy.net/ 或留空直连"
                value={mirrorUrl}
                onInput={(e) => setMirrorUrl((e.target as HTMLInputElement).value)}
                class="input-field full-width"
              />
            </label>

            <div class="guardian-actions">
              <button
                type="button"
                class="btn-secondary"
                disabled={checkingUpdate || applyingUpdate}
                onClick={handleCheckUpdate}
              >
                {checkingUpdate ? '🔍 正在连接 GitHub 检查...' : '🔍 检查 GitHub 最新版本'}
              </button>

              <button
                type="button"
                class="btn-primary"
                disabled={applyingUpdate}
                onClick={handleApplyUpdate}
              >
                {applyingUpdate ? '⏳ 正在下载并升级重启...' : '🚀 立即从 GitHub 下载并升级'}
              </button>
            </div>

            {updateCheckInfo && (
              <div class="api-config-box" style={{ marginTop: '16px' }}>
                <div>
                  <strong>检查结果：</strong>
                  当前版本 <code>{updateCheckInfo.current_version}</code> · 最新版本{' '}
                  <code>{updateCheckInfo.latest_version}</code>{' '}
                  {updateCheckInfo.has_update ? (
                    <span class="word-badge badge-new">有新版本可升级</span>
                  ) : (
                    <span class="word-badge badge-observe">已是最新</span>
                  )}
                </div>
                {updateCheckInfo.release_notes && (
                  <pre class="release-notes-box">{updateCheckInfo.release_notes}</pre>
                )}
              </div>
            )}
          </div>
        )}
      </div>

      {/* 右栏：1:1 儿童点读模拟器实时预览 */}
      <aside class="admin-panel preview-sticky">
        <div class="prompt-box-header">
          <div>
            <h3 style={{ margin: 0, fontSize: '1.05rem' }}>📱 儿童点读实时预览</h3>
            <span style={{ fontSize: '0.8rem', color: 'var(--text-pinyin)' }}>
              默认隐藏拼音，点击汉字测试语音与 10 秒拼音气泡
            </span>
          </div>
          <div style={{ display: 'flex', gap: '6px', flexWrap: 'wrap' }}>
            <button
              type="button"
              class={`mode-btn ${previewOrientation === 'landscape' ? 'active' : ''}`}
              style={{ padding: '4px 10px', fontSize: '0.8rem' }}
              onClick={() => setPreviewOrientation('landscape')}
            >
              横屏 4:3
            </button>
            <button
              type="button"
              class={`mode-btn ${previewOrientation === 'portrait' ? 'active' : ''}`}
              style={{ padding: '4px 10px', fontSize: '0.8rem' }}
              onClick={() => setPreviewOrientation('portrait')}
            >
              竖屏 3:4
            </button>
            <button
              type="button"
              class={`mode-btn ${previewShowPinyin ? 'active' : ''}`}
              style={{ padding: '4px 10px', fontSize: '0.8rem' }}
              onClick={() => setPreviewShowPinyin(!previewShowPinyin)}
            >
              {previewShowPinyin ? '常驻拼音:开' : '常驻拼音:关'}
            </button>
          </div>
        </div>

        <div class="preview-viewport" data-orientation={previewOrientation}>
          <div
            style={{
              display: 'flex',
              flexDirection: 'column',
              justifyContent: 'space-between',
              width: '100%',
              height: '100%',
              gap: '12px'
            }}
          >
            <div style={{ display: 'flex', justifyContent: 'space-between', fontSize: '0.82rem', color: 'var(--text-pinyin)' }}>
              <span>{title || '未命名故事预览'}</span>
              <span>
                第 {previewPageIndex + 1} / {Math.max(1, sentences.length)} 页
              </span>
            </div>

            <div class="reader-illustration-card" style={{ flex: 1, minHeight: '140px' }}>
              {activePreviewImage ? (
                <img src={activePreviewImage} alt="预览配图" class="story-image" />
              ) : (
                <div class="story-image-placeholder">
                  <span style={{ fontSize: '2.2rem' }}>🖼️</span>
                  <span style={{ fontSize: '0.9rem' }}>
                    第 {previewPageIndex + 1} 页插画预览（未上传时发布将自动生成清新插画）
                  </span>
                </div>
              )}
            </div>

            <SentenceDisplay
              tokens={activePreviewTokens}
              activeWordIndex={null}
              activeTokenId={previewActiveTokenId}
              showPinyinHint={previewShowPinyin}
              onTokenClick={(tok) => {
                setPreviewActiveTokenId(tok.id);
                if (previewTimerRef.current !== null) {
                  window.clearTimeout(previewTimerRef.current);
                }
                previewTimerRef.current = window.setTimeout(() => {
                  setPreviewActiveTokenId(null);
                  previewTimerRef.current = null;
                }, PREVIEW_PINYIN_DURATION_MS);
                void audioPlayer.playUrlOrFallback('', tok.character);
              }}
            />

            <div style={{ display: 'flex', justifyContent: 'space-between', gap: '8px' }}>
              <button
                type="button"
                class="btn-secondary"
                style={{ padding: '6px 12px', fontSize: '0.85rem' }}
                disabled={previewPageIndex <= 0}
                onClick={() => setPreviewPageIndex((p) => Math.max(0, p - 1))}
              >
                ◀ 上一页
              </button>
              <button
                type="button"
                class="btn-audio"
                style={{ padding: '6px 14px', fontSize: '0.85rem' }}
                onClick={() => void audioPlayer.playUrlOrFallback('', activePreviewSentence)}
              >
                🔊 试听整句
              </button>
              <button
                type="button"
                class="btn-secondary"
                style={{ padding: '6px 12px', fontSize: '0.85rem' }}
                disabled={previewPageIndex >= Math.max(0, sentences.length - 1)}
                onClick={() => setPreviewPageIndex((p) => Math.min(Math.max(0, sentences.length - 1), p + 1))}
              >
                下一页 ▶
              </button>
            </div>
          </div>
        </div>
      </aside>
    </div>
  );
}
