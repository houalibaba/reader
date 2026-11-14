-- 0001_init.sql
-- Chinese Children Adaptive Reading System (iStoreOS LAN Edition)
-- Initial SQLite Schema & Views

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA synchronous = NORMAL;

-- 1. 儿童档案表 (children)
CREATE TABLE IF NOT EXISTS children (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    avatar TEXT,
    pin TEXT,
    learning_stage TEXT,
    prior_source TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 默认种子儿童档案（防止首次安装未手动建档时点击“开始阅读”无反应）
INSERT OR IGNORE INTO children (id, name, avatar, learning_stage)
VALUES ('child_default_liuliu', '六六', '🐻', '学龄前/一年级');

-- 2. 基础汉字表 (characters - 预留字频与先验资料，不固定单一读音)
CREATE TABLE IF NOT EXISTS characters (
    character TEXT PRIMARY KEY,
    frequency_rank INTEGER,
    stroke_count INTEGER,
    notes TEXT
);

-- 3. 故事主表 (stories)
CREATE TABLE IF NOT EXISTS stories (
    id TEXT PRIMARY KEY,
    title TEXT NOT NULL,
    theme TEXT,
    description TEXT,
    cover_url TEXT,
    status TEXT DEFAULT 'draft',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 4. 故事版本表 (story_versions)
CREATE TABLE IF NOT EXISTS story_versions (
    id TEXT PRIMARY KEY,
    story_id TEXT NOT NULL REFERENCES stories(id) ON DELETE CASCADE,
    version_number INTEGER NOT NULL,
    text_content TEXT NOT NULL,
    checksum TEXT NOT NULL,
    status TEXT DEFAULT 'published',
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(story_id, version_number)
);

-- 5. 句子表 (story_sentences)
CREATE TABLE IF NOT EXISTS story_sentences (
    id TEXT PRIMARY KEY,
    version_id TEXT NOT NULL REFERENCES story_versions(id) ON DELETE CASCADE,
    sequence_index INTEGER NOT NULL,
    text TEXT NOT NULL,
    image_url TEXT,
    audio_url TEXT,
    UNIQUE(version_id, sequence_index)
);

-- 6. 逐字及词标注表 (sentence_tokens)
CREATE TABLE IF NOT EXISTS sentence_tokens (
    id TEXT PRIMARY KEY,
    sentence_id TEXT NOT NULL REFERENCES story_sentences(id) ON DELETE CASCADE,
    token_index INTEGER NOT NULL,
    character TEXT NOT NULL,
    pinyin TEXT NOT NULL,
    word TEXT NOT NULL,
    word_index INTEGER DEFAULT 0,
    audio_url TEXT,
    is_polyphone INTEGER DEFAULT 0,
    proofread_status TEXT DEFAULT 'verified',
    UNIQUE(sentence_id, token_index)
);

-- 7. 阅读会话表 (reading_sessions)
CREATE TABLE IF NOT EXISTS reading_sessions (
    id TEXT PRIMARY KEY,
    child_id TEXT NOT NULL REFERENCES children(id) ON DELETE CASCADE,
    story_version_id TEXT NOT NULL REFERENCES story_versions(id) ON DELETE CASCADE,
    mode TEXT DEFAULT 'solo', -- solo / accompanied
    status TEXT DEFAULT 'active', -- active / completed / abandoned
    end_reason TEXT, -- finished / user_exit / timeout
    current_sentence_index INTEGER DEFAULT 0,
    pinyin_hint_enabled INTEGER DEFAULT 0,
    started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    ended_at DATETIME
);

-- 8. 句子展示曝光表 (sentence_views)
CREATE TABLE IF NOT EXISTS sentence_views (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES reading_sessions(id) ON DELETE CASCADE,
    sentence_id TEXT NOT NULL REFERENCES story_sentences(id) ON DELETE CASCADE,
    entered_at DATETIME NOT NULL,
    exited_at DATETIME,
    dwell_time_ms INTEGER DEFAULT 0,
    is_revisit INTEGER DEFAULT 0,
    is_valid INTEGER DEFAULT 1,
    invalid_reason TEXT,
    client_version TEXT,
    received_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 9. 点字求助事件表 (character_events)
CREATE TABLE IF NOT EXISTS character_events (
    id TEXT PRIMARY KEY,
    sentence_view_id TEXT NOT NULL REFERENCES sentence_views(id) ON DELETE CASCADE,
    token_id TEXT NOT NULL REFERENCES sentence_tokens(id) ON DELETE CASCADE,
    character TEXT NOT NULL,
    pinyin_played TEXT NOT NULL,
    clicked_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    is_valid INTEGER DEFAULT 1,
    invalid_reason TEXT,
    received_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 10. 整句音频播放事件表 (audio_events)
CREATE TABLE IF NOT EXISTS audio_events (
    id TEXT PRIMARY KEY,
    sentence_view_id TEXT NOT NULL REFERENCES sentence_views(id) ON DELETE CASCADE,
    audio_type TEXT DEFAULT 'sentence', -- sentence / character / word
    played_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    play_duration_ms INTEGER DEFAULT 0,
    completed INTEGER DEFAULT 1,
    received_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 11. 前后台与可见性事件表 (visibility_events)
CREATE TABLE IF NOT EXISTS visibility_events (
    id TEXT PRIMARY KEY,
    sentence_view_id TEXT NOT NULL REFERENCES sentence_views(id) ON DELETE CASCADE,
    state TEXT NOT NULL, -- hidden / visible
    occurred_at DATETIME NOT NULL,
    background_duration_ms INTEGER DEFAULT 0,
    received_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- 索引优化
CREATE INDEX IF NOT EXISTS idx_story_versions_story ON story_versions(story_id);
CREATE INDEX IF NOT EXISTS idx_story_sentences_version ON story_sentences(version_id, sequence_index);
CREATE INDEX IF NOT EXISTS idx_sentence_tokens_sentence ON sentence_tokens(sentence_id, token_index);
CREATE INDEX IF NOT EXISTS idx_reading_sessions_child ON reading_sessions(child_id, status);
CREATE INDEX IF NOT EXISTS idx_sentence_views_session ON sentence_views(session_id);
CREATE INDEX IF NOT EXISTS idx_character_events_view ON character_events(sentence_view_id);

-- 12. 派生词汇推测与遗忘曲线视图 (vocabulary_view)
DROP VIEW IF EXISTS vocabulary_view;
CREATE VIEW vocabulary_view AS
SELECT 
    rs.child_id,
    st.character,
    st.pinyin,
    COUNT(DISTINCT sv.id) AS exposure_count,
    COUNT(DISTINCT CASE WHEN ce.id IS NOT NULL AND ce.is_valid = 1 THEN sv.id END) AS helped_view_count,
    COUNT(CASE WHEN ce.is_valid = 1 THEN ce.id END) AS help_count,
    MAX(CASE WHEN ce.is_valid = 1 THEN ce.clicked_at END) AS last_help_at,
    MAX(sv.entered_at) AS last_seen_at,
    CASE 
        WHEN COUNT(CASE WHEN ce.is_valid = 1 THEN ce.id END) > 2 THEN 'needs_support'
        WHEN COUNT(DISTINCT sv.id) < 3 THEN 'insufficient_evidence'
        ELSE 'likely_familiar'
    END AS estimated_status
FROM sentence_views sv
JOIN reading_sessions rs ON sv.session_id = rs.id
JOIN story_sentences ss ON sv.sentence_id = ss.id
JOIN sentence_tokens st ON st.sentence_id = ss.id
LEFT JOIN character_events ce ON ce.sentence_view_id = sv.id AND ce.token_id = st.id
WHERE sv.is_valid = 1
  AND rs.mode = 'solo'
  AND rs.pinyin_hint_enabled = 0
GROUP BY rs.child_id, st.character, st.pinyin;
