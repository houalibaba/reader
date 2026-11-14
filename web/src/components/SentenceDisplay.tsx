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
            aria-label={isPunct ? tok.character : `汉字 ${tok.character}，所属词 ${tok.word}`}
          >
            {(showPinyinHint || isCharSelected) && !isPunct && hasValidPinyin && (
              <span class="char-pinyin">{tok.pinyin}</span>
            )}
            <span class="char-glyph">{tok.character}</span>
          </button>
        );
      })}
    </div>
  );
}
