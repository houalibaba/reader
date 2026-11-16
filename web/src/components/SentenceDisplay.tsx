import { SentenceToken } from '../api/client';

interface SentenceDisplayProps {
  tokens: SentenceToken[];
  activeWordIndex: number | null;
  activeTokenId: string | null;
  showPinyinHint: boolean;
  onTokenClick: (token: SentenceToken) => void;
}

const PUNCTUATION = new Set(['，', '。', '！', '？', '、', '：', '；', '“', '”', '‘', '’', '（', '）']);

export function SentenceDisplay({
  tokens,
  activeWordIndex,
  activeTokenId,
  showPinyinHint,
  onTokenClick
}: SentenceDisplayProps) {
  return (
    <div
      class="sentence-tokens-wrap sentence-area"
      data-show-pinyin={showPinyinHint}
      role="region"
      aria-label="故事句子阅读区"
    >
      {tokens.map((tok) => {
        const isPunct = PUNCTUATION.has(tok.character);
        const isWordHighlighted = activeWordIndex !== null && tok.word_index === activeWordIndex && !isPunct;
        const isCharSelected = activeTokenId === tok.id;
        const hasValidPinyin = Boolean(tok.pinyin && tok.pinyin !== tok.character);

        return (
          <button
            key={tok.id}
            type="button"
            disabled={isPunct}
            data-punct={isPunct}
            onPointerDown={(e) => {
              if (isPunct) return;
              if (e.pointerType === 'mouse' && e.button !== 0) return;
              onTokenClick(tok);
            }}
            onClick={(e) => {
              // 键盘 Enter / Space 触发无障碍点读，避免与 onPointerDown 重复触发
              if (e.detail === 0 && !isPunct) {
                onTokenClick(tok);
              }
            }}
            class={[
              'char-hitbox',
              'char-token',
              isPunct ? 'char-punct' : 'char-interactive',
              isWordHighlighted ? 'word-highlighted' : '',
              isCharSelected ? 'char-selected is-speaking' : ''
            ]
              .filter(Boolean)
              .join(' ')}
            aria-label={isPunct ? tok.character : `汉字 ${tok.character}，所属词 ${tok.word}`}
          >
            {/* 始终保留拼音行占位高度（\u00A0），切换拼音显示时整句基线零抖动 */}
            <span
              class="char-pinyin"
              style={
                isCharSelected && !showPinyinHint && hasValidPinyin
                  ? {
                      position: 'absolute',
                      top: '-30px',
                      left: '50%',
                      transform: 'translateX(-50%)',
                      opacity: 1,
                      fontSize: '1.05rem',
                      fontWeight: 700,
                      background: 'var(--bg-card)',
                      color: 'var(--text-primary)',
                      border: '1.5px solid var(--accent-speak-ring)',
                      borderRadius: '10px',
                      padding: '2px 10px',
                      whiteSpace: 'nowrap',
                      boxShadow: 'var(--shadow-card)',
                      zIndex: 20,
                      pointerEvents: 'none'
                    }
                  : undefined
              }
            >
              {!isPunct && hasValidPinyin ? tok.pinyin : '\u00A0'}
            </span>
            <span class="char-glyph char-hanzi">{tok.character}</span>
          </button>
        );
      })}
    </div>
  );
}
