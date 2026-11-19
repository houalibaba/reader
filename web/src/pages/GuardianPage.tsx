import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import {
  apiClient,
  ChildProfile,
  HealthStatusResponse,
  PlanTargetItem,
  SentenceToken,
  StoryPlanResponse,
  StorySummary,
  UpdateCheckResponse,
  VocabularyEntry
} from '../api/client';
import { audioPlayer } from '../audio/player';
import { SentenceDisplay } from '../components/SentenceDisplay';
import {
  deleteStoryAnimation,
  resolveStoryAnimationUrl,
  sortStoriesNewestFirst,
  uploadStoryAnimation
} from '../utils/rewardAnimation';

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
      token_index: idx,
      character: ch,
      pinyin: isPunct ? '' : POLYPHONIC_CHARS.has(ch) ? 'duō yīn' : 'pīn yīn',
      word: ch,
      word_index: wordIndex,
      audio_url: '',
      is_polyphone: POLYPHONIC_CHARS.has(ch)
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

function runViewTransition(updateFn: () => void) {
  const prefersReduced =
    typeof window !== 'undefined' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  if (!prefersReduced && typeof (document as any).startViewTransition === 'function') {
    (document as any).startViewTransition(updateFn);
  } else {
    updateFn();
  }
}

interface DurationPoint {
  label: string;
  minutes: number;
  title?: string;
}

function DurationLineChart({ points }: { points: DurationPoint[] }) {
  if (points.length === 0) {
    return <p class="empty-hint-text">暂无阅读时长记录，开始阅读后将自动生成趋势图。</p>;
  }

  const width = 340;
  const height = 148;
  const padX = 28;
  const padY = 24;
  const maxVal = Math.max(50, ...points.map((p) => p.minutes));

  const coords = points.map((p, idx) => {
    const x =
      points.length === 1
        ? width / 2
        : padX + (idx / (points.length - 1)) * (width - padX * 2);
    const y = height - padY - (p.minutes / maxVal) * (height - padY * 2);
    return { ...p, x, y };
  });

  const linePath = coords.map((c, i) => `${i === 0 ? 'M' : 'L'}${c.x},${c.y}`).join(' ');
  const areaPath = `${linePath} L${coords[coords.length - 1].x},${height - padY} L${coords[0].x},${height - padY} Z`;

  return (
    <svg
      viewBox={`0 0 ${width} ${height}`}
      role="img"
      aria-label="每次阅读时长折线图"
      class="duration-svg-chart"
    >
      <defs>
        <linearGradient id="readerDurationGrad" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stop-color="#3b82f6" stop-opacity="0.22" />
          <stop offset="100%" stop-color="#3b82f6" stop-opacity="0.02" />
        </linearGradient>
      </defs>

      {[0, 0.25, 0.5, 0.75, 1].map((ratio) => {
        const y = height - padY - ratio * (height - padY * 2);
        const tickVal = Math.round(maxVal * ratio);
        return (
          <g key={ratio}>
            <line
              x1={padX}
              y1={y}
              x2={width - padX}
              y2={y}
              stroke="#eceef2"
              stroke-width="1"
            />
            <text x={padX - 6} y={y + 3} text-anchor="end" font-size="9" fill="#86909c">
              {tickVal}
            </text>
          </g>
        );
      })}

      <path d={areaPath} fill="url(#readerDurationGrad)" />
      <path d={linePath} fill="none" stroke="#3b82f6" stroke-width="2.4" stroke-linecap="round" />

      {coords.map((c, idx) => (
        <g key={`${c.label}_${idx}`}>
          <circle cx={c.x} cy={c.y} r="4.5" fill="#3b82f6" stroke="#ffffff" stroke-width="2" />
          <text x={c.x} y={c.y - 9} text-anchor="middle" font-size="10" font-weight="600" fill="#374151">
            {c.minutes}m
          </text>
          <text x={c.x} y={height - 4} text-anchor="middle" font-size="10" fill="#68707c">
            {c.label}
          </text>
        </g>
      ))}
    </svg>
  );
}

export function GuardianPage({ activeChild, onClose, onChildDeleted }: GuardianPageProps) {
  const [pin, setPin] = useState<string>(() => sessionStorage.getItem('guardian_pin') || '1234');
  const [activeTab, setActiveTab] = useState<'plan' | 'bookshelf' | 'backup' | 'upgrade'>('plan');
  const [showHeaderMenu, setShowHeaderMenu] = useState<boolean>(false);

  const [statTab, setStatTab] = useState<'weekly' | 'duration' | 'mastered' | 'review'>('duration');
  const [durationViewMode, setDurationViewMode] = useState<'chart' | 'overview'>('chart');
  const [chartRange, setChartRange] = useState<'4' | '8' | 'all'>('4');
  const [masteredSearch, setMasteredSearch] = useState<string>('');
  const [showAllMasteredChars, setShowAllMasteredChars] = useState<boolean>(false);
  const isComposingRef = useRef<boolean>(false);

  const [plan, setPlan] = useState<StoryPlanResponse | null>(null);
  const [vocabList, setVocabList] = useState<VocabularyEntry[]>([]);
  const [loadingPlan, setLoadingPlan] = useState<boolean>(false);
  const [copiedPrompt, setCopiedPrompt] = useState<boolean>(false);
  const [showAllReviewChars, setShowAllReviewChars] = useState<boolean>(false);

  const [expandedStep, setExpandedStep] = useState<'step2' | 'step3' | null>(null);

  // 一键纯净正文导入 + 1 对 1 专属动画上传
  const [title, setTitle] = useState<string>('');
  const [theme, setTheme] = useState<string>(PRESET_THEMES[0]);
  const [description, setDescription] = useState<string>('');
  const [rawText, setRawText] = useState<string>('');
  const [batchSourceImages, setBatchSourceImages] = useState<PageImageSlot[]>([]);
  const [pageImages, setPageImages] = useState<Record<number, PageImageSlot>>({});
  const [pendingAnimationFile, setPendingAnimationFile] = useState<File | null>(null);
  const [uploadingAnimStoryId, setUploadingAnimStoryId] = useState<string | null>(null);
  const [pullingGist, setPullingGist] = useState<boolean>(false);

  const [previewPageIndex, setPreviewPageIndex] = useState<number>(0);
  const [previewShowPinyin, setPreviewShowPinyin] = useState<boolean>(false);
  const [previewActiveTokenId, setPreviewActiveTokenId] = useState<string | null>(null);
  const previewTimerRef = useRef<number | null>(null);

  const [autoTheme, setAutoTheme] = useState<string>(PRESET_THEMES[1]);
  const [llmApiUrl, setLlmApiUrl] = useState<string>('');
  const [llmApiKey, setLlmApiKey] = useState<string>('');
  const [llmModel, setLlmModel] = useState<string>('');
  const [imageApiUrl, setImageApiUrl] = useState<string>('');
  const [imageApiKey, setImageApiKey] = useState<string>('');
  const [imageModel, setImageModel] = useState<string>('');
  const [showAutoGenPanel, setShowAutoGenPanel] = useState<boolean>(false);

  const [stories, setStories] = useState<StorySummary[]>([]);
  const [submitting, setSubmitting] = useState<boolean>(false);
  const [statusMsg, setStatusMsg] = useState<string>('');
  const [errorMsg, setErrorMsg] = useState<string>('');

  const [healthInfo, setHealthInfo] = useState<HealthStatusResponse | null>(null);
  const [mirrorUrl, setMirrorUrl] = useState<string>(
    () => localStorage.getItem('github_mirror_url') || 'https://ghproxy.net/'
  );
  const [updateCheckInfo, setUpdateCheckInfo] = useState<UpdateCheckResponse | null>(null);
  const [checkingUpdate, setCheckingUpdate] = useState<boolean>(false);
  const [applyingUpdate, setApplyingUpdate] = useState<boolean>(false);

  const batchImgInputRef = useRef<HTMLInputElement | null>(null);
  const step2AnimInputRef = useRef<HTMLInputElement | null>(null);
  const zipInputRef = useRef<HTMLInputElement | null>(null);
  const menuDialogRef = useRef<HTMLDialogElement | null>(null);

  const sortedStories = useMemo(() => sortStoriesNewestFirst(stories), [stories]);
  const sentences = useMemo(() => splitSentencesClient(rawText), [rawText]);

  const reviewCharSet = useMemo(() => {
    const s = new Set<string>();
    plan?.review_targets?.forEach((t) => s.add(t.character));
    plan?.observe_targets?.forEach((t) => s.add(t.character));
    return s;
  }, [plan]);

  const newCharSet = useMemo(() => {
    const s = new Set<string>();
    plan?.new_candidates?.forEach((t) => s.add(t.character));
    return s;
  }, [plan]);

  const masteredCount = useMemo(() => {
    const fromSummary =
      (plan as any)?.summary?.familiar_count ??
      (plan as any)?.summary?.mastered_count ??
      (plan as any)?.summary?.likely_familiar_count;
    if (typeof fromSummary === 'number' && fromSummary > 0) {
      return fromSummary;
    }
    const familiarVocab = vocabList.filter((v) => v.estimated_status === 'likely_familiar').length;
    if (familiarVocab > 0) {
      return familiarVocab;
    }
    return (plan as any)?.familiar_anchors?.length || vocabList.length || 0;
  }, [plan, vocabList]);

  const urgentReviewCount = plan?.review_targets?.length || 0;
  const observeCount = plan?.observe_targets?.length || 0;
  const newCandidateCount = plan?.new_candidates?.length || 0;

  const masteredCharItems = useMemo<Array<{ character: string; pinyin: string }>>(() => {
    const map = new Map<string, string>();
    vocabList.forEach((v) => {
      if (v.estimated_status === 'likely_familiar' && v.character) {
        map.set(v.character, v.pinyin || '');
      }
    });
    const anchors: any[] = (plan as any)?.familiar_anchors || [];
    anchors.forEach((a) => {
      const ch = typeof a === 'string' ? a : a?.character;
      const py = typeof a === 'string' ? '' : a?.pinyin || '';
      if (ch && !map.has(ch)) {
        map.set(ch, py);
      }
    });
    if (map.size === 0) {
      vocabList.forEach((v) => {
        if (v.character && !map.has(v.character)) {
          map.set(v.character, v.pinyin || '');
        }
      });
    }
    return Array.from(map.entries()).map(([character, pinyin]) => ({ character, pinyin }));
  }, [vocabList, plan]);

  const filteredMasteredChars = useMemo(() => {
    const q = masteredSearch.trim().toLowerCase();
    if (!q) return masteredCharItems;
    return masteredCharItems.filter(
      (item) =>
        item.character.includes(q) || (item.pinyin && item.pinyin.toLowerCase().includes(q))
    );
  }, [masteredCharItems, masteredSearch]);

  const allDurationPoints = useMemo<DurationPoint[]>(() => {
    const summaryData = (plan as any)?.summary || {};
    const historyList: any[] =
      summaryData.reading_sessions ||
      summaryData.recent_sessions ||
      (plan as any)?.reading_history ||
      [];
    if (Array.isArray(historyList) && historyList.length > 0) {
      return historyList.map((h, idx) => ({
        label: String(h.date || h.label || `第${idx + 1}次`).slice(-5),
        minutes: Math.max(1, Math.round(Number(h.minutes ?? h.duration_minutes ?? 10))),
        title: h.title || h.story_title || ''
      }));
    }
    const readStories = sortedStories.filter((s) => s.resume_sentence_index > 0 || s.sentence_count > 0);
    return readStories.map((s, idx) => {
      const readPages = s.resume_sentence_index > 0 ? s.resume_sentence_index + 1 : s.sentence_count;
      const estMinutes = Math.max(3, Math.round(readPages * 0.6));
      return {
        label: `第${idx + 1}篇`,
        minutes: estMinutes,
        title: s.title
      };
    });
  }, [plan, sortedStories]);

  const visibleDurationPoints = useMemo(() => {
    if (chartRange === 'all') return allDurationPoints;
    const limit = Number(chartRange);
    return allDurationPoints.slice(-limit);
  }, [allDurationPoints, chartRange]);

  const weeklyReadCount = useMemo(() => {
    const fromSummary = (plan as any)?.summary?.weekly_read_count;
    if (typeof fromSummary === 'number') return fromSummary;
    return Math.min(sortedStories.length, allDurationPoints.length || sortedStories.length);
  }, [plan, sortedStories.length, allDurationPoints.length]);

  const totalReadMinutes = useMemo(() => {
    const fromSummary = (plan as any)?.summary?.total_read_minutes;
    if (typeof fromSummary === 'number') return fromSummary;
    return allDurationPoints.reduce((acc, item) => acc + item.minutes, 0);
  }, [plan, allDurationPoints]);

  const sortedReviewItems = useMemo<Array<PlanTargetItem & { computedPct: number }>>(() => {
    if (!plan) return [];
    const seen = new Set<string>();
    const combined: Array<PlanTargetItem & { computedPct: number }> = [];

    const pushItem = (item: PlanTargetItem, fallbackPct: number) => {
      if (!item.character || seen.has(item.character)) return;
      seen.add(item.character);
      const rawPct = typeof item.retention_pct === 'number' ? item.retention_pct : fallbackPct;
      const clamped = Math.max(1, Math.min(100, Math.round(rawPct)));
      combined.push({
        ...item,
        computedPct: clamped
      });
    };

    (plan.review_targets || []).forEach((item, idx) => pushItem(item, Math.min(35, 8 + idx * 5)));
    (plan.observe_targets || []).forEach((item, idx) => pushItem(item, Math.min(75, 42 + idx * 6)));

    return combined.sort((a, b) => a.computedPct - b.computedPct);
  }, [plan]);

  const visibleReviewItems = useMemo(() => {
    if (showAllReviewChars) return sortedReviewItems;
    return sortedReviewItems.slice(0, 6);
  }, [sortedReviewItems, showAllReviewChars]);

  const hiddenReviewCount = Math.max(0, sortedReviewItems.length - 6);

  const loadStories = async () => {
    try {
      const list = await apiClient.listStories(activeChild?.id);
      setStories(sortStoriesNewestFirst(list));
    } catch {
      // ignore
    }
  };

  const loadStoryPlan = async (childId?: string) => {
    const targetId = childId || activeChild?.id;
    if (!targetId) return;
    setLoadingPlan(true);
    try {
      const [planRes, vocabRes] = await Promise.all([
        apiClient.getStoryPlan(targetId),
        apiClient.getVocabulary(targetId).catch(() => [] as VocabularyEntry[])
      ]);
      setPlan(planRes);
      setVocabList(vocabRes);
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
    const dialog = menuDialogRef.current;
    if (!dialog) return;
    if (showHeaderMenu) {
      if (!dialog.open && typeof dialog.showModal === 'function') {
        dialog.showModal();
      }
    } else if (dialog.open) {
      dialog.close();
    }
  }, [showHeaderMenu]);

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

  const handleSelectStatCard = (target: 'weekly' | 'duration' | 'mastered' | 'review') => {
    runViewTransition(() => {
      if (target === 'duration' && statTab === 'duration') {
        setDurationViewMode((prev) => (prev === 'chart' ? 'overview' : 'chart'));
      } else {
        setStatTab(target);
        if (target === 'duration') {
          setDurationViewMode('chart');
        }
      }
    });
  };

  const handleCopyPrompt = async () => {
    if (!plan?.story_generation_prompt) {
      setErrorMsg('正在加载识字计划提示词，请稍候再试');
      return;
    }
    try {
      await navigator.clipboard.writeText(plan.story_generation_prompt);
      setCopiedPrompt(true);
      setStatusMsg('✅ 已复制 AI 提示词！可直接发给大模型生成故事，再点第 2 步导入正文。');
      setTimeout(() => setCopiedPrompt(false), 2600);
    } catch {
      setStatusMsg('请展开下方“查看完整提示词”手动全选复制');
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
      `已按图片数 (${sourceSlots.length}张) 与总行数 (${totalLines}行) 平均分配绑定：每张图对应约 ${avgLines} 行`
    );
  };

  const handlePullFromGist = async (force = false) => {
    setPullingGist(true);
    setErrorMsg('');
    setStatusMsg('⏳ 正在从 GitHub Gist 拉取最新故事、配图与专属动画...');
    try {
      const res: any = await apiClient.pullStoryFromGist(pin, force);
      if (res.title) setTitle(res.title);
      if (res.theme) setTheme(res.theme);
      if (res.description) setDescription(res.description);
      if (res.raw_text) setRawText(res.raw_text);
      setPreviewPageIndex(0);

      setBatchSourceImages([]);
      if (res.image_urls && res.image_urls.length > 0) {
        const mappedFromGist: Record<number, PageImageSlot> = {};
        const uniqueUrls = Array.from(new Set(res.image_urls.filter(Boolean))) as string[];
        res.image_urls.forEach((url: string, idx: number) => {
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

      const animHint = resolveStoryAnimationUrl(res) ? '（含专属动画）' : '';

      if (res.status === 'already_imported') {
        const shouldForce = window.confirm(
          `Gist 上的《${res.title}》（版本 ${res.version}）此前已在书架中。是否强制重新拉取并覆盖更新？`
        );
        if (shouldForce) {
          await handlePullFromGist(true);
          return;
        }
        setStatusMsg(
          `ℹ️ Gist 最新故事《${res.title}》${animHint}（共 ${res.sentence_count} 行）已在书架中，已加载到第 3 步预览。`
        );
        setActiveTab('plan');
        setExpandedStep('step3');
      } else {
        setStatusMsg(
          `🎉 从 Gist 拉取并导入成功：《${res.title}》${animHint}（v${res.version_number}，${res.lines_per_image}）！`
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

  const handlePublishQuickStory = async (e?: Event) => {
    if (e) e.preventDefault();
    if (!title.trim() || sentences.length === 0) {
      setErrorMsg('请先在第 2 步填写故事标题并粘贴故事正文');
      setExpandedStep('step2');
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

      // 若监护人选择了 1 对 1 专属动画，发布成功后流式上传绑定
      let animResultText = '';
      if (pendingAnimationFile && res.imported_story_id) {
        try {
          setStatusMsg('⏳ 故事已发布，正在流式上传专属动画...');
          await uploadStoryAnimation(pin, res.imported_story_id, pendingAnimationFile);
          setPendingAnimationFile(null);
          animResultText = '，并已绑定专属动画';
        } catch (animErr: any) {
          setErrorMsg(`故事已发布，但专属动画上传失败：${animErr.message || String(animErr)}`);
        }
      }

      setStatusMsg(
        `🎉 发布成功！《${title.trim()}》共 ${res.sentence_count} 页（v${res.version_number}）${animResultText}，已自动预缓存语音。`
      );
      await loadStories();
    } catch (err: any) {
      setErrorMsg(`发布失败：${err.message || String(err)}`);
    } finally {
      setSubmitting(false);
    }
  };

  const handleUploadStoryAnimationDirect = async (st: StorySummary, file: File | undefined) => {
    if (!file) return;
    setUploadingAnimStoryId(st.id);
    setErrorMsg('');
    setStatusMsg(`⏳ 正在为《${st.title}》上传专属动画...`);
    try {
      await uploadStoryAnimation(pin, st.id, file);
      setStatusMsg(`🎬 《${st.title}》专属动画绑定成功！`);
      await loadStories();
    } catch (err: any) {
      setErrorMsg(`上传《${st.title}》专属动画失败：${err.message || String(err)}`);
      setStatusMsg('');
    } finally {
      setUploadingAnimStoryId(null);
    }
  };

  const handleDeleteStoryAnimationDirect = async (st: StorySummary) => {
    if (!window.confirm(`确定移除《${st.title}》的专属动画吗？`)) return;
    setUploadingAnimStoryId(st.id);
    setErrorMsg('');
    try {
      await deleteStoryAnimation(pin, st.id);
      setStatusMsg(`已移除《${st.title}》的专属动画`);
      await loadStories();
    } catch (err: any) {
      setErrorMsg(`移除专属动画失败：${err.message || String(err)}`);
    } finally {
      setUploadingAnimStoryId(null);
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
        setExpandedStep('step3');
        setStatusMsg(`✨ 已生成《${res.title}》草稿，请在第 3 步预览并发布。`);
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
        setStatusMsg(`🚀 发现 GitHub 新版本 ${res.latest_version}（当前 ${res.current_version}），可点击升级！`);
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
        `🎉 升级成功（${res.previous_version || ''} -> ${res.new_version || '最新版'}）！服务正在后台重启，约 3 秒后刷新页面...`
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

  const activePreviewSentence =
    sentences[previewPageIndex] || '太阳升起来了，杰西带着粉红色的小猪鲁本走进大森林。';
  const activePreviewTokens = useMemo(
    () => buildPreviewTokens(activePreviewSentence, previewPageIndex),
    [activePreviewSentence, previewPageIndex]
  );
  const activePreviewImage = pageImages[previewPageIndex]?.previewUrl || '';

  const topCards = [
    { id: 'weekly' as const,   label: '本周阅读', value: weeklyReadCount,   unit: '次',   tone: 'green', icon: '📖' },
    { id: 'duration' as const, label: '阅读时长', value: totalReadMinutes,  unit: '分钟', tone: 'blue',  icon: '⏱️' },
    { id: 'mastered' as const, label: '累计识字', value: masteredCount,     unit: '个',   tone: 'amber', icon: '⭐' },
    { id: 'review' as const,   label: '待复习字', value: urgentReviewCount, unit: '个',   tone: 'sky',   icon: '🔄' },
  ];

  const displayedMasteredItems = showAllMasteredChars
    ? filteredMasteredChars
    : filteredMasteredChars.slice(0, 24);

  return (
    <div class="guardian-mobile-shell">
      <div class="guardian-card-container">
        <header class="guardian-top-header">
          <div class="guardian-title-block">
            <h1>监护人工作台</h1>
            <p class="guardian-reader-meta">
              读者：{activeChild ? activeChild.name : '六六'}
              {healthInfo?.version ? ` · ${healthInfo.version}` : ' · v0.3.12'}
            </p>
          </div>
          <div class="guardian-header-actions">
            <button
              type="button"
              class="guardian-back-reader-btn"
              onClick={onClose}
              title="回到儿童阅读书架"
            >
              ← 阅读
            </button>
            <button
              type="button"
              class="guardian-menu-btn"
              onClick={() => setShowHeaderMenu(true)}
              aria-label="打开工作台菜单与返回书架"
              title="菜单与设置"
            >
              <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.1" stroke-linecap="round" aria-hidden="true">
                <line x1="4" y1="7" x2="20" y2="7" />
                <line x1="4" y1="12" x2="20" y2="12" />
                <line x1="4" y1="17" x2="20" y2="17" />
              </svg>
            </button>
          </div>
        </header>

        <div class="guardian-scroll-viewport">
          {errorMsg && (
            <div class="guardian-alert guardian-alert-error" role="alert">
              <span>⚠️ {errorMsg}</span>
              <button type="button" class="alert-close-btn" onClick={() => setErrorMsg('')}>✕</button>
            </div>
          )}
          {statusMsg && (
            <div class="guardian-alert guardian-alert-ok" role="status">
              <span>{statusMsg}</span>
              <button type="button" class="alert-close-btn" onClick={() => setStatusMsg('')}>✕</button>
            </div>
          )}

          {activeTab === 'plan' && (
            <main class="guardian-main-body">
              <section class="guardian-top-kpi-grid" role="tablist" aria-label="学习指标视图切换">
                {topCards.map((card) => (
                  <button
                    key={card.id}
                    type="button"
                    role="tab"
                    aria-selected={statTab === card.id}
                    data-tone={card.tone}
                    class="kpi-tab-card"
                    onClick={() => handleSelectStatCard(card.id)}
                  >
                    <span class="kpi-tab-icon" aria-hidden="true">{card.icon}</span>
                    <span class="kpi-tab-label">{card.label}</span>
                    <strong class="kpi-tab-value">
                      {card.value}
                      <small class="kpi-tab-unit">{card.unit}</small>
                    </strong>
                  </button>
                ))}
              </section>

              <section class="guardian-dynamic-panel">
                {statTab === 'duration' && durationViewMode === 'chart' && (
                  <div class="guardian-review-section">
                    <div class="section-caption-row">
                      <h2 class="section-caption">每次阅读时长</h2>
                      <div style={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
                        <select
                          aria-label="选择统计周期"
                          class="compact-range-select"
                          value={chartRange}
                          onChange={(e) => setChartRange((e.target as HTMLSelectElement).value as '4' | '8' | 'all')}
                        >
                          <option value="4">近 4 次</option>
                          <option value="8">近 8 次</option>
                          <option value="all">全部</option>
                        </select>
                        <button
                          type="button"
                          class="text-link-btn"
                          onClick={() => handleSelectStatCard('duration')}
                        >
                          识字概览
                        </button>
                      </div>
                    </div>
                    <DurationLineChart points={visibleDurationPoints} />
                  </div>
                )}

                {statTab === 'duration' && durationViewMode === 'overview' && (
                  <div class="guardian-review-section">
                    <div class="section-caption-row">
                      <h2 class="section-caption">识字状态概览</h2>
                      <button
                        type="button"
                        class="text-link-btn"
                        onClick={() => handleSelectStatCard('duration')}
                      >
                        返回时长图表
                      </button>
                    </div>
                    <div class="guardian-stats-grid" aria-label="识字统计概览">
                      <div class="stat-tile stat-tile-mastered">
                        <span class="stat-tile-label">已掌握字</span>
                        <strong class="stat-tile-value">{masteredCount}</strong>
                      </div>
                      <div class="stat-tile stat-tile-urgent">
                        <span class="stat-tile-label">急需复习</span>
                        <strong class="stat-tile-value">{urgentReviewCount}</strong>
                      </div>
                      <div class="stat-tile stat-tile-observe">
                        <span class="stat-tile-label">待巩固</span>
                        <strong class="stat-tile-value">{observeCount}</strong>
                      </div>
                      <div class="stat-tile stat-tile-new">
                        <span class="stat-tile-label">本篇新字</span>
                        <strong class="stat-tile-value">{newCandidateCount}</strong>
                      </div>
                    </div>
                  </div>
                )}

                {statTab === 'review' && (
                  <div class="guardian-review-section">
                    <div class="section-caption-row">
                      <h2 class="section-caption">复习字，保留率低的在前</h2>
                      <button
                        type="button"
                        class="text-link-btn"
                        disabled={loadingPlan}
                        onClick={() => void loadStoryPlan()}
                      >
                        {loadingPlan ? '刷新中...' : '刷新'}
                      </button>
                    </div>

                    {sortedReviewItems.length > 0 ? (
                      <div class="retention-list">
                        {visibleReviewItems.map((item) => {
                          const pct = item.computedPct;
                          const barColorClass =
                            pct <= 15
                              ? 'retention-fill-low'
                              : pct <= 30
                              ? 'retention-fill-mid'
                              : 'retention-fill-high';

                          return (
                            <div key={item.character + item.pinyin} class="retention-row">
                              <div class="retention-char-cell">
                                <span class="retention-hanzi">{item.character}</span>
                                <span class="retention-pinyin">{item.pinyin}</span>
                              </div>
                              <div class="retention-track" role="progressbar" aria-valuenow={pct} aria-valuemin={0} aria-valuemax={100}>
                                <div class={`retention-fill ${barColorClass}`} style={{ width: `${Math.max(6, pct)}%` }} />
                              </div>
                              <span class="retention-pct">{pct}%</span>
                            </div>
                          );
                        })}

                        {hiddenReviewCount > 0 && (
                          <button
                            type="button"
                            class="retention-more-btn"
                            onClick={() => setShowAllReviewChars(!showAllReviewChars)}
                          >
                            {showAllReviewChars
                              ? '收起部分复习字'
                              : `另有 ${hiddenReviewCount} 个，查看全部`}
                          </button>
                        )}
                      </div>
                    ) : (
                      <p class="empty-hint-text">
                        {loadingPlan ? '正在计算六六的艾宾浩斯遗忘曲线...' : '当前暂无急需复习字，继续保持阅读吧！'}
                      </p>
                    )}
                  </div>
                )}

                {statTab === 'mastered' && (
                  <div class="guardian-review-section">
                    <div class="section-caption-row">
                      <h2 class="section-caption">已掌握的字</h2>
                      <span class="guardian-reader-meta" style={{ margin: 0 }}>
                        共 {filteredMasteredChars.length} 个
                      </span>
                    </div>

                    <input
                      type="search"
                      class="compact-input"
                      placeholder="搜索汉字或拼音..."
                      aria-label="搜索已掌握的字"
                      onCompositionStart={() => {
                        isComposingRef.current = true;
                      }}
                      onCompositionEnd={(e) => {
                        isComposingRef.current = false;
                        setMasteredSearch((e.target as HTMLInputElement).value);
                      }}
                      onInput={(e) => {
                        if (!isComposingRef.current) {
                          setMasteredSearch((e.target as HTMLInputElement).value);
                        }
                      }}
                    />

                    {displayedMasteredItems.length > 0 ? (
                      <>
                        <div class="mastered-char-grid">
                          {displayedMasteredItems.map((item) => (
                            <div key={item.character} class="mastered-char-tile">
                              {item.pinyin && <span class="mastered-char-py">{item.pinyin}</span>}
                              <strong class="mastered-char-hz">{item.character}</strong>
                            </div>
                          ))}
                        </div>
                        {filteredMasteredChars.length > 24 && (
                          <button
                            type="button"
                            class="retention-more-btn"
                            onClick={() => setShowAllMasteredChars(!showAllMasteredChars)}
                          >
                            {showAllMasteredChars
                              ? '收起字表'
                              : `展开全部 ${filteredMasteredChars.length} 字`}
                          </button>
                        )}
                      </>
                    ) : (
                      <p class="empty-hint-text">暂无匹配的已掌握汉字记录。</p>
                    )}
                  </div>
                )}

                {statTab === 'weekly' && (
                  <div class="guardian-review-section">
                    <div class="section-caption-row">
                      <h2 class="section-caption">本周阅读记录</h2>
                      <span class="guardian-reader-meta" style={{ margin: 0 }}>
                        共 {sortedStories.length} 篇故事
                      </span>
                    </div>
                    {sortedStories.length > 0 ? (
                      <div class="story-manage-list">
                        {sortedStories.slice(0, 6).map((st) => (
                          <div key={st.id} class="story-manage-row">
                            <div class="story-manage-info">
                              <strong>{st.title}</strong>
                              <span>
                                {st.theme} · 进度 {Math.min(st.sentence_count, (st.resume_sentence_index || 0) + 1)}/{st.sentence_count} 页
                              </span>
                            </div>
                          </div>
                        ))}
                      </div>
                    ) : (
                      <p class="empty-hint-text">暂无阅读记录。</p>
                    )}
                  </div>
                )}
              </section>

              <section class="guardian-steps-section">
                <h2 class="section-caption">生成新故事，三步</h2>

                <div class="three-steps-stack">
                  <button
                    type="button"
                    class="step-action-btn step-action-primary"
                    onClick={handleCopyPrompt}
                  >
                    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <rect x="9" y="9" width="13" height="13" rx="2" ry="2" />
                      <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
                    </svg>
                    <span>1 &nbsp;{copiedPrompt ? '已复制 AI 提示词 ✓' : '复制 AI 提示词'}</span>
                  </button>

                  <button
                    type="button"
                    class={`step-action-btn step-action-outline ${expandedStep === 'step2' ? 'is-expanded' : ''}`}
                    onClick={() => setExpandedStep(expandedStep === 'step2' ? null : 'step2')}
                  >
                    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4" />
                      <polyline points="17 8 12 3 7 8" />
                      <line x1="12" y1="3" x2="12" y2="15" />
                    </svg>
                    <span>
                      2 &nbsp;导入故事正文
                      {sentences.length > 0 ? `（已拆 ${sentences.length} 页）` : ''}
                    </span>
                  </button>

                  {expandedStep === 'step2' && (
                    <div class="step-drawer-card">
                      <div class="drawer-quick-row">
                        <button
                          type="button"
                          class="btn-inline-action"
                          disabled={pullingGist}
                          onClick={() => void handlePullFromGist(false)}
                        >
                          {pullingGist ? '⏳ 正在拉取...' : '☁️ 从 Gist 一键拉取最新故事与配图'}
                        </button>
                        <button
                          type="button"
                          class="btn-inline-action"
                          onClick={() => setShowAutoGenPanel(!showAutoGenPanel)}
                        >
                          {showAutoGenPanel ? '收起 AI 自动生成' : '🤖 AI 全自动生成'}
                        </button>
                      </div>

                      {showAutoGenPanel && (
                        <div class="autogen-mini-box">
                          <label class="compact-field">
                            <span>探险主题</span>
                            <input
                              type="text"
                              list="preset-themes-list"
                              value={autoTheme}
                              onInput={(e) => setAutoTheme((e.target as HTMLInputElement).value)}
                              class="compact-input"
                            />
                          </label>
                          <div class="drawer-quick-row">
                            <button
                              type="button"
                              class="btn-inline-action"
                              disabled={submitting}
                              onClick={() => void handleAutoGenerate(false)}
                            >
                              {submitting ? '生成中...' : '生成草稿'}
                            </button>
                            <button
                              type="button"
                              class="btn-inline-primary"
                              disabled={submitting}
                              onClick={() => void handleAutoGenerate(true)}
                            >
                              {submitting ? '生成中...' : '⚡ 生成并直接发布'}
                            </button>
                          </div>
                        </div>
                      )}

                      <label class="compact-field">
                        <span>故事标题 *</span>
                        <input
                          type="text"
                          required
                          placeholder="例如：小猪鲁本与森林木桥"
                          value={title}
                          onInput={(e) => setTitle((e.target as HTMLInputElement).value)}
                          class="compact-input"
                        />
                      </label>

                      <label class="compact-field">
                        <span>故事主题</span>
                        <input
                          type="text"
                          list="preset-themes-list"
                          value={theme}
                          onInput={(e) => setTheme((e.target as HTMLInputElement).value)}
                          class="compact-input"
                        />
                        <datalist id="preset-themes-list">
                          {PRESET_THEMES.map((t) => (
                            <option key={t} value={t} />
                          ))}
                        </datalist>
                      </label>

                      <label class="compact-field">
                        <span>故事正文（每行一句，或按句号自动拆页）*</span>
                        <textarea
                          class="compact-textarea"
                          rows={6}
                          placeholder={'太阳升起来了，杰西带着粉红色的小猪鲁本走进大森林。\n他们来到小河边，稳稳地走过木桥。'}
                          value={rawText}
                          onInput={(e) => setRawText((e.target as HTMLTextAreaElement).value)}
                        />
                      </label>

                      <div class="drawer-quick-row">
                        <input
                          ref={batchImgInputRef}
                          type="file"
                          accept="image/*"
                          multiple
                          style={{ display: 'none' }}
                          onChange={(e) => handleBatchImagesUpload((e.target as HTMLInputElement).files)}
                        />
                        <input
                          ref={step2AnimInputRef}
                          type="file"
                          accept="video/mp4,video/webm,image/gif,image/webp"
                          style={{ display: 'none' }}
                          onChange={(e) => {
                            const file = (e.target as HTMLInputElement).files?.[0] || null;
                            setPendingAnimationFile(file);
                          }}
                        />
                        <button
                          type="button"
                          class="btn-inline-action"
                          onClick={() => batchImgInputRef.current?.click()}
                        >
                          🖼️ 批量上传配图 ({Object.keys(pageImages).length}/{sentences.length || 0})
                        </button>
                        <button
                          type="button"
                          class="btn-inline-action"
                          onClick={() => step2AnimInputRef.current?.click()}
                        >
                          {pendingAnimationFile
                            ? `🎬 已选动画：${pendingAnimationFile.name}`
                            : '🎬 上传专属动画（可选）'}
                        </button>
                      </div>

                      <div class="drawer-quick-row">
                        {pendingAnimationFile && (
                          <button
                            type="button"
                            class="btn-inline-action"
                            onClick={() => setPendingAnimationFile(null)}
                          >
                            清除已选动画
                          </button>
                        )}
                        <button
                          type="button"
                          class="btn-inline-primary"
                          onClick={() => setExpandedStep('step3')}
                        >
                          下一步：去预览并发布 →
                        </button>
                      </div>
                    </div>
                  )}

                  <button
                    type="button"
                    class={`step-action-btn step-action-outline ${expandedStep === 'step3' ? 'is-expanded' : ''}`}
                    onClick={() => setExpandedStep(expandedStep === 'step3' ? null : 'step3')}
                  >
                    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z" />
                      <circle cx="12" cy="12" r="3" />
                    </svg>
                    <span>3 &nbsp;预览并发布到书架</span>
                  </button>

                  {expandedStep === 'step3' && (
                    <div class="step-drawer-card">
                      <div class="preview-mini-header">
                        <span>
                          <strong>{title || '未命名故事预览'}</strong> · 第 {previewPageIndex + 1} / {Math.max(1, sentences.length)} 页
                        </span>
                        <button
                          type="button"
                          class="text-link-btn"
                          onClick={() => setPreviewShowPinyin(!previewShowPinyin)}
                        >
                          {previewShowPinyin ? '隐藏常驻拼音' : '开启常驻拼音'}
                        </button>
                      </div>

                      <div class="preview-mini-stage">
                        <div class="reader-illustration-compact" style={{ maxHeight: '140px' }}>
                          {activePreviewImage ? (
                            <img src={activePreviewImage} alt="预览配图" class="story-image" />
                          ) : (
                            <div class="story-image-placeholder">
                              <span>第 {previewPageIndex + 1} 页插画预览（发布时支持自动配图）</span>
                            </div>
                          )}
                        </div>

                        <SentenceDisplay
                          tokens={activePreviewTokens}
                          activeWordIndex={null}
                          activeTokenId={previewActiveTokenId}
                          showPinyinHint={previewShowPinyin}
                          reviewChars={reviewCharSet}
                          newChars={newCharSet}
                          showLegend={false}
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

                        <div class="preview-mini-nav">
                          <button
                            type="button"
                            class="btn-inline-action"
                            disabled={previewPageIndex <= 0}
                            onClick={() => setPreviewPageIndex((p) => Math.max(0, p - 1))}
                          >
                            ‹ 上一页
                          </button>
                          <button
                            type="button"
                            class="btn-inline-action"
                            onClick={() => void audioPlayer.playUrlOrFallback('', activePreviewSentence)}
                          >
                            🔊 试听
                          </button>
                          <button
                            type="button"
                            class="btn-inline-action"
                            disabled={previewPageIndex >= Math.max(0, sentences.length - 1)}
                            onClick={() =>
                              setPreviewPageIndex((p) => Math.min(Math.max(0, sentences.length - 1), p + 1))
                            }
                          >
                            下一页 ›
                          </button>
                        </div>
                      </div>

                      {sentences.length > 0 && (
                        <div class="page-inspector-compact">
                          {sentences.map((sent, idx) => {
                            const hasImg = Boolean(pageImages[idx]);
                            return (
                              <div
                                key={idx}
                                class={`inspector-compact-row ${previewPageIndex === idx ? 'is-active' : ''}`}
                                onClick={() => setPreviewPageIndex(idx)}
                              >
                                <span class="inspector-page-num">P{idx + 1}</span>
                                <span class="inspector-sent-text">{sent}</span>
                                <label
                                  class="inspector-upload-tag"
                                  onClick={(e) => e.stopPropagation()}
                                >
                                  {hasImg ? '已配图' : '+配图'}
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
                      )}

                      <button
                        type="button"
                        class="step-action-btn step-action-primary"
                        disabled={submitting || sentences.length === 0}
                        onClick={() => void handlePublishQuickStory()}
                      >
                        {submitting
                          ? '⏳ 正在打包并预缓存语音...'
                          : `🚀 确认发布到书架（共 ${sentences.length} 页${pendingAnimationFile ? ' + 专属动画' : ''}）`}
                      </button>
                    </div>
                  )}
                </div>

                <details class="prompt-collapse-details">
                  <summary class="prompt-collapse-summary">
                    <span>查看完整提示词</span>
                    <svg class="chevron-icon" width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <polyline points="6 9 12 15 18 9" />
                    </svg>
                  </summary>
                  <div class="prompt-collapse-body">
                    <textarea
                      readOnly
                      class="compact-textarea prompt-mono-box"
                      rows={10}
                      value={plan?.story_generation_prompt || '暂无提示词数据'}
                      onClick={(e) => (e.target as HTMLTextAreaElement).select()}
                    />
                  </div>
                </details>
              </section>
            </main>
          )}

          {/* 主视图 2：书架管理（支持给每篇故事单独上传/替换/移除 1 对 1 专属动画） */}
          {activeTab === 'bookshelf' && (
            <main class="guardian-main-body">
              <div class="section-caption-row">
                <h2 class="section-caption">故事书架管理（共 {sortedStories.length} 篇，新书在前）</h2>
                <button
                  type="button"
                  class="text-link-btn"
                  disabled={pullingGist}
                  onClick={() => void handlePullFromGist(false)}
                >
                  {pullingGist ? '拉取中...' : '从 Gist 同步'}
                </button>
              </div>

              {sortedStories.length === 0 ? (
                <p class="empty-hint-text">书架暂无故事，可在「识字计划」按三步流程导入或从 Gist 拉取。</p>
              ) : (
                <div class="story-manage-list">
                  {sortedStories.map((st) => {
                    const hasAnim = Boolean(resolveStoryAnimationUrl(st));
                    const isUploading = uploadingAnimStoryId === st.id;
                    return (
                      <div key={st.id} class="story-manage-row">
                        <div class="story-manage-info">
                          <strong>{st.title}</strong>
                          <span>
                            v{st.version_number} · {st.theme} · 共 {st.sentence_count} 页
                            {hasAnim ? ' · 🎬 已配动画' : ''}
                          </span>
                        </div>
                        <div style={{ display: 'flex', alignItems: 'center', gap: '6px', flexShrink: 0 }}>
                          <label class="inspector-upload-tag" style={{ cursor: isUploading ? 'wait' : 'pointer' }}>
                            {isUploading ? '上传中...' : hasAnim ? '换动画' : '+动画'}
                            <input
                              type="file"
                              accept="video/mp4,video/webm,image/gif,image/webp"
                              disabled={isUploading}
                              style={{ display: 'none' }}
                              onChange={(e) =>
                                void handleUploadStoryAnimationDirect(
                                  st,
                                  (e.target as HTMLInputElement).files?.[0]
                                )
                              }
                            />
                          </label>
                          {hasAnim && (
                            <button
                              type="button"
                              class="text-link-btn"
                              disabled={isUploading}
                              onClick={() => void handleDeleteStoryAnimationDirect(st)}
                            >
                              删动画
                            </button>
                          )}
                          <button
                            type="button"
                            class="btn-danger-outline"
                            onClick={() => void handleDeleteStory(st)}
                          >
                            删除
                          </button>
                        </div>
                      </div>
                    );
                  })}
                </div>
              )}
            </main>
          )}

          {activeTab === 'backup' && (
            <main class="guardian-main-body">
              <h2 class="section-caption">数据备份与离线导入</h2>
              <div class="three-steps-stack">
                <input
                  ref={zipInputRef}
                  type="file"
                  accept=".zip"
                  style={{ display: 'none' }}
                  onChange={(e) => void handleZipImport((e.target as HTMLInputElement).files?.[0])}
                />
                <button
                  type="button"
                  class="step-action-btn step-action-outline"
                  onClick={() => zipInputRef.current?.click()}
                >
                  📦 导入离线 ZIP 故事包
                </button>

                <a
                  href={`/api/v1/admin/export?format=json&pin=${encodeURIComponent(pin)}`}
                  target="_blank"
                  rel="noreferrer"
                  class="step-action-btn step-action-outline"
                  style={{ textDecoration: 'none' }}
                >
                  ⬇️ 导出 JSON 识字学习记录
                </a>

                <a
                  href={`/api/v1/admin/export?format=sqlite&pin=${encodeURIComponent(pin)}`}
                  target="_blank"
                  rel="noreferrer"
                  class="step-action-btn step-action-outline"
                  style={{ textDecoration: 'none' }}
                >
                  💾 备份 SQLite 数据库文件
                </a>

                {activeChild && (
                  <button
                    type="button"
                    class="step-action-btn step-action-danger"
                    onClick={() => void handleDeleteCurrentChild()}
                  >
                    🗑️ 删除当前读者档案（{activeChild.name}）
                  </button>
                )}
              </div>
            </main>
          )}

          {activeTab === 'upgrade' && (
            <main class="guardian-main-body">
              <h2 class="section-caption">系统状态与在线升级</h2>

              {healthInfo && (
                <div class="upgrade-stats-row">
                  <div class="upgrade-stat-pill">
                    <span>当前版本</span>
                    <strong>{healthInfo.version}</strong>
                  </div>
                  <div class="upgrade-stat-pill">
                    <span>架构</span>
                    <strong>{healthInfo.arch}</strong>
                  </div>
                  <div class="upgrade-stat-pill">
                    <span>可用空间</span>
                    <strong>{healthInfo.free_disk_mb} MB</strong>
                  </div>
                </div>
              )}

              <label class="compact-field" style={{ marginTop: '12px' }}>
                <span>GitHub 加速镜像前缀（直连可留空）</span>
                <input
                  type="text"
                  placeholder="https://ghproxy.net/ 或留空"
                  value={mirrorUrl}
                  onInput={(e) => setMirrorUrl((e.target as HTMLInputElement).value)}
                  class="compact-input"
                />
              </label>

              <div class="three-steps-stack" style={{ marginTop: '14px' }}>
                <button
                  type="button"
                  class="step-action-btn step-action-outline"
                  disabled={checkingUpdate || applyingUpdate}
                  onClick={() => void handleCheckUpdate()}
                >
                  {checkingUpdate ? '🔍 正在连接 GitHub 检查...' : '🔍 检查 GitHub 最新版本'}
                </button>

                <button
                  type="button"
                  class="step-action-btn step-action-primary"
                  disabled={applyingUpdate}
                  onClick={() => void handleApplyUpdate()}
                >
                  {applyingUpdate ? '⏳ 正在下载并升级重启...' : '🚀 立即从 GitHub 下载并升级'}
                </button>
              </div>

              {updateCheckInfo && (
                <div class="step-drawer-card" style={{ marginTop: '14px' }}>
                  <div style={{ fontSize: '0.9rem' }}>
                    当前 <code>{updateCheckInfo.current_version}</code> · 最新{' '}
                    <code>{updateCheckInfo.latest_version}</code>{' '}
                    {updateCheckInfo.has_update ? (
                      <strong style={{ color: '#15803d' }}>（有新版本可升级）</strong>
                    ) : (
                      <span style={{ color: 'var(--text-pinyin)' }}>（已是最新）</span>
                    )}
                  </div>
                  {updateCheckInfo.release_notes && (
                    <pre class="release-notes-box">{updateCheckInfo.release_notes}</pre>
                  )}
                </div>
              )}
            </main>
          )}
        </div>

        <nav class="guardian-bottom-nav" aria-label="监护人工作台导航">
          <button
            type="button"
            class={`bottom-nav-item ${activeTab === 'plan' ? 'is-active' : ''}`}
            onClick={() => setActiveTab('plan')}
          >
            识字计划
          </button>
          <button
            type="button"
            class={`bottom-nav-item ${activeTab === 'bookshelf' ? 'is-active' : ''}`}
            onClick={() => setActiveTab('bookshelf')}
          >
            书架
          </button>
          <button
            type="button"
            class={`bottom-nav-item ${activeTab === 'backup' ? 'is-active' : ''}`}
            onClick={() => setActiveTab('backup')}
          >
            备份
          </button>
          <button
            type="button"
            class={`bottom-nav-item ${activeTab === 'upgrade' ? 'is-active' : ''}`}
            onClick={() => setActiveTab('upgrade')}
          >
            升级
          </button>
        </nav>
      </div>

      <dialog
        ref={menuDialogRef}
        class="reader-settings-dialog"
        onClick={(e) => {
          if (e.target === menuDialogRef.current) {
            setShowHeaderMenu(false);
          }
        }}
        onCancel={() => setShowHeaderMenu(false)}
      >
        <div class="reader-settings-sheet">
          <div class="settings-sheet-header">
            <strong>工作台设置与导航</strong>
            <button
              type="button"
              class="reader-icon-btn"
              onClick={() => setShowHeaderMenu(false)}
              aria-label="关闭菜单"
            >
              ✕
            </button>
          </div>

          <label class="compact-field">
            <span>监护人 PIN 码</span>
            <input
              type="password"
              value={pin}
              onInput={(e) => setPin((e.target as HTMLInputElement).value)}
              class="compact-input"
            />
          </label>

          <button
            type="button"
            class="step-action-btn step-action-primary"
            onClick={() => {
              setShowHeaderMenu(false);
              onClose();
            }}
          >
            ← 返回儿童阅读书架
          </button>
        </div>
      </dialog>
    </div>
  );
}
