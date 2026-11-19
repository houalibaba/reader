import { StorySummary } from '../api/client';

export interface StoryRewardAnimation {
  storyId: string;
  title: string;
  url: string;
  poster?: string;
  media_type: 'video' | 'image';
}

export interface ReadingQualityCheckInput {
  childId: string;
  storyId: string;
  storyTitle: string;
  animationUrl?: string;
  posterUrl?: string;
  totalSentences: number;
  /** 每页的有效活跃停留时长（毫秒，已扣除切后台/息屏时间） */
  pageActiveMs: Record<number, number>;
  /** 每页的字符数，用于动态计算该页最低有效停留时长 */
  pageCharCounts: Record<number, number>;
  /** 整篇故事的点字求助 + 听整句朗读互动次数 */
  interactionCount: number;
}

export interface ReadingUnlockOutcome {
  qualified: boolean;
  newlyUnlocked: boolean;
  alreadyUnlocked: boolean;
  reason: 'no_animation' | 'too_fast' | 'low_interaction' | 'already_unlocked' | 'ok';
  message: string;
  animation: StoryRewardAnimation | null;
}

type UnlockedStoriesStore = Record<string, string[]>;

const STORAGE_KEY = 'reader_story_animation_unlocks_v2';

/**
 * 统一的故事排序：新书排最上面
 * 优先按 created_at / updated_at / published_at 降序；其次按 id 自然降序与版本号降序
 */
export function sortStoriesNewestFirst(list: StorySummary[]): StorySummary[] {
  return [...list].sort((a, b) => {
    const aAny = a as any;
    const bAny = b as any;
    const timeA = Date.parse(aAny.created_at || aAny.updated_at || aAny.published_at || '');
    const timeB = Date.parse(bAny.created_at || bAny.updated_at || bAny.published_at || '');
    if (!Number.isNaN(timeA) && !Number.isNaN(timeB) && timeA !== timeB) {
      return timeB - timeA;
    }
    const idCmp = String(b.id || '').localeCompare(String(a.id || ''), undefined, {
      numeric: true
    });
    if (idCmp !== 0) return idCmp;
    if (
      typeof b.version_number === 'number' &&
      typeof a.version_number === 'number' &&
      b.version_number !== a.version_number
    ) {
      return b.version_number - a.version_number;
    }
    return 0;
  });
}

export function isAnimationImageUrl(url: string): boolean {
  return /\.(gif|webp|apng|png|jpg|jpeg)(\?.*)?$/i.test(url);
}

/**
 * 从 StorySummary 或 StoryVersionDetail 中统一提取 1 对 1 绑定的专属动画 URL
 * 兼容后端字段：animation_url | reward_animation_url | reward_animation.url
 */
export function resolveStoryAnimationUrl(story: any): string {
  if (!story || typeof story !== 'object') return '';
  const direct =
    story.animation_url ||
    story.reward_animation_url ||
    story.reward_animation?.url ||
    '';
  return typeof direct === 'string' ? direct.trim() : '';
}

export function buildStoryRewardAnimation(story: {
  id?: string;
  story_id?: string;
  title?: string;
  cover_url?: string;
  animation_url?: string;
  reward_animation?: { url?: string; title?: string; poster?: string; media_type?: string };
}): StoryRewardAnimation | null {
  const url = resolveStoryAnimationUrl(story);
  if (!url) return null;
  const storyId = String(story.story_id || story.id || '');
  const rawTitle = story.reward_animation?.title || (story.title ? `《${story.title}》专属动画` : '故事专属动画');
  const mediaType: 'video' | 'image' =
    story.reward_animation?.media_type === 'image' || isAnimationImageUrl(url) ? 'image' : 'video';

  return {
    storyId,
    title: rawTitle,
    url,
    poster: story.reward_animation?.poster || story.cover_url || undefined,
    media_type: mediaType
  };
}

function readUnlockedStore(): UnlockedStoriesStore {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return {};
    const parsed = JSON.parse(raw);
    return parsed && typeof parsed === 'object' ? (parsed as UnlockedStoriesStore) : {};
  } catch {
    return {};
  }
}

function writeUnlockedStore(store: UnlockedStoriesStore): void {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(store));
  } catch {
    // ignore quota errors
  }
}

export function getUnlockedStoryIdSet(childId?: string): Set<string> {
  const key = childId || 'default';
  const store = readUnlockedStore();
  const list = Array.isArray(store[key]) ? store[key] : [];
  return new Set(list);
}

export function isStoryAnimationUnlocked(childId: string | undefined, storyId: string): boolean {
  if (!storyId) return false;
  return getUnlockedStoryIdSet(childId).has(storyId);
}

function markStoryAnimationUnlocked(childId: string | undefined, storyId: string): void {
  const key = childId || 'default';
  const store = readUnlockedStore();
  const prev = Array.isArray(store[key]) ? store[key] : [];
  if (!prev.includes(storyId)) {
    store[key] = [...prev, storyId];
    writeUnlockedStore(store);
  }
}

/**
 * 1 对 1 防刷校验：
 * - 若本篇故事未上传专属动画（animationUrl 为空），直接静默跳过，不显示任何动画提示
 * - 若有专属动画，校验每页有效活跃停留时长（扣除切后台）与最低点读/听读互动次数
 * - 达标后永久解锁当前孩子在本篇故事的专属动画
 */
export function evaluateAndUnlockStoryAnimation(
  input: ReadingQualityCheckInput
): ReadingUnlockOutcome {
  const animUrl = (input.animationUrl || '').trim();
  if (!animUrl) {
    return {
      qualified: true,
      newlyUnlocked: false,
      alreadyUnlocked: false,
      reason: 'no_animation',
      message: '',
      animation: null
    };
  }

  const animation: StoryRewardAnimation = {
    storyId: input.storyId,
    title: `《${input.storyTitle}》专属动画`,
    url: animUrl,
    poster: input.posterUrl || undefined,
    media_type: isAnimationImageUrl(animUrl) ? 'image' : 'video'
  };

  const alreadyUnlocked = isStoryAnimationUnlocked(input.childId, input.storyId);
  if (alreadyUnlocked) {
    return {
      qualified: true,
      newlyUnlocked: false,
      alreadyUnlocked: true,
      reason: 'already_unlocked',
      message: '本篇故事的专属动画已解锁，可以随时点击重温！',
      animation
    };
  }

  const total = Math.max(1, input.totalSentences);
  let validPages = 0;
  let totalActiveMs = 0;

  for (let i = 0; i < total; i++) {
    const activeMs = Math.max(0, input.pageActiveMs[i] || 0);
    const charCount = Math.max(4, input.pageCharCounts[i] || 10);
    const minPageMs = Math.max(2200, Math.min(5500, charCount * 260));
    totalActiveMs += activeMs;
    if (activeMs >= minPageMs) {
      validPages += 1;
    }
  }

  const minRequiredPages = Math.max(1, Math.ceil(total * 0.7));
  const minTotalMs = Math.max(12000, total * 2000);

  if (validPages < minRequiredPages || totalActiveMs < minTotalMs) {
    return {
      qualified: false,
      newlyUnlocked: false,
      alreadyUnlocked: false,
      reason: 'too_fast',
      message: '翻得有点快哦！认真读完每一页，就能解锁本篇专属动画啦。',
      animation: null
    };
  }

  if (input.interactionCount < 1 && total >= 3) {
    return {
      qualified: false,
      newlyUnlocked: false,
      alreadyUnlocked: false,
      reason: 'low_interaction',
      message: '点一点不认识的字，或者听一听整句朗读，就能解锁本篇专属动画哦！',
      animation: null
    };
  }

  markStoryAnimationUnlocked(input.childId, input.storyId);
  return {
    qualified: true,
    newlyUnlocked: true,
    alreadyUnlocked: false,
    reason: 'ok',
    message: `太棒啦！认真读完故事，成功解锁《${input.storyTitle}》专属动画！`,
    animation
  };
}

/**
 * 监护人端：以 multipart/form-data 流式上传某篇故事的 1 对 1 专属动画
 */
export async function uploadStoryAnimation(
  pin: string,
  storyId: string,
  file: File
): Promise<{ animation_url: string }> {
  const form = new FormData();
  form.append('animation', file);
  const res = await fetch(`/api/v1/admin/stories/${encodeURIComponent(storyId)}/animation`, {
    method: 'POST',
    headers: {
      'X-Guardian-Pin': pin
    },
    body: form
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    throw new Error(data.error || data.detail || `HTTP ${res.status}`);
  }
  return { animation_url: data.animation_url || '' };
}

/**
 * 监护人端：删除某篇故事的 1 对 1 专属动画
 */
export async function deleteStoryAnimation(pin: string, storyId: string): Promise<void> {
  const res = await fetch(`/api/v1/admin/stories/${encodeURIComponent(storyId)}/animation`, {
    method: 'DELETE',
    headers: {
      'X-Guardian-Pin': pin
    }
  });
  if (!res.ok) {
    const data = await res.json().catch(() => ({}));
    throw new Error(data.error || data.detail || `HTTP ${res.status}`);
  }
}
