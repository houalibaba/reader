import { useMemo } from 'preact/hooks';
import { SentenceToken } from '../api/client';

interface SentenceDisplayProps {
  tokens: SentenceToken[];
  activeWordIndex: number | null;
  activeTokenId: string | null;
  showPinyinHint: boolean;
  reviewChars?: Set<string>;
  newChars?: Set<string>;
  showLegend?: boolean;
  onTokenClick: (token: SentenceToken) => void;
}

const PUNCTUATION = new Set(['，', '。', '！', '？', '、', '：', '；', '“', '”', '‘', '’', '（', '）', ',', '.', '!', '?']);

interface WordGroup {
  key: string;
  wordIndex: number | null;
  isPunctGroup: boolean;
  tokens: SentenceToken[];
}

export function SentenceDisplay({
  tokens,
  activeWordIndex,
  activeTokenId,
  showPinyinHint,
  reviewChars,
  newChars,
  showLegend = true,
  onTokenClick
}: SentenceDisplayProps) {
  // 按词语分组（同一 word_index 的连续汉字归为一个不可拆分的词组容器，附着在词尾的标点与该词同行不落单）
  const wordGroups = useMemo<WordGroup[]>(() => {
    const groups: WordGroup[] = [];
    for (let i = 0; i < tokens.length; i++) {
      const tok = tokens[i];
      const isPunct = PUNCTUATION.has(tok.character);
      if (isPunct) {
        // 将标点符号并入前一个词组尾部，防止行首出现孤立标点
        if (groups.length > 0) {
          groups[groups.length - 1].tokens.push(tok);
        } else {
          groups.push({
            key: `punct_${tok.id}_${i}`,
            wordIndex: null,
            isPunctGroup: true,
            tokens: [tok]
          });
        }
        continue;
      }

      const prev = groups[groups.length - 1];
      if (
        prev &&
        !prev.isPunctGroup &&
        prev.wordIndex !== null &&
        prev.wordIndex === tok.word_index &&
        !PUNCTUATION.has(prev.tokens[prev.tokens.length - 1].character)
      ) {
        prev.tokens.push(tok);
      } else {
        groups.push({
          key: `word_${tok.word_index}_${tok.id}_${i}`,
          wordIndex: tok.word_index,
          isPunctGroup: false,
          tokens: [tok]
        });
      }
    }
    return groups;
  }, [tokens]);

  return (
    <div
      class="sentence-card-glass sentence-area"
      data-show-pinyin={showPinyinHint ? 'true' : 'false'}
      role="region"
      aria-label="故事句子阅读区"
    >
      <div class="sentence-tokens-wrap">
        {wordGroups.map((group) => {
          const isGroupHighlighted =
            activeWordIndex !== null &&
            group.wordIndex !== null &&
            group.wordIndex === activeWordIndex;

          return (
            <span
              key={group.key}
              class={`word-group ${isGroupHighlighted ? 'word-group-active' : ''}`}
            >
              {group.tokens.map((tok) => {
                const isPunct = PUNCTUATION.has(tok.character);
                const isCharSelected = activeTokenId === tok.id;
                const hasValidPinyin = Boolean(tok.pinyin && tok.pinyin !== tok.character);
                // 严格执行：默认隐藏拼音，仅在点击该字或家长显式开启全局拼音时显示
                const shouldShowPinyin = !isPunct && hasValidPinyin && (showPinyinHint || isCharSelected);

                const isReviewChar = !isPunct && Boolean(reviewChars?.has(tok.character));
                const isNewChar = !isPunct && !isReviewChar && Boolean(newChars?.has(tok.character));
                const charRole = isReviewChar ? 'review' : isNewChar ? 'new' : 'normal';

                return (
                  <button
                    key={tok.id}
                    type="button"
                    disabled={isPunct}
                    data-punct={isPunct ? 'true' : 'false'}
                    data-selected={isCharSelected ? 'true' : 'false'}
                    data-char-role={charRole}
                    onPointerDown={(e) => {
                      if (isPunct) return;
                      if (e.pointerType === 'mouse' && e.button !== 0) return;
                      onTokenClick(tok);
                    }}
                    onClick={(e) => {
                      // 仅响应键盘无障碍触发，避免与 onPointerDown 重复触发
                      if (e.detail === 0 && !isPunct) {
                        onTokenClick(tok);
                      }
                    }}
                    class={[
                      'char-hitbox',
                      'char-token',
                      isPunct ? 'char-punct' : 'char-interactive',
                      isReviewChar ? 'char-role-review' : '',
                      isNewChar ? 'char-role-new' : '',
                      isCharSelected ? 'char-selected is-speaking' : ''
                    ]
                      .filter(Boolean)
                      .join(' ')}
                    aria-label={
                      isPunct
                        ? tok.character
                        : `${isReviewChar ? '复习字 ' : isNewChar ? '新字 ' : ''}汉字 ${tok.character}，所属词 ${tok.word}`
                    }
                  >
                    <span
                      class={`char-pinyin ${shouldShowPinyin ? 'is-visible' : 'is-hidden'}`}
                      aria-hidden={!shouldShowPinyin}
                    >
                      {shouldShowPinyin ? tok.pinyin : '\u00A0'}
                    </span>
                    <span class="char-glyph char-hanzi">{tok.character}</span>
                  </button>
                );
              })}
            </span>
          );
        })}
      </div>

      {showLegend && (
        <div class="sentence-legend-bar" aria-label="字色说明">
          <span class="legend-pill legend-pill-review">复习字</span>
          {newChars && newChars.size > 0 && (
            <span class="legend-pill legend-pill-new">本篇新字</span>
          )}
          <span class="legend-pill legend-pill-hint">其余字点一下才出拼音</span>
        </div>
      )}
    </div>
  );
}
