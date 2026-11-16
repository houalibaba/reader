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
    <div class="sentence-tokens-wrap" role="region" aria-label="故事句子阅读区">
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
            onClick={() => !isPunct && onTokenClick(tok)}
            class={[
              'char-hitbox',
              isPunct ? 'char-punct' : 'char-interactive',
              isWordHighlighted ? 'word-highlighted' : '',
              isCharSelected ? 'char-selected' : ''
            ].join(' ')}
            style={{
              position: 'relative',
              backgroundColor: isWordHighlighted ? '#ffe08a' : undefined,
              borderRadius: isWordHighlighted ? '12px' : undefined,
              transition: 'background-color 0.15s ease'
            }}
            aria-label={isPunct ? tok.character : `汉字 ${tok.character}，所属词 ${tok.word}`}
          >
            {/* 借鉴 v0.1.0：点字时在汉字正上方弹出悬浮拼音气泡，或开启全局拼音提示时常驻 */}
            {(showPinyinHint || isCharSelected) && !isPunct && hasValidPinyin && (
              <span
                class="char-pinyin"
                style={
                  isCharSelected && !showPinyinHint
                    ? {
                        position: 'absolute',
                        top: '-28px',
                        left: '50%',
                        transform: 'translateX(-50%)',
                        fontSize: '18px',
                        fontWeight: 600,
                        background: '#fff',
                        color: '#2b2118',
                        border: '1px solid #ead9c3',
                        borderRadius: '8px',
                        padding: '2px 8px',
                        whiteSpace: 'nowrap',
                        boxShadow: '0 4px 12px rgba(0, 0, 0, 0.12)',
                        zIndex: 20,
                        pointerEvents: 'none'
                      }
                    : undefined
                }
              >
                {tok.pinyin}
              </span>
            )}
            <span class="char-glyph">{tok.character}</span>
          </button>
        );
      })}
    </div>
  );
}
