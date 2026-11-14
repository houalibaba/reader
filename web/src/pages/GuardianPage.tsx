import { useEffect, useState } from 'preact/hooks';
import {
  apiClient,
  ChildProfile,
  StoryPlanResponse,
  StorySummary,
  UpdateCheckResult,
  VocabularyEntry
} from '../api/client';

interface GuardianPageProps {
  activeChild: ChildProfile | null;
  onClose: () => void;
  onChildDeleted: () => void;
}

const PRESET_THEMES = [
  '勇气与探索（森林过河）',
  '太空与星际（月亮奇遇）',
  '海洋与沙滩（寻宝之旅）',
  '恐龙与远古（高山探秘）',
  '友谊与分享（果园野餐）',
  '日常与好习惯（快乐成长）',
  '我的世界·故事模式',
  'custom'
];

function fileToBase64(file: File): Promise<{ filename: string; data_base64: string }> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => {
      const result = String(reader.result || '');
      const commaIdx = result.indexOf(',');
      const b64 = commaIdx >= 0 ? result.slice(commaIdx + 1) : result;
      resolve({ filename: file.name, data_base64: b64 });
    };
    reader.onerror = () => reject(reader.error);
    reader.readAsDataURL(file);
  });
}

export function GuardianPage({ activeChild, onClose, onChildDeleted }: GuardianPageProps) {
  const [pin, setPin] = useState<string>('1234');
  const [vocab, setVocab] = useState<VocabularyEntry[]>([]);
  const [stories, setStories] = useState<StorySummary[]>([]);
  const [storyPlan, setStoryPlan] = useState<StoryPlanResponse | null>(null);
  const [statusMsg, setStatusMsg] = useState<string>('');
  const [copyMsg, setCopyMsg] = useState<string>('');
  const [generating, setGenerating] = useState<boolean>(false);

  // Theme selection for auto story generation
  const [selectedThemePreset, setSelectedThemePreset] = useState<string>(PRESET_THEMES[0]);
  const [customTheme, setCustomTheme] = useState<string>('');

  // Quick Story Creation / Update Form State + User-Prepared Custom Images
  const [selectedStoryId, setSelectedStoryId] = useState<string>('');
  const [storyTitle, setStoryTitle] = useState<string>('');
  const [storyTheme, setStoryTheme] = useState<string>(PRESET_THEMES[0]);
  const [storyDesc, setStoryDesc] = useState<string>('');
  const [rawText, setRawText] = useState<string>('');
  const [preparedFiles, setPreparedFiles] = useState<File[]>([]);

  // Self-Update State
  const [currentVersion, setCurrentVersion] = useState<string>('v0.3.0');
  const [currentArch, setCurrentArch] = useState<string>('');
  const [mirrorUrl, setMirrorUrl] = useState<string>('');
  const [updateInfo, setUpdateInfo] = useState<UpdateCheckResult | null>(null);
  const [checkingUpdate, setCheckingUpdate] = useState<boolean>(false);
  const [applyingUpdate, setApplyingUpdate] = useState<boolean>(false);

  // Optional Cloud LLM Settings
  const [llmApiKey, setLlmApiKey] = useState<string>('');
  const [llmModel, setLlmModel] = useState<string>('nvidia/nemotron-3-ultra-550b-a55b:free');

  const effectiveAutoTheme =
    selectedThemePreset === 'custom' ? customTheme.trim() || '奇幻冒险' : selectedThemePreset;

  useEffect(() => {
    void apiClient.getHealth().then((h) => {
      if (h.version) setCurrentVersion(h.version);
      if (h.arch) setCurrentArch(h.arch);
    }).catch(() => {});
    void apiClient.listStories(activeChild?.id).then(setStories).catch(() => setStories([]));
    if (activeChild) {
      apiClient.getVocabulary(activeChild.id).then(setVocab).catch(() => setVocab([]));
      apiClient.getStoryPlan(activeChild.id).then(setStoryPlan).catch(() => setStoryPlan(null));
    }
  }, [activeChild]);

  const handleSelectStoryMode = (storyId: string) => {
    setSelectedStoryId(storyId);
    if (!storyId) {
      setStoryTitle('');
      setStoryTheme(effectiveAutoTheme);
      setStoryDesc('');
      return;
    }
    const found = stories.find((s) => s.id === storyId);
    if (found) {
      setStoryTitle(found.title);
      setStoryTheme(found.theme || effectiveAutoTheme);
      setStoryDesc(found.description || '');
    }
  };

  const handlePreparedImagesChange = (e: Event) => {
    const input = e.target as HTMLInputElement;
    if (!input.files) {
      setPreparedFiles([]);
      return;
    }
    const files = Array.from(input.files).sort((a, b) =>
      a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' })
    );
    setPreparedFiles(files);
  };

  const handleCopyPrompt = async () => {
    if (!storyPlan?.story_generation_prompt) return;
    try {
      await navigator.clipboard.writeText(
        `【故事主题】：${effectiveAutoTheme}\n` + storyPlan.story_generation_prompt
      );
      setCopyMsg('✅ 含所选主题的提示词已复制到剪贴板！');
    } catch {
      setCopyMsg('请全选下方文本框复制提示词。');
    }
  };

  const handleFullAutoGenerate = async (autoPublish: boolean) => {
    setGenerating(true);
    try {
      setStatusMsg(
        autoPublish
          ? `⚡ 正在围绕主题【${effectiveAutoTheme}】全自动生成故事、绘制无文字剧透绘本插图并上架...`
          : `⚡ 正在围绕主题【${effectiveAutoTheme}】全自动生成故事草稿...`
      );
      const res = await apiClient.autoGenerateStory(pin, {
        child_id: activeChild?.id,
        theme: effectiveAutoTheme,
        auto_publish: autoPublish,
        llm_api_key: llmApiKey.trim() || undefined,
        llm_model: llmModel.trim() || undefined
      });
      setStoryTitle(res.title);
      setStoryTheme(res.theme);
      setStoryDesc(res.description);
      setRawText(res.raw_text);

      if (autoPublish && res.published) {
        setStatusMsg(
          `🎉 全自动生成并上架成功：《${res.title}》（主题：${res.theme}，共 ${res.sentence_count} 句 + 配套绘本插图）已上架书架！`
        );
        const updatedStories = await apiClient.listStories(activeChild?.id);
        setStories(updatedStories);
      } else {
        setStatusMsg(
          `✨ 已按主题【${res.theme}】自动生成故事草稿《${res.title}》并填入下方编辑器，你可以上传自己准备的配图或直接点击上架！`
        );
      }
    } catch (err: any) {
      setStatusMsg(`自动生成失败：${err.message || String(err)}`);
    } finally {
      setGenerating(false);
    }
  };

  const handleQuickPublishStory = async (e: Event) => {
    e.preventDefault();
    if (!storyTitle.trim() || !rawText.trim()) {
      setStatusMsg('请填写故事标题和故事正文句子！');
      return;
    }
    try {
      setStatusMsg(
        preparedFiles.length > 0
          ? `正在上传你准备的 ${preparedFiles.length} 张配图、自动分句注音并发布到书架...`
          : '正在自动分句、校验多音字注音、生成配套插图并发布到书架...'
      );
      const customImages =
        preparedFiles.length > 0 ? await Promise.all(preparedFiles.map(fileToBase64)) : undefined;

      const res = await apiClient.quickCreateOrUpdateStory(pin, {
        story_id: selectedStoryId || undefined,
        title: storyTitle.trim(),
        theme: storyTheme.trim() || effectiveAutoTheme,
        description: storyDesc.trim(),
        raw_text: rawText.trim(),
        custom_images: customImages
      });
      setStatusMsg(
        `🎉 发布成功：《${storyTitle}》（故事 ID: ${res.imported_story_id}，版本 v${res.version_number}，共 ${res.sentence_count} 句，已绑定 ${
          preparedFiles.length > 0 ? `你上传的 ${preparedFiles.length} 张自定义配图` : '自动绘本插图'
        }）已上架书架！`
      );
      setRawText('');
      setPreparedFiles([]);
      const updatedStories = await apiClient.listStories(activeChild?.id);
      setStories(updatedStories);
    } catch (err: any) {
      setStatusMsg(`发布失败：${err.message || String(err)}`);
    }
  };

  const handleImportZip = async (e: Event) => {
    const input = e.target as HTMLInputElement;
    if (!input.files || input.files.length === 0) return;
    const formData = new FormData();
    formData.append('package', input.files[0]);

    setStatusMsg('正在导入故事包...');
    const res = await fetch('/api/v1/admin/import', {
      method: 'POST',
      headers: { 'X-Guardian-Pin': pin },
      body: formData
    });
    const data = await res.json();
    if (res.ok) {
      setStatusMsg(`导入成功：故事 ${data.imported_story_id}（版本 ${data.imported_version_id}，共 ${data.sentence_count} 句）`);
      const updatedStories = await apiClient.listStories(activeChild?.id);
      setStories(updatedStories);
    } else {
      setStatusMsg(`导入失败：${data.error || data.detail || res.statusText}`);
    }
  };


  const handleCheckUpdate = async () => {
    setCheckingUpdate(true);
    setStatusMsg('正在连接 GitHub 检查最新版本...');
    try {
      const res = await apiClient.checkUpdate(pin, mirrorUrl.trim() || undefined);
      setUpdateInfo(res);
      if (res.has_update) {
        setStatusMsg(`🎉 发现新版本 ${res.latest_version}（当前 ${res.current_version}），可点击“立即升级”自动更新。`);
      } else {
        setStatusMsg(`✅ 当前已是最新版本（${res.current_version}）。`);
      }
    } catch (err) {
      setStatusMsg(`检查更新失败：${err instanceof Error ? err.message : String(err)}`);
    } finally {
      setCheckingUpdate(false);
    }
  };

  const handleApplyUpdate = async () => {
    setApplyingUpdate(true);
    setStatusMsg('正在从 GitHub 下载新版本并校验 SHA256，请勿关闭页面...');
    try {
      const res = await apiClient.applyUpdate(pin, mirrorUrl.trim() || undefined);
      setStatusMsg(`🚀 ${res.message}`);
      if (res.status === 'restarting') {
        const start = Date.now();
        const poll = setInterval(async () => {
          try {
            const h = await apiClient.getHealth();
            if (h.status === 'ok' && h.version === res.new_version) {
              clearInterval(poll);
              window.location.reload();
            }
          } catch {
            // waiting for procd to respawn new binary
          }
          if (Date.now() - start > 30000) {
            clearInterval(poll);
            setApplyingUpdate(false);
            setStatusMsg('升级包已替换，若页面未自动刷新请手动刷新浏览器。');
          }
        }, 1500);
      } else {
        setApplyingUpdate(false);
      }
    } catch (err) {
      setApplyingUpdate(false);
      setStatusMsg(`自动升级失败：${err instanceof Error ? err.message : String(err)}`);
    }
  };

  const handleDeleteStory = async (storyId: string, title?: string) => {
    if (!storyId) return;
    const label = title ? `《${title}》` : storyId;
    setStatusMsg(`正在删除故事 ${label}...`);
    try {
      const res = await apiClient.deleteStory(pin, storyId);
      setStatusMsg(
        `🗑️ 已删除故事《${res.title || title || storyId}》${
          res.preserved_vocab_history ? '（已自动保留孩子的识字与遗忘曲线记录）' : ''
        }。`
      );
      if (selectedStoryId === storyId) {
        setSelectedStoryId('');
        setStoryTitle('');
        setStoryDesc('');
      }
      const updatedStories = await apiClient.listStories(activeChild?.id);
      setStories(updatedStories);
    } catch (err: any) {
      setStatusMsg(`删除故事失败：${err.message || String(err)}`);
    }
  };

  const handleDeleteChildData = async () => {
    if (!activeChild) return;
    const res = await fetch(`/api/v1/children/${encodeURIComponent(activeChild.id)}`, {
      method: 'DELETE',
      headers: { 'X-Guardian-Pin': pin }
    });
    if (res.ok) {
      setStatusMsg('儿童档案及关联行为数据已彻底清除。');
      onChildDeleted();
    } else {
      const err = await res.json();
      setStatusMsg(`删除失败：${err.error || err.detail || res.statusText}`);
    }
  };

  return (
    <div class="guardian-panel">
      <div class="guardian-header">
        <h2>🔒 监护人管理与全自动故事工作台</h2>
        <button type="button" class="btn-secondary" onClick={onClose}>
          ← 返回阅读器书架
        </button>
      </div>

      <div class="guardian-section">
        <label>
          <strong>监护人 PIN 码：</strong>
          <input
            type="password"
            value={pin}
            onInput={(e) => setPin((e.target as HTMLInputElement).value)}
            class="input-field"
          />
        </label>
        {statusMsg && <p class="status-notice">{statusMsg}</p>}
      </div>

      {activeChild && storyPlan && (
        <div class="guardian-section">
          <h3>⚡ 一键全自动生成定制新故事（基于【{activeChild.name}】的阅读历史）</h3>

          <div class="form-row" style={{ marginBottom: '16px', alignItems: 'center' }}>
            <label>
              <strong>🎨 选择新故事主题：</strong>
              <select
                value={selectedThemePreset}
                onChange={(e) => {
                  const val = (e.target as HTMLSelectElement).value;
                  setSelectedThemePreset(val);
                  if (val !== 'custom') setStoryTheme(val);
                }}
                class="input-field"
              >
                {PRESET_THEMES.map((t) =>
                  t === 'custom' ? (
                    <option key="custom" value="custom">
                      ✍️ 自定义主题...
                    </option>
                  ) : (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  )
                )}
              </select>
            </label>

            {selectedThemePreset === 'custom' && (
              <label>
                输入自定义主题：
                <input
                  type="text"
                  placeholder="例如：我的世界红石探险..."
                  value={customTheme}
                  onInput={(e) => {
                    const val = (e.target as HTMLInputElement).value;
                    setCustomTheme(val);
                    setStoryTheme(val);
                  }}
                  class="input-field"
                />
              </label>
            )}
          </div>

          <div class="copy-feedback" style={{ marginBottom: '12px', lineHeight: '1.6' }}>
            📊 <strong>10分钟科学阅读配比（360~450字 / 16~20句）</strong>：已掌握字词库{' '}
            <strong>{storyPlan.familiar_anchors.length}</strong> 项（+高频熟字基座，占 90%~92%）｜ 艾宾浩斯遗忘曲线待复习生字{' '}
            <strong>{storyPlan.review_targets.length + storyPlan.observe_targets.length}</strong> 个 ｜ 科学配额新拓展生字{' '}
            <strong>{storyPlan.new_candidates.length}</strong> 个（生字合计占 8%~10%）
          </div>

          <label class="block-label" style={{ marginBottom: '12px' }}>
            📋 自动生成的 10 分钟科学配比 AI 提示词（包含全部已掌握字词 + 遗忘曲线复习生字 + 新生字）：
            <textarea
              rows={7}
              readOnly
              value={`【故事主题】：${effectiveAutoTheme}\n${storyPlan.story_generation_prompt}`}
              class="textarea-field"
              style={{ fontSize: '13px', background: '#f8fafc' }}
            />
          </label>

          <div class="guardian-actions">
            <button
              type="button"
              class="btn-primary"
              disabled={generating}
              onClick={() => handleFullAutoGenerate(true)}
            >
              🚀 按所选主题一键全自动生成（含配套插图）并上架
            </button>
            <button
              type="button"
              class="btn-secondary"
              disabled={generating}
              onClick={() => handleFullAutoGenerate(false)}
            >
              ✨ 生成草稿到下方编辑器（可搭配你自己准备的图片发布）
            </button>
            <button type="button" class="btn-secondary" onClick={handleCopyPrompt}>
              📋 复制完整 AI 提示词
            </button>
          </div>
          {copyMsg && <p class="copy-feedback">{copyMsg}</p>}
        </div>
      )}

      <div class="guardian-section">
        <h3>📝 故事编辑器（支持上传你自己准备的配图、微调文本或更新已有故事）</h3>
        <form onSubmit={handleQuickPublishStory} class="quick-story-form">
          <div class="form-row">
            <label>
              操作模式：
              <select
                value={selectedStoryId}
                onChange={(e) => handleSelectStoryMode((e.target as HTMLSelectElement).value)}
                class="input-field"
              >
                <option value="">➕ 创建全新故事</option>
                {stories.map((st) => (
                  <option key={st.id} value={st.id}>
                    🔄 更新已有故事：《{st.title}》（当前 v{st.version_number} -&gt; 自动升级为 v{st.version_number + 1}）
                  </option>
                ))}
              </select>
            </label>

            <label>
              故事标题：
              <input
                type="text"
                placeholder="例如：我的世界：小猪鲁本与五彩护身符"
                value={storyTitle}
                onInput={(e) => setStoryTitle((e.target as HTMLInputElement).value)}
                class="input-field"
                required
              />
            </label>

            <label>
              故事主题：
              <input
                type="text"
                placeholder="例如：我的世界·故事模式"
                value={storyTheme}
                onInput={(e) => setStoryTheme((e.target as HTMLInputElement).value)}
                class="input-field"
              />
            </label>
          </div>

          <label class="block-label">
            🖼️ 上传你自己准备好的故事配图（可选，支持一次多选 .jpg/.png/.webp/.svg；按文件名排序自动对应到每一句或每一幕，不传则自动生成绘本插图）：
            <input
              type="file"
              accept="image/*"
              multiple
              onChange={handlePreparedImagesChange}
              class="input-field full-width"
            />
          </label>
          {preparedFiles.length > 0 && (
            <p class="copy-feedback">
              ✅ 已选择 {preparedFiles.length} 张自定义配图（顺序：{preparedFiles.map((f) => f.name).join(' -> ')}）
            </p>
          )}

          <label class="block-label">
            故事正文（自动分句并完成多音字语境注音）：
            <textarea
              rows={6}
              placeholder="点击上方“✨ 生成草稿到下方编辑器”或直接在此粘贴故事文本..."
              value={rawText}
              onInput={(e) => setRawText((e.target as HTMLTextAreaElement).value)}
              class="textarea-field"
              required
            />
          </label>

          <div class="guardian-actions">
            <button type="submit" class="btn-primary">
              🚀 自动分词注音、绑定配图并上架到书架
            </button>
            {selectedStoryId && (
              <button
                type="button"
                class="btn-danger"
                onClick={() => handleDeleteStory(selectedStoryId, storyTitle)}
              >
                🗑️ 删除当前所选故事
              </button>
            )}
          </div>
        </form>
      </div>

      <div class="guardian-section">
        <h3>📚 已上架故事管理（共 {stories.length} 篇，支持一键删除）</h3>
        {stories.length === 0 ? (
          <p class="copy-feedback">当前书架暂无故事。</p>
        ) : (
          <div style={{ display: 'flex', flexDirection: 'column', gap: '8px' }}>
            {stories.map((st) => (
              <div
                key={st.id}
                style={{
                  display: 'flex',
                  justifyContent: 'space-between',
                  alignItems: 'center',
                  padding: '8px 12px',
                  background: '#f8fafc',
                  borderRadius: '8px',
                  border: '1px solid #e2e8f0'
                }}
              >
                <span>
                  <strong>《{st.title}》</strong>（主题：{st.theme || '默认'} · 版本 v{st.version_number} · 共{' '}
                  {st.sentence_count} 句）
                </span>
                <button
                  type="button"
                  class="btn-danger"
                  onClick={() => handleDeleteStory(st.id, st.title)}
                >
                  🗑️ 删除故事
                </button>
              </div>
            ))}
          </div>
        )}
      </div>


      <div class="guardian-section">
        <h3>🔄 系统版本与在线自动升级（GitHub: houalibaba/Reader）</h3>
        <div class="form-row" style={{ alignItems: 'center' }}>
          <span>
            当前版本：<strong>{currentVersion}</strong>
            {currentArch ? ` (${currentArch})` : ''}
          </span>
          <label>
            GitHub 加速前缀（可选，国内直连慢时填写）：
            <input
              type="text"
              placeholder="例如 https://gh-proxy.com/"
              value={mirrorUrl}
              onInput={(e) => setMirrorUrl((e.target as HTMLInputElement).value)}
              class="input-field"
            />
          </label>
        </div>
        <div class="guardian-actions" style={{ marginTop: '10px' }}>
          <button
            type="button"
            class="btn-secondary"
            disabled={checkingUpdate || applyingUpdate}
            onClick={handleCheckUpdate}
          >
            {checkingUpdate ? '⏳ 检查中...' : '🔍 检查 GitHub 更新'}
          </button>
          {updateInfo?.has_update && (
            <button
              type="button"
              class="btn-primary"
              disabled={applyingUpdate}
              onClick={handleApplyUpdate}
            >
              {applyingUpdate ? '⏳ 正在下载并重启...' : `⬆️ 立即升级到 ${updateInfo.latest_version}`}
            </button>
          )}
        </div>
        {updateInfo && (
          <p class="copy-feedback" style={{ marginTop: '8px' }}>
            仓库：{updateInfo.repo} ｜ 最新版本：{updateInfo.latest_version}
            {updateInfo.asset_size ? ` ｜ 大小：${(updateInfo.asset_size / 1024 / 1024).toFixed(1)} MB` : ''}
            {updateInfo.release_notes ? ` ｜ 更新说明：${updateInfo.release_notes.slice(0, 120)}` : ''}
          </p>
        )}
      </div>

      <div class="guardian-section">
        <h3>📦 离线故事包 (.zip) 导入与数据备份</h3>
        <div class="guardian-actions">
          <label class="btn-secondary file-upload-btn">
            📦 导入离线故事包 (.zip)
            <input type="file" accept=".zip,application/json" onChange={handleImportZip} hidden />
          </label>
          <a
            href={`/api/v1/admin/export?pin=${encodeURIComponent(pin)}`}
            target="_blank"
            rel="noreferrer"
            class="btn-secondary"
          >
            💾 导出 JSON 备份
          </a>
          <a
            href={`/api/v1/admin/export?format=sqlite&pin=${encodeURIComponent(pin)}`}
            target="_blank"
            rel="noreferrer"
            class="btn-secondary"
          >
            🗄️ 导出 SQLite 快照
          </a>
          {activeChild && (
            <button type="button" class="btn-danger" onClick={handleDeleteChildData}>
              🗑️ 清除【{activeChild.name}】全部历史与派生状态
            </button>
          )}
        </div>
      </div>
    </div>
  );
}
