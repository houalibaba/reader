package content

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

type TokenManifest struct {
	ID              string `json:"id,omitempty"`
	TokenIndex      int    `json:"token_index"`
	Character       string `json:"character"`
	Pinyin          string `json:"pinyin"`
	Word            string `json:"word"`
	WordIndex       int    `json:"word_index"`
	AudioURL        string `json:"audio_url,omitempty"`
	IsPolyphone     bool   `json:"is_polyphone"`
	ProofreadStatus string `json:"proofread_status,omitempty"`
}

type SentenceManifest struct {
	ID            string          `json:"id,omitempty"`
	SequenceIndex int             `json:"sequence_index"`
	Text          string          `json:"text"`
	ImageURL      string          `json:"image_url"`
	AudioURL      string          `json:"audio_url"`
	Tokens        []TokenManifest `json:"tokens"`
}

type StoryManifest struct {
	StoryID       string             `json:"story_id"`
	Title         string             `json:"title"`
	Theme         string             `json:"theme"`
	Description   string             `json:"description"`
	CoverURL      string             `json:"cover_url"`
	VersionID     string             `json:"version_id"`
	VersionNumber int                `json:"version_number"`
	Status        string             `json:"status"`
	Sentences     []SentenceManifest `json:"sentences"`
}

type WordEntry struct {
	Pinyins []string
	Polys   []bool
}

// ContextWordDict provides built-in context-sensitive polyphone disambiguation for online quick-story authoring.
var ContextWordDict = map[string]WordEntry{
	"小熊":  {Pinyins: []string{"xiǎo", "xióng"}, Polys: []bool{false, false}},
	"来到":  {Pinyins: []string{"lái", "dào"}, Polys: []bool{false, false}},
	"一条":  {Pinyins: []string{"yì", "tiáo"}, Polys: []bool{false, false}},
	"小河":  {Pinyins: []string{"xiǎo", "hé"}, Polys: []bool{false, false}},
	"河边":  {Pinyins: []string{"hé", "biān"}, Polys: []bool{false, false}},
	"看着":  {Pinyins: []string{"kàn", "zhe"}, Polys: []bool{true, true}},
	"哗哗":  {Pinyins: []string{"huā", "huā"}, Polys: []bool{false, false}},
	"流淌":  {Pinyins: []string{"liú", "tǎng"}, Polys: []bool{false, false}},
	"河水":  {Pinyins: []string{"hé", "shuǐ"}, Polys: []bool{false, false}},
	"心里":  {Pinyins: []string{"xīn", "lǐ"}, Polys: []bool{false, false}},
	"有点":  {Pinyins: []string{"yǒu", "diǎn"}, Polys: []bool{false, false}},
	"着急":  {Pinyins: []string{"zháo", "jí"}, Polys: []bool{true, false}},
	"鹿阿姨": {Pinyins: []string{"lù", "ā", "yí"}, Polys: []bool{false, false, false}},
	"走过来": {Pinyins: []string{"zǒu", "guò", "lái"}, Polys: []bool{false, false, false}},
	"温和":  {Pinyins: []string{"wēn", "hé"}, Polys: []bool{false, true}},
	"告诉":  {Pinyins: []string{"gào", "su"}, Polys: []bool{false, false}},
	"木桥":  {Pinyins: []string{"mù", "qiáo"}, Polys: []bool{false, false}},
	"可以":  {Pinyins: []string{"kě", "yǐ"}, Polys: []bool{false, false}},
	"安全":  {Pinyins: []string{"ān", "quán"}, Polys: []bool{false, false}},
	"通行":  {Pinyins: []string{"tōng", "xíng"}, Polys: []bool{false, true}},
	"行走":  {Pinyins: []string{"xíng", "zǒu"}, Polys: []bool{true, false}},
	"银行":  {Pinyins: []string{"yín", "háng"}, Polys: []bool{false, true}},
	"一行":  {Pinyins: []string{"yì", "háng"}, Polys: []bool{false, true}},
	"脚印":  {Pinyins: []string{"jiǎo", "yìn"}, Polys: []bool{false, false}},
	"勇敢":  {Pinyins: []string{"yǒng", "gǎn"}, Polys: []bool{false, false}},
	"迈开":  {Pinyins: []string{"mài", "kāi"}, Polys: []bool{false, false}},
	"脚步":  {Pinyins: []string{"jiǎo", "bù"}, Polys: []bool{false, false}},
	"稳稳":  {Pinyins: []string{"wěn", "wěn"}, Polys: []bool{false, false}},
	"走过":  {Pinyins: []string{"zǒu", "guò"}, Polys: []bool{false, false}},
	"到达":  {Pinyins: []string{"dào", "dá"}, Polys: []bool{false, false}},
	"对岸":  {Pinyins: []string{"duì", "àn"}, Polys: []bool{false, false}},
	"森林":  {Pinyins: []string{"sēn", "lín"}, Polys: []bool{false, false}},
	"开心":  {Pinyins: []string{"kāi", "xīn"}, Polys: []bool{false, false}},
	"极了":  {Pinyins: []string{"jí", "le"}, Polys: []bool{false, true}},
}

var SingleCharDict = map[string]struct {
	Pinyin string
	Poly   bool
}{
	"小": {"xiǎo", false}, "熊": {"xióng", false}, "来": {"lái", false}, "到": {"dào", false},
	"一": {"yī", false}, "条": {"tiáo", false}, "河": {"hé", false}, "边": {"biān", false},
	"他": {"tā", false}, "看": {"kàn", true}, "着": {"zhe", true}, "的": {"de", true},
	"水": {"shuǐ", false}, "地": {"de", true}, "说": {"shuō", true}, "前": {"qián", false},
	"面": {"miàn", false}, "有": {"yǒu", false}, "座": {"zuò", false}, "桥": {"qiáo", false},
	"上": {"shàng", false}, "留": {"liú", false}, "下": {"xià", false}, "行": {"xíng", true},
	"走": {"zǒu", false}, "过": {"guò", false}, "了": {"le", true}, "山": {"shān", false},
	"木": {"mù", false}, "火": {"huǒ", false}, "日": {"rì", false}, "月": {"yuè", false},
	"大": {"dà", false}, "风": {"fēng", false}, "雨": {"yǔ", false}, "花": {"huā", false},
	"草": {"cǎo", false}, "鸟": {"niǎo", false}, "鱼": {"yú", false},
}

func MatchWordsForCharPinyin(ch, py string) []string {
	var out []string
	for w, entry := range ContextWordDict {
		runes := []rune(w)
		for idx, r := range runes {
			if idx < len(entry.Pinyins) && string(r) == ch && entry.Pinyins[idx] == py {
				out = append(out, w)
				break
			}
		}
	}
	if len(out) == 0 {
		return []string{ch}
	}
	return out
}

func SplitSentences(rawText string) []string {
	var result []string
	for _, line := range strings.Split(rawText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var buf strings.Builder
		for _, r := range line {
			buf.WriteRune(r)
			if r == '。' || r == '！' || r == '？' {
				s := strings.TrimSpace(buf.String())
				if s != "" {
					result = append(result, s)
				}
				buf.Reset()
			}
		}
		if rem := strings.TrimSpace(buf.String()); rem != "" {
			result = append(result, rem)
		}
	}
	return result
}

func SegmentAndAnnotate(sentenceID, text string) []TokenManifest {
	runes := []rune(text)
	n := len(runes)
	var tokens []TokenManifest
	i := 0
	tokenIdx := 0
	wordIdx := 0

	for i < n {
		matchedWord := ""
		var matchedEntry WordEntry
		for length := 4; length >= 2; length-- {
			if i+length <= n {
				cand := string(runes[i : i+length])
				if entry, ok := ContextWordDict[cand]; ok {
					matchedWord = cand
					matchedEntry = entry
					break
				}
			}
		}
		if matchedWord != "" {
			wRunes := []rune(matchedWord)
			for offset, r := range wRunes {
				ch := string(r)
				py := ch
				poly := false
				if offset < len(matchedEntry.Pinyins) {
					py = matchedEntry.Pinyins[offset]
				}
				if offset < len(matchedEntry.Polys) {
					poly = matchedEntry.Polys[offset]
				}
				tokens = append(tokens, TokenManifest{
					ID:              fmt.Sprintf("%s_t%02d", sentenceID, tokenIdx),
					TokenIndex:      tokenIdx,
					Character:       ch,
					Pinyin:          py,
					Word:            matchedWord,
					WordIndex:       wordIdx,
					IsPolyphone:     poly,
					ProofreadStatus: "verified",
				})
				tokenIdx++
			}
			i += utf8.RuneCountInString(matchedWord)
			wordIdx++
		} else {
			ch := string(runes[i])
			py := ch
			poly := false
			if sc, ok := SingleCharDict[ch]; ok {
				py = sc.Pinyin
				poly = sc.Poly
			}
			tokens = append(tokens, TokenManifest{
				ID:              fmt.Sprintf("%s_t%02d", sentenceID, tokenIdx),
				TokenIndex:      tokenIdx,
				Character:       ch,
				Pinyin:          py,
				Word:            ch,
				WordIndex:       wordIdx,
				IsPolyphone:     poly,
				ProofreadStatus: "verified",
			})
			tokenIdx++
			wordIdx++
			i++
		}
	}
	return tokens
}

type Importer struct {
	db       *sql.DB
	mediaDir string
	tmpDir   string
}

func NewImporter(db *sql.DB, mediaDir, tmpDir string) *Importer {
	return &Importer{
		db:       db,
		mediaDir: mediaDir,
		tmpDir:   tmpDir,
	}
}

func ValidateManifest(m *StoryManifest) error {
	if strings.TrimSpace(m.StoryID) == "" {
		return errors.New("manifest missing story_id")
	}
	if strings.TrimSpace(m.Title) == "" {
		return errors.New("manifest missing title")
	}
	if strings.TrimSpace(m.VersionID) == "" {
		return errors.New("manifest missing version_id")
	}
	if m.VersionNumber <= 0 {
		return errors.New("manifest version_number must be >= 1")
	}
	if len(m.Sentences) == 0 {
		return errors.New("manifest must contain at least one sentence")
	}
	for i, s := range m.Sentences {
		if strings.TrimSpace(s.Text) == "" {
			return fmt.Errorf("sentence[%d] text is empty", i)
		}
		if len(s.Tokens) == 0 {
			return fmt.Errorf("sentence[%d] tokens is empty", i)
		}
		for j, tok := range s.Tokens {
			if strings.TrimSpace(tok.Character) == "" || strings.TrimSpace(tok.Pinyin) == "" || strings.TrimSpace(tok.Word) == "" {
				return fmt.Errorf("sentence[%d].tokens[%d] missing character, pinyin, or word", i, j)
			}
		}
	}
	return nil
}

// ImportZip extracts and validates a story package (.zip) using the disk tmp directory
// and persists the story, version, sentences, tokens, and media assets atomically.
func (imp *Importer) ImportZip(ctx context.Context, zipPath string) (*StoryManifest, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip %s: %w", zipPath, err)
	}
	defer func() { _ = zr.Close() }()

	var manifest *StoryManifest
	for _, f := range zr.File {
		if filepath.Clean(f.Name) == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("open manifest.json: %w", err)
			}
			data, err := io.ReadAll(io.LimitReader(rc, 10*1024*1024))
			_ = rc.Close()
			if err != nil {
				return nil, fmt.Errorf("read manifest.json: %w", err)
			}
			var m StoryManifest
			if err := json.Unmarshal(data, &m); err != nil {
				return nil, fmt.Errorf("parse manifest.json: %w", err)
			}
			manifest = &m
			break
		}
	}
	if manifest == nil {
		return nil, errors.New("manifest.json not found in zip archive")
	}
	if err := ValidateManifest(manifest); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}

	cleanMediaRoot := filepath.Clean(imp.mediaDir)
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || filepath.Clean(f.Name) == "manifest.json" {
			continue
		}
		rel := filepath.Clean(f.Name)
		if strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
			return nil, fmt.Errorf("unsafe zip entry path: %s", f.Name)
		}
		rel = strings.TrimPrefix(rel, "media/")
		if !strings.HasPrefix(rel, "stories/") && !strings.HasPrefix(rel, "audio/") {
			continue
		}
		destPath := filepath.Join(cleanMediaRoot, rel)
		if !strings.HasPrefix(filepath.Clean(destPath), cleanMediaRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("zip path traversal blocked: %s", f.Name)
		}
		if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir media %s: %w", filepath.Dir(destPath), err)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %s: %w", f.Name, err)
		}
		out, err := os.Create(destPath)
		if err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("create media file %s: %w", destPath, err)
		}
		_, copyErr := io.Copy(out, io.LimitReader(rc, 50*1024*1024))
		_ = out.Close()
		_ = rc.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("extract media file %s: %w", destPath, copyErr)
		}
	}

	if err := imp.SaveManifest(ctx, manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func (imp *Importer) SaveManifest(ctx context.Context, m *StoryManifest) error {
	if err := ValidateManifest(m); err != nil {
		return err
	}

	var fullText strings.Builder
	for i, s := range m.Sentences {
		if i > 0 {
			fullText.WriteString("\n")
		}
		fullText.WriteString(s.Text)
	}
	sum := sha256.Sum256([]byte(fullText.String()))
	checksum := hex.EncodeToString(sum[:])

	status := m.Status
	if status == "" {
		status = "published"
	}

	tx, err := imp.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO stories (id, title, theme, description, cover_url, status)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			title = excluded.title,
			theme = excluded.theme,
			description = excluded.description,
			cover_url = excluded.cover_url,
			status = excluded.status
	`, m.StoryID, m.Title, m.Theme, m.Description, m.CoverURL, status)
	if err != nil {
		return fmt.Errorf("upsert story %s: %w", m.StoryID, err)
	}

	var existingChecksum string
	err = tx.QueryRowContext(ctx, "SELECT checksum FROM story_versions WHERE id = ?", m.VersionID).Scan(&existingChecksum)
	if err == nil && existingChecksum != checksum {
		return fmt.Errorf("version %s already published with different text checksum; create a new version instead", m.VersionID)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO story_versions (id, story_id, version_number, text_content, checksum, status)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status
	`, m.VersionID, m.StoryID, m.VersionNumber, fullText.String(), checksum, status)
	if err != nil {
		return fmt.Errorf("upsert story_version %s: %w", m.VersionID, err)
	}

	for _, s := range m.Sentences {
		sentID := s.ID
		if sentID == "" {
			sentID = fmt.Sprintf("%s_s%02d", m.VersionID, s.SequenceIndex)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO story_sentences (id, version_id, sequence_index, text, image_url, audio_url)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				image_url = excluded.image_url,
				audio_url = excluded.audio_url
		`, sentID, m.VersionID, s.SequenceIndex, s.Text, s.ImageURL, s.AudioURL)
		if err != nil {
			return fmt.Errorf("upsert sentence %s: %w", sentID, err)
		}

		for _, tok := range s.Tokens {
			tokID := tok.ID
			if tokID == "" {
				tokID = fmt.Sprintf("%s_t%02d", sentID, tok.TokenIndex)
			}
			polyInt := 0
			if tok.IsPolyphone {
				polyInt = 1
			}
			proofStatus := tok.ProofreadStatus
			if proofStatus == "" {
				proofStatus = "verified"
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO sentence_tokens (
					id, sentence_id, token_index, character, pinyin, word,
					word_index, audio_url, is_polyphone, proofread_status
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET
					pinyin = excluded.pinyin,
					word = excluded.word,
					word_index = excluded.word_index,
					audio_url = excluded.audio_url,
					is_polyphone = excluded.is_polyphone,
					proofread_status = excluded.proofread_status
			`, tokID, sentID, tok.TokenIndex, tok.Character, tok.Pinyin, tok.Word, tok.WordIndex, tok.AudioURL, polyInt, proofStatus)
			if err != nil {
				return fmt.Errorf("upsert token %s: %w", tokID, err)
			}

			_, _ = tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO characters (character) VALUES (?)
			`, tok.Character)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit story import: %w", err)
	}
	return nil
}
