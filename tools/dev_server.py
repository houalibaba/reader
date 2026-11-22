#!/usr/bin/env python3
"""
tools/dev_server.py
Zero-dependency Python 3 + SQLite 3 reference server implementing the exact same SQL schema
(0001_init.sql), mount checks, immutable story versioning, Range media serving, and REST API
contracts as the Go server. Used for local development and automated E2E verification when
the Go toolchain is not installed on the host.
"""

import hashlib
import io
import json
import os
import shutil
import sqlite3
import tempfile
import uuid
import zipfile
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

from fastapi import FastAPI, File, Header, HTTPException, Query, Request, Response, UploadFile
from fastapi.responses import JSONResponse

from story_packager import generate_svg_illustration, segment_and_annotate
from vocab_planner import auto_generate_story_from_plan, compute_forgetting_curve_metrics, generate_vocabulary_plan, verify_draft_against_plan

MIN_VALID_DWELL_MS = 300
MAX_VALID_DWELL_MS = 180000


def compute_valid_dwell(total_elapsed_ms: int, bg_durations_ms: List[int]) -> Tuple[int, int, str]:
    total_bg = sum(d for d in bg_durations_ms if d > 0)
    dwell = max(0, total_elapsed_ms - total_bg)
    if dwell < MIN_VALID_DWELL_MS:
        return dwell, 0, "fast_flip"
    if dwell > MAX_VALID_DWELL_MS:
        return dwell, 0, "abnormal_long_dwell"
    return dwell, 1, ""


def split_sentences(raw_text: str) -> List[str]:
    result: List[str] = []
    for line in raw_text.splitlines():
        line = line.strip()
        if not line:
            continue
        buf = []
        for ch in line:
            buf.append(ch)
            if ch in ("。", "！", "？"):
                s = "".join(buf).strip()
                if s:
                    result.append(s)
                buf = []
        rem = "".join(buf).strip()
        if rem:
            result.append(rem)
    return result


class ReaderAppContext:
    def __init__(self, config_path: str):
        with open(config_path, "r", encoding="utf-8") as f:
            self.cfg = json.load(f)
        self.data_dir = Path(self.cfg["data_dir"]).resolve()
        self.guardian_pin = self.cfg.get("guardian_pin", "1234")
        self.min_free_disk_mb = self.cfg.get("min_free_disk_mb", 1024)

        self.verify_disk_mount()
        self.db_path = self.data_dir / "data" / "reader.db"
        self.media_dir = self.data_dir / "media"
        self.tmp_dir = self.data_dir / "tmp"
        self.backup_dir = self.data_dir / "backups"
        self.init_db()

    def verify_disk_mount(self) -> None:
        marker = self.data_dir / ".reader-disk"
        if not marker.is_file():
            raise RuntimeError(
                f"external disk mount marker missing ({marker}): refusing to start on router flash"
            )
        for sub in ["data", "media/stories", "media/audio", "tmp", "backups"]:
            (self.data_dir / sub).mkdir(parents=True, exist_ok=True)

    def get_conn(self) -> sqlite3.Connection:
        conn = sqlite3.connect(str(self.db_path))
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode = WAL;")
        conn.execute("PRAGMA foreign_keys = ON;")
        conn.execute("PRAGMA synchronous = NORMAL;")
        return conn

    def init_db(self) -> None:
        sql_path = (
            Path(__file__).resolve().parent.parent
            / "server"
            / "internal"
            / "migrations"
            / "0001_init.sql"
        )
        sql_script = sql_path.read_text(encoding="utf-8")
        with self.get_conn() as conn:
            conn.executescript(sql_script)

    def check_free_disk_mb(self) -> Tuple[int, bool]:
        st = os.statvfs(str(self.data_dir))
        free_mb = (st.f_bavail * st.f_frsize) // (1024 * 1024)
        return int(free_mb), free_mb >= self.min_free_disk_mb


def parse_bool_flag(value: Any) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, (int, float)):
        return value == 1
    if isinstance(value, str):
        return value.strip().lower() in ("true", "1", "yes", "on")
    return False


def create_app(config_path: str) -> FastAPI:
    ctx = ReaderAppContext(config_path)
    app = FastAPI(title="Chinese Children Adaptive Reader API")
    app.state.ctx = ctx

    def check_pin(x_guardian_pin: Optional[str], pin_query: Optional[str]) -> None:
        pin = x_guardian_pin or pin_query or ""
        if ctx.guardian_pin and pin != ctx.guardian_pin:
            raise HTTPException(status_code=401, detail="invalid guardian PIN")

    @app.get("/api/v1/health")
    def health() -> Dict[str, Any]:
        free_mb, ok = ctx.check_free_disk_mb()
        return {
            "status": "ok",
            "free_disk_mb": free_mb,
            "disk_space_ok": ok,
            "min_free_disk_mb": ctx.min_free_disk_mb,
        }

    @app.get("/api/v1/children")
    def list_children() -> Dict[str, Any]:
        with ctx.get_conn() as conn:
            rows = conn.execute(
                "SELECT id, name, COALESCE(avatar, ''), COALESCE(learning_stage, ''), created_at FROM children ORDER BY created_at ASC"
            ).fetchall()
        return {
            "children": [
                {
                    "id": r[0],
                    "name": r[1],
                    "avatar": r[2],
                    "learning_stage": r[3],
                    "created_at": r[4],
                }
                for r in rows
            ]
        }

    @app.post("/api/v1/children", status_code=201)
    async def create_child(req: Request) -> Dict[str, Any]:
        body = await req.json()
        name = (body.get("name") or "").strip()
        if not name:
            raise HTTPException(status_code=400, detail="name is required")
        cid = body.get("id") or f"child_{uuid.uuid4().hex[:12]}"
        avatar = body.get("avatar") or "🐻"
        stage = body.get("learning_stage") or ""
        prior = body.get("prior_source") or ""
        pin = body.get("pin") or ""
        now = datetime.now(timezone.utc).isoformat()
        with ctx.get_conn() as conn:
            conn.execute(
                "INSERT INTO children (id, name, avatar, pin, learning_stage, prior_source, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
                (cid, name, avatar, pin, stage, prior, now, now),
            )
        return {"id": cid, "name": name, "avatar": avatar, "learning_stage": stage, "created_at": now}

    @app.delete("/api/v1/children/{child_id}")
    def delete_child(
        child_id: str,
        x_guardian_pin: Optional[str] = Header(default=None),
        pin: Optional[str] = Query(default=None),
    ) -> Dict[str, Any]:
        check_pin(x_guardian_pin, pin)
        with ctx.get_conn() as conn:
            cur = conn.execute("DELETE FROM children WHERE id = ?", (child_id,))
            if cur.rowcount == 0:
                raise HTTPException(status_code=404, detail="child not found")
        return {"deleted": child_id}

    @app.get("/api/v1/children/{child_id}/vocabulary")
    def get_vocabulary(child_id: str) -> Dict[str, Any]:
        with ctx.get_conn() as conn:
            rows = conn.execute(
                """
                WITH token_views AS (
                    SELECT
                        st.character,
                        st.pinyin,
                        sv.id AS view_id,
                        sv.entered_at,
                        COUNT(CASE WHEN ce.is_valid = 1 THEN ce.id END) AS view_help_count,
                        MAX(CASE WHEN ce.is_valid = 1 THEN ce.clicked_at END) AS view_last_help_at
                    FROM sentence_views sv
                    JOIN reading_sessions rs ON sv.session_id = rs.id
                    JOIN story_sentences ss ON sv.sentence_id = ss.id
                    JOIN sentence_tokens st ON st.sentence_id = ss.id
                    LEFT JOIN character_events ce ON ce.sentence_view_id = sv.id AND ce.token_id = st.id
                    WHERE rs.child_id = ?
                      AND sv.is_valid = 1
                      AND rs.mode = 'solo'
                      AND rs.pinyin_hint_enabled = 0
                    GROUP BY st.character, st.pinyin, sv.id, sv.entered_at
                ),
                char_agg AS (
                    SELECT
                        character,
                        pinyin,
                        COUNT(*) AS exposure_count,
                        SUM(CASE WHEN view_help_count > 0 THEN 1 ELSE 0 END) AS helped_view_count,
                        SUM(view_help_count) AS help_count,
                        MAX(view_last_help_at) AS last_help_at,
                        MAX(entered_at) AS last_seen_at
                    FROM token_views
                    GROUP BY character, pinyin
                )
                SELECT
                    ca.character,
                    ca.pinyin,
                    ca.exposure_count,
                    ca.helped_view_count,
                    ca.help_count,
                    COALESCE(ca.last_help_at, ''),
                    COALESCE(ca.last_seen_at, ''),
                    (
                        SELECT COUNT(*)
                        FROM token_views tv
                        WHERE tv.character = ca.character
                          AND tv.pinyin = ca.pinyin
                          AND tv.view_help_count = 0
                          AND (ca.last_help_at IS NULL OR julianday(tv.entered_at) > julianday(ca.last_help_at))
                    ) AS post_help_unhelped
                FROM char_agg ca
                ORDER BY ca.help_count DESC, ca.exposure_count DESC
                """,
                (child_id,),
            ).fetchall()
        vocab_items = []
        for r in rows:
            rec = {
                "character": r[0],
                "pinyin": r[1],
                "exposure_count": r[2],
                "helped_view_count": r[3],
                "help_count": r[4],
                "last_help_at": r[5],
                "last_seen_at": r[6],
                "post_help_unhelped": r[7],
            }
            fc = compute_forgetting_curve_metrics(rec)
            if fc["active_needs_support"]:
                rec["estimated_status"] = "needs_support"
            elif rec["exposure_count"] < 3:
                rec["estimated_status"] = "insufficient_evidence"
            else:
                rec["estimated_status"] = "likely_familiar"
            vocab_items.append(rec)
        return {
            "child_id": child_id,
            "vocabulary": vocab_items,
        }

    @app.get("/api/v1/children/{child_id}/story-plan")
    def get_child_story_plan(child_id: str) -> Dict[str, Any]:
        vocab_res = get_vocabulary(child_id)
        plan = generate_vocabulary_plan(vocab_res["vocabulary"])
        plan["child_id"] = child_id
        return plan

    @app.get("/api/v1/stories")
    def list_stories(child_id: Optional[str] = Query(default=None)) -> Dict[str, Any]:
        with ctx.get_conn() as conn:
            rows = conn.execute(
                """
                SELECT s.id, s.title, COALESCE(s.theme, ''), COALESCE(s.description, ''), COALESCE(s.cover_url, ''),
                       sv.id AS version_id, sv.version_number,
                       (SELECT COUNT(*) FROM story_sentences ss WHERE ss.version_id = sv.id) AS sentence_count
                FROM stories s
                JOIN story_versions sv ON sv.story_id = s.id
                WHERE s.status = 'published' AND sv.status = 'published'
                  AND sv.version_number = (
                      SELECT MAX(sv2.version_number) FROM story_versions sv2
                      WHERE sv2.story_id = s.id AND sv2.status = 'published'
                  )
                ORDER BY s.created_at ASC
                """
            ).fetchall()

            stories = []
            for r in rows:
                st = {
                    "id": r[0],
                    "title": r[1],
                    "theme": r[2],
                    "description": r[3],
                    "cover_url": r[4],
                    "latest_version_id": r[5],
                    "version_number": r[6],
                    "sentence_count": r[7],
                    "resume_sentence_index": 0,
                    "review_char_count": 0,
                }
                if child_id:
                    active = conn.execute(
                        """
                        SELECT id, current_sentence_index FROM reading_sessions
                        WHERE child_id = ? AND story_version_id = ? AND status = 'active'
                        ORDER BY started_at DESC LIMIT 1
                        """,
                        (child_id, st["latest_version_id"]),
                    ).fetchone()
                    if active:
                        st["active_session_id"] = active[0]
                        st["resume_sentence_index"] = active[1]

                    rev = conn.execute(
                        """
                        SELECT COUNT(DISTINCT st.character)
                        FROM story_sentences ss
                        JOIN sentence_tokens st ON st.sentence_id = ss.id
                        JOIN vocabulary_view vv ON vv.character = st.character AND vv.pinyin = st.pinyin
                        WHERE ss.version_id = ? AND vv.child_id = ? AND vv.estimated_status = 'needs_support'
                        """,
                        (st["latest_version_id"], child_id),
                    ).fetchone()
                    st["review_char_count"] = rev[0] if rev else 0
                stories.append(st)
        return {"stories": stories}


    @app.delete("/api/v1/admin/stories/{story_id}")
    @app.delete("/api/v1/stories/{story_id}")
    def delete_story(
        story_id: str,
        hard: Optional[str] = Query(default=None),
        x_guardian_pin: Optional[str] = Header(default=None),
        pin: Optional[str] = Query(default=None),
    ) -> Dict[str, Any]:
        check_pin(x_guardian_pin, pin)
        with ctx.get_conn() as conn:
            row = conn.execute(
                "SELECT title FROM stories WHERE id = ? AND status != 'deleted'",
                (story_id,),
            ).fetchone()
            if not row:
                raise HTTPException(status_code=404, detail="story not found")
            title = row[0]

            sess_row = conn.execute(
                """
                SELECT COUNT(*) FROM reading_sessions rs
                JOIN story_versions sv ON rs.story_version_id = sv.id
                WHERE sv.story_id = ?
                """,
                (story_id,),
            ).fetchone()
            sess_count = sess_row[0] if sess_row else 0
            hard_delete = (hard == "1") or (sess_count == 0)

            if hard_delete:
                conn.execute("DELETE FROM stories WHERE id = ?", (story_id,))
            else:
                now = datetime.now(timezone.utc).isoformat()
                conn.execute("UPDATE stories SET status = 'deleted' WHERE id = ?", (story_id,))
                conn.execute("UPDATE story_versions SET status = 'deleted' WHERE story_id = ?", (story_id,))
                conn.execute(
                    """
                    UPDATE reading_sessions
                    SET status = 'abandoned', end_reason = 'story_deleted', ended_at = COALESCE(ended_at, ?)
                    WHERE status = 'active' AND story_version_id IN (SELECT id FROM story_versions WHERE story_id = ?)
                    """,
                    (now, story_id),
                )

        if ".." not in story_id:
            story_media_dir = ctx.media_dir / "stories" / story_id
            if story_media_dir.exists():
                shutil.rmtree(story_media_dir, ignore_errors=True)

        return {
            "deleted": story_id,
            "title": title,
            "preserved_vocab_history": not hard_delete,
        }

    @app.get("/api/v1/stories/{story_id}/versions/{version_id}")
    def get_story_version(story_id: str, version_id: str) -> Dict[str, Any]:
        with ctx.get_conn() as conn:
            meta = conn.execute(
                """
                SELECT s.title, COALESCE(s.theme, ''), COALESCE(s.description, ''), COALESCE(s.cover_url, ''), sv.version_number
                FROM stories s
                JOIN story_versions sv ON sv.story_id = s.id
                WHERE s.id = ? AND sv.id = ?
                """,
                (story_id, version_id),
            ).fetchone()
            if not meta:
                raise HTTPException(status_code=404, detail="story version not found")

            sent_rows = conn.execute(
                """
                SELECT id, sequence_index, text, COALESCE(image_url, ''), COALESCE(audio_url, '')
                FROM story_sentences
                WHERE version_id = ?
                ORDER BY sequence_index ASC
                """,
                (version_id,),
            ).fetchall()

            def is_real_audio(a_url: str) -> bool:
                if not a_url or not a_url.startswith("/media/"):
                    return False
                rel = a_url[len("/media/"):]
                if ".." in rel:
                    return False
                p = ctx.media_dir / rel
                return p.is_file() and p.stat().st_size > 512

            punct_set = set("，。！？、：；“”‘’（），.!?")
            sentences = []
            for sr in sent_rows:
                tok_rows = conn.execute(
                    """
                    SELECT id, token_index, character, pinyin, word, word_index, COALESCE(audio_url, ''), is_polyphone
                    FROM sentence_tokens
                    WHERE sentence_id = ?
                    ORDER BY token_index ASC
                    """,
                    (sr[0],),
                ).fetchall()
                tok_list = [
                    {
                        "id": tr[0],
                        "token_index": tr[1],
                        "character": tr[2],
                        "pinyin": tr[3],
                        "word": tr[4],
                        "word_index": tr[5],
                        "audio_url": tr[6] if is_real_audio(tr[6]) else "",
                        "is_polyphone": bool(tr[7]),
                    }
                    for tr in tok_rows
                ]
                if any((t["character"] not in punct_set and (not t["pinyin"] or t["pinyin"] == t["character"])) for t in tok_list):
                    re_toks = segment_and_annotate(sr[0], sr[2])
                    for idx, t in enumerate(tok_list):
                        if idx < len(re_toks) and t["character"] == re_toks[idx]["character"]:
                            t["pinyin"] = re_toks[idx]["pinyin"]
                            t["word"] = re_toks[idx]["word"]
                            t["word_index"] = re_toks[idx]["word_index"]
                            t["is_polyphone"] = re_toks[idx]["is_polyphone"]
                            conn.execute(
                                "UPDATE sentence_tokens SET pinyin = ?, word = ?, word_index = ?, is_polyphone = ? WHERE id = ?",
                                (t["pinyin"], t["word"], t["word_index"], 1 if t["is_polyphone"] else 0, t["id"]),
                            )
                sentences.append(
                    {
                        "id": sr[0],
                        "sequence_index": sr[1],
                        "text": sr[2],
                        "image_url": sr[3],
                        "audio_url": sr[4] if is_real_audio(sr[4]) else "",
                        "tokens": tok_list,
                    }
                )

        return {
            "story_id": story_id,
            "version_id": version_id,
            "version_number": meta[4],
            "title": meta[0],
            "theme": meta[1],
            "description": meta[2],
            "cover_url": meta[3],
            "sentences": sentences,
        }

    @app.post("/api/v1/sessions")
    async def create_or_resume_session(req: Request) -> JSONResponse:
        body = await req.json()
        child_id = body.get("child_id")
        sv_id = body.get("story_version_id")
        mode = body.get("mode") or "solo"
        pinyin_hint = 1 if body.get("pinyin_hint_enabled") else 0
        force_new = bool(body.get("force_new"))

        if not child_id or not sv_id:
            raise HTTPException(status_code=400, detail="child_id and story_version_id required")

        with ctx.get_conn() as conn:
            if not force_new:
                existing = conn.execute(
                    """
                    SELECT id, mode, current_sentence_index, started_at
                    FROM reading_sessions
                    WHERE child_id = ? AND story_version_id = ? AND status = 'active'
                    ORDER BY started_at DESC LIMIT 1
                    """,
                    (child_id, sv_id),
                ).fetchone()
                if existing:
                    return JSONResponse(
                        status_code=200,
                        content={
                            "id": existing[0],
                            "child_id": child_id,
                            "story_version_id": sv_id,
                            "mode": existing[1],
                            "status": "active",
                            "current_sentence_index": existing[2],
                            "resumed": True,
                            "started_at": existing[3],
                        },
                    )

            sess_id = f"sess_{uuid.uuid4().hex[:12]}"
            now = datetime.now(timezone.utc).isoformat()
            conn.execute(
                """
                INSERT INTO reading_sessions (
                    id, child_id, story_version_id, mode, status, current_sentence_index, pinyin_hint_enabled, started_at
                ) VALUES (?, ?, ?, ?, 'active', 0, ?, ?)
                """,
                (sess_id, child_id, sv_id, mode, pinyin_hint, now),
            )
            return JSONResponse(
                status_code=201,
                content={
                    "id": sess_id,
                    "child_id": child_id,
                    "story_version_id": sv_id,
                    "mode": mode,
                    "status": "active",
                    "current_sentence_index": 0,
                    "resumed": False,
                    "started_at": now,
                },
            )

    @app.post("/api/v1/sessions/{session_id}/events")
    async def ingest_events(session_id: str, req: Request) -> Dict[str, int]:
        payload = await req.json()
        with ctx.get_conn() as conn:
            sess = conn.execute("SELECT 1 FROM reading_sessions WHERE id = ?", (session_id,)).fetchone()
            if not sess:
                raise HTTPException(status_code=400, detail="session not found")

            p_views = 0
            for sv in payload.get("sentence_views") or []:
                sv_id = sv.get("id")
                sent_id = sv.get("sentence_id")
                if not sv_id or not sent_id:
                    continue
                conn.execute(
                    """
                    INSERT INTO sentence_views (
                        id, session_id, sentence_id, entered_at, exited_at,
                        dwell_time_ms, is_revisit, is_valid, invalid_reason, client_version
                    ) VALUES (?, ?, ?, ?, ?, 0, ?, 1, '', ?)
                    ON CONFLICT(id) DO UPDATE SET
                        exited_at = COALESCE(excluded.exited_at, sentence_views.exited_at)
                    """,
                    (
                        sv_id,
                        session_id,
                        sent_id,
                        sv.get("entered_at"),
                        sv.get("exited_at"),
                        1 if sv.get("is_revisit") else 0,
                        sv.get("client_version") or "",
                    ),
                )
                p_views += 1

            p_vis = 0
            for ve in payload.get("visibility_events") or []:
                if not ve.get("id") or not ve.get("sentence_view_id"):
                    continue
                conn.execute(
                    """
                    INSERT OR IGNORE INTO visibility_events (
                        id, sentence_view_id, state, occurred_at, background_duration_ms
                    ) VALUES (?, ?, ?, ?, ?)
                    """,
                    (
                        ve["id"],
                        ve["sentence_view_id"],
                        ve.get("state") or "visible",
                        ve.get("occurred_at"),
                        int(ve.get("background_duration_ms") or 0),
                    ),
                )
                p_vis += 1

            for sv in payload.get("sentence_views") or []:
                sv_id = sv.get("id")
                if not sv_id:
                    continue
                dedup_bg = [
                    r[0]
                    for r in conn.execute(
                        "SELECT background_duration_ms FROM visibility_events WHERE sentence_view_id = ?",
                        (sv_id,),
                    ).fetchall()
                ]
                dwell_ms, is_valid, reason = compute_valid_dwell(
                    int(sv.get("total_elapsed_ms") or 0), dedup_bg
                )
                conn.execute(
                    """
                    UPDATE sentence_views
                    SET dwell_time_ms = ?,
                        is_valid = ?,
                        invalid_reason = ?
                    WHERE id = ?
                    """,
                    (dwell_ms, is_valid, reason, sv_id),
                )

            p_chars = 0
            for ce in payload.get("character_events") or []:
                if not ce.get("id") or not ce.get("sentence_view_id") or not ce.get("token_id"):
                    continue
                conn.execute(
                    """
                    INSERT OR IGNORE INTO character_events (
                        id, sentence_view_id, token_id, character, pinyin_played, clicked_at, is_valid, invalid_reason
                    ) VALUES (?, ?, ?, ?, ?, ?, 1, '')
                    """,
                    (
                        ce["id"],
                        ce["sentence_view_id"],
                        ce["token_id"],
                        ce.get("character") or "",
                        ce.get("pinyin_played") or "",
                        ce.get("clicked_at") or datetime.now(timezone.utc).isoformat(),
                    ),
                )
                p_chars += 1

            p_audio = 0
            for ae in payload.get("audio_events") or []:
                if not ae.get("id") or not ae.get("sentence_view_id"):
                    continue
                conn.execute(
                    """
                    INSERT OR IGNORE INTO audio_events (
                        id, sentence_view_id, audio_type, played_at, play_duration_ms, completed
                    ) VALUES (?, ?, ?, ?, ?, ?)
                    """,
                    (
                        ae["id"],
                        ae["sentence_view_id"],
                        ae.get("audio_type") or "sentence",
                        ae.get("played_at") or datetime.now(timezone.utc).isoformat(),
                        int(ae.get("play_duration_ms") or 0),
                        1 if ae.get("completed", True) else 0,
                    ),
                )
                p_audio += 1

            if "current_sentence_index" in payload and payload["current_sentence_index"] is not None:
                conn.execute(
                    "UPDATE reading_sessions SET current_sentence_index = ? WHERE id = ?",
                    (int(payload["current_sentence_index"]), session_id),
                )
            if payload.get("session_status"):
                ended_at = (
                    datetime.now(timezone.utc).isoformat()
                    if payload["session_status"] in ("completed", "abandoned")
                    else None
                )
                conn.execute(
                    """
                    UPDATE reading_sessions
                    SET status = ?,
                        end_reason = COALESCE(NULLIF(?, ''), end_reason),
                        ended_at = COALESCE(?, ended_at)
                    WHERE id = ?
                    """,
                    (payload["session_status"], payload.get("end_reason") or "", ended_at, session_id),
                )

        return {
            "processed_views": p_views,
            "processed_char_events": p_chars,
            "processed_audio": p_audio,
            "processed_visibility": p_vis,
        }

    @app.post("/api/v1/admin/events/{event_id}/validity")
    async def mark_event_validity(
        event_id: str,
        req: Request,
        x_guardian_pin: Optional[str] = Header(default=None),
        pin: Optional[str] = Query(default=None),
    ) -> Dict[str, Any]:
        check_pin(x_guardian_pin, pin)
        body = await req.json()
        is_valid = 1 if body.get("is_valid") else 0
        reason = body.get("reason") or ("guardian_marked_accidental" if not is_valid else "")
        with ctx.get_conn() as conn:
            cur = conn.execute(
                "UPDATE character_events SET is_valid = ?, invalid_reason = ? WHERE id = ?",
                (is_valid, reason, event_id),
            )
            if cur.rowcount == 0:
                raise HTTPException(status_code=404, detail="character_event not found")
        return {"event_id": event_id, "is_valid": bool(is_valid)}

    def persist_manifest(manifest: Dict[str, Any]) -> Dict[str, Any]:
        full_text = "\n".join(s["text"] for s in manifest["sentences"])
        checksum = hashlib.sha256(full_text.encode("utf-8")).hexdigest()
        status = manifest.get("status") or "published"

        with ctx.get_conn() as conn:
            existing = conn.execute(
                "SELECT checksum FROM story_versions WHERE id = ?", (manifest["version_id"],)
            ).fetchone()
            if existing and existing[0] != checksum:
                raise HTTPException(
                    status_code=400,
                    detail="version already published with different checksum; create a new version instead",
                )

            conn.execute(
                """
                INSERT INTO stories (id, title, theme, description, cover_url, status)
                VALUES (?, ?, ?, ?, ?, ?)
                ON CONFLICT(id) DO UPDATE SET
                    title = excluded.title,
                    theme = excluded.theme,
                    description = excluded.description,
                    cover_url = excluded.cover_url,
                    status = excluded.status
                """,
                (
                    manifest["story_id"],
                    manifest["title"],
                    manifest.get("theme") or "",
                    manifest.get("description") or "",
                    manifest.get("cover_url") or "",
                    status,
                ),
            )
            conn.execute(
                """
                INSERT INTO story_versions (id, story_id, version_number, text_content, checksum, status)
                VALUES (?, ?, ?, ?, ?, ?)
                ON CONFLICT(id) DO UPDATE SET status = excluded.status
                """,
                (
                    manifest["version_id"],
                    manifest["story_id"],
                    int(manifest.get("version_number") or 1),
                    full_text,
                    checksum,
                    status,
                ),
            )
            for s in manifest["sentences"]:
                sent_id = s.get("id") or f"{manifest['version_id']}_s{s['sequence_index']:02d}"
                conn.execute(
                    """
                    INSERT INTO story_sentences (id, version_id, sequence_index, text, image_url, audio_url)
                    VALUES (?, ?, ?, ?, ?, ?)
                    ON CONFLICT(id) DO UPDATE SET
                        image_url = excluded.image_url,
                        audio_url = excluded.audio_url
                    """,
                    (
                        sent_id,
                        manifest["version_id"],
                        int(s["sequence_index"]),
                        s["text"],
                        s.get("image_url") or "",
                        s.get("audio_url") or "",
                    ),
                )
                for tok in s["tokens"]:
                    tok_id = tok.get("id") or f"{sent_id}_t{tok['token_index']:02d}"
                    conn.execute(
                        """
                        INSERT INTO sentence_tokens (
                            id, sentence_id, token_index, character, pinyin, word,
                            word_index, audio_url, is_polyphone, proofread_status
                        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                        ON CONFLICT(id) DO UPDATE SET
                            pinyin = excluded.pinyin,
                            word = excluded.word,
                            word_index = excluded.word_index,
                            audio_url = excluded.audio_url,
                            is_polyphone = excluded.is_polyphone
                        """,
                        (
                            tok_id,
                            sent_id,
                            int(tok["token_index"]),
                            tok["character"],
                            tok["pinyin"],
                            tok["word"],
                            int(tok.get("word_index") or 0),
                            tok.get("audio_url") or "",
                            1 if tok.get("is_polyphone") else 0,
                            tok.get("proofread_status") or "verified",
                        ),
                    )
                    conn.execute(
                        "INSERT OR IGNORE INTO characters (character) VALUES (?)", (tok["character"],)
                    )

        return {
            "imported_story_id": manifest["story_id"],
            "imported_version_id": manifest["version_id"],
            "version_number": int(manifest.get("version_number") or 1),
            "sentence_count": len(manifest["sentences"]),
        }

    @app.post("/api/v1/admin/auto-generate-story")
    async def admin_auto_generate_story(
        req: Request,
        x_guardian_pin: Optional[str] = Header(default=None),
        pin: Optional[str] = Query(default=None),
    ) -> Dict[str, Any]:
        check_pin(x_guardian_pin, pin)
        body = await req.json()
        child_id = (body.get("child_id") or "").strip()
        theme = (body.get("theme") or "勇气与探索").strip()
        auto_publish = parse_bool_flag(body.get("auto_publish"))

        vocab_records = get_vocabulary(child_id)["vocabulary"] if child_id else []
        plan = generate_vocabulary_plan(vocab_records)
        draft = auto_generate_story_from_plan(
            plan,
            theme=theme,
            llm_api_url=body.get("llm_api_url") or "",
            llm_api_key=body.get("llm_api_key") or "",
            llm_model=body.get("llm_model") or "nvidia/nemotron-3-ultra-550b-a55b:free",
        )
        validation = draft.get("validation") or verify_draft_against_plan(draft.get("sentences") or [], plan)
        draft["validation"] = validation
        draft["published"] = False

        can_auto_publish = auto_publish
        if can_auto_publish and (draft.get("generator") == "smart_template" or not validation.get("passed")):
            can_auto_publish = False
            draft["auto_publish_blocked"] = True
            draft["auto_publish_reason"] = "草稿未通过完整度与目标字覆盖校验（或已降级为短模板），已保留为草稿，请审阅或重试后再发布"

        if can_auto_publish:
            story_id = f"story_{uuid.uuid4().hex[:10]}"
            version_id = f"{story_id}_v1"
            sentences_manifest = []
            story_media_dir = ctx.media_dir / "stories" / story_id / version_id
            story_media_dir.mkdir(parents=True, exist_ok=True)
            total_p = len(draft["sentences"])
            for idx, s_text in enumerate(draft["sentences"]):
                sent_id = f"{version_id}_s{idx:02d}"
                svg_bytes = generate_svg_illustration(theme, s_text, idx, total_p)
                (story_media_dir / f"page_{idx:02d}.svg").write_bytes(svg_bytes)
                img_url = f"/media/stories/{story_id}/{version_id}/page_{idx:02d}.svg"
                sentences_manifest.append(
                    {
                        "id": sent_id,
                        "sequence_index": idx,
                        "text": s_text,
                        "image_url": img_url,
                        "audio_url": "",
                        "tokens": segment_and_annotate(sent_id, s_text),
                    }
                )
            manifest = {
                "story_id": story_id,
                "title": draft["title"],
                "theme": draft["theme"],
                "description": draft["description"],
                "version_id": version_id,
                "version_number": 1,
                "status": "published",
                "sentences": sentences_manifest,
            }
            pub_res = persist_manifest(manifest)
            draft.update(pub_res)
            draft["published"] = True

        return draft

    @app.post("/api/v1/admin/quick-story")
    async def admin_quick_story(
        req: Request,
        x_guardian_pin: Optional[str] = Header(default=None),
        pin: Optional[str] = Query(default=None),
    ) -> Dict[str, Any]:
        check_pin(x_guardian_pin, pin)
        body = await req.json()
        title = (body.get("title") or "").strip()
        raw_text = (body.get("raw_text") or "").strip()
        if not title or not raw_text:
            raise HTTPException(status_code=400, detail="title and raw_text are required")

        story_id = (body.get("story_id") or "").strip() or f"story_{uuid.uuid4().hex[:10]}"
        theme = (body.get("theme") or "").strip() or "自适应定制故事"
        description = (body.get("description") or "").strip()

        with ctx.get_conn() as conn:
            row = conn.execute(
                "SELECT COALESCE(MAX(version_number), 0) FROM story_versions WHERE story_id = ?",
                (story_id,),
            ).fetchone()
            next_ver = (row[0] if row else 0) + 1

        version_id = f"{story_id}_v{next_ver}"
        raw_sentences = split_sentences(raw_text)
        if not raw_sentences:
            raise HTTPException(status_code=400, detail="no valid sentences found in raw_text")

        story_media_dir = ctx.media_dir / "stories" / story_id / version_id
        story_media_dir.mkdir(parents=True, exist_ok=True)
        custom_imgs = body.get("custom_images") or []
        saved_urls: List[str] = []
        import base64 as _b64
        for i, cimg in enumerate(custom_imgs):
            b64_str = (cimg.get("data_base64") or "").strip()
            if not b64_str:
                continue
            ext = os.path.splitext(cimg.get("filename") or "")[1].lower()
            if ext not in (".jpg", ".jpeg", ".png", ".webp", ".svg", ".gif"):
                ext = ".jpg"
            fname = f"custom_{i:02d}{ext}"
            (story_media_dir / fname).write_bytes(_b64.b64decode(b64_str))
            saved_urls.append(f"/media/stories/{story_id}/{version_id}/{fname}")

        sentences_manifest = []
        total_sents = len(raw_sentences)
        for idx, s_text in enumerate(raw_sentences):
            sent_id = f"{version_id}_s{idx:02d}"
            tokens = segment_and_annotate(sent_id, s_text)
            if saved_urls:
                img_idx = min(len(saved_urls) - 1, (idx * len(saved_urls)) // total_sents)
                img_url = saved_urls[img_idx]
            else:
                svg_bytes = generate_svg_illustration(theme, s_text, idx, total_sents)
                (story_media_dir / f"page_{idx:02d}.svg").write_bytes(svg_bytes)
                img_url = f"/media/stories/{story_id}/{version_id}/page_{idx:02d}.svg"
            sentences_manifest.append(
                {
                    "id": sent_id,
                    "sequence_index": idx,
                    "text": s_text,
                    "image_url": img_url,
                    "audio_url": "",
                    "tokens": tokens,
                }
            )

        manifest = {
            "story_id": story_id,
            "title": title,
            "theme": theme,
            "description": description,
            "version_id": version_id,
            "version_number": next_ver,
            "status": "published",
            "sentences": sentences_manifest,
        }
        return persist_manifest(manifest)

    @app.post("/api/v1/admin/import")
    async def admin_import(
        req: Request,
        package: Optional[UploadFile] = File(default=None),
        x_guardian_pin: Optional[str] = Header(default=None),
        pin: Optional[str] = Query(default=None),
    ) -> Dict[str, Any]:
        check_pin(x_guardian_pin, pin)
        free_mb, ok = ctx.check_free_disk_mb()
        if not ok:
            raise HTTPException(
                status_code=507,
                detail=f"free disk space ({free_mb} MB) below minimum ({ctx.min_free_disk_mb} MB)",
            )

        manifest: Optional[Dict[str, Any]] = None
        if package is not None:
            tmp_fd, tmp_path = tempfile.mkstemp(suffix=".zip", dir=str(ctx.tmp_dir))
            os.close(tmp_fd)
            try:
                with open(tmp_path, "wb") as out_f:
                    shutil.copyfileobj(package.file, out_f)
                with zipfile.ZipFile(tmp_path, "r") as zf:
                    if "manifest.json" not in zf.namelist():
                        raise HTTPException(status_code=400, detail="manifest.json missing in zip")
                    manifest = json.loads(zf.read("manifest.json").decode("utf-8"))
                    for member in zf.infolist():
                        if member.is_dir() or member.filename == "manifest.json":
                            continue
                        clean_rel = os.path.normpath(member.filename).lstrip("/")
                        if clean_rel.startswith(".."):
                            raise HTTPException(status_code=400, detail="unsafe zip entry path")
                        if clean_rel.startswith("media/"):
                            clean_rel = clean_rel[len("media/") :]
                        if not (clean_rel.startswith("stories/") or clean_rel.startswith("audio/")):
                            continue
                        dest = (ctx.media_dir / clean_rel).resolve()
                        if not str(dest).startswith(str(ctx.media_dir.resolve()) + os.sep):
                            raise HTTPException(status_code=400, detail="path traversal blocked")
                        dest.parent.mkdir(parents=True, exist_ok=True)
                        dest.write_bytes(zf.read(member.filename))
            finally:
                if os.path.exists(tmp_path):
                    os.remove(tmp_path)
        else:
            manifest = await req.json()

        if not manifest or not manifest.get("story_id") or not manifest.get("version_id"):
            raise HTTPException(status_code=400, detail="invalid manifest")

        return persist_manifest(manifest)

    @app.get("/api/v1/admin/export")
    def admin_export(
        x_guardian_pin: Optional[str] = Header(default=None),
        pin: Optional[str] = Query(default=None),
    ) -> Dict[str, Any]:
        check_pin(x_guardian_pin, pin)
        with ctx.get_conn() as conn:
            children = [dict(r) for r in conn.execute("SELECT * FROM children").fetchall()]
            sessions = [dict(r) for r in conn.execute("SELECT * FROM reading_sessions").fetchall()]
            char_events = [dict(r) for r in conn.execute("SELECT * FROM character_events").fetchall()]
        return {
            "exported_at": datetime.now(timezone.utc).isoformat(),
            "children": children,
            "reading_sessions": sessions,
            "character_events": char_events,
        }

    @app.get("/media/{rel_path:path}")
    def serve_media(rel_path: str, req: Request) -> Response:
        if ".." in rel_path:
            raise HTTPException(status_code=403, detail="path traversal blocked")
        target = (ctx.media_dir / rel_path).resolve()
        if not str(target).startswith(str(ctx.media_dir.resolve()) + os.sep) or not target.is_file():
            raise HTTPException(status_code=404, detail="media not found")

        data = target.read_bytes()
        st = target.stat()
        etag = f'W/"{int(st.st_mtime):x}-{st.st_size:x}"'
        if req.headers.get("if-none-match") == etag:
            return Response(status_code=304, headers={"ETag": etag})

        range_header = req.headers.get("range")
        if range_header and range_header.startswith("bytes="):
            parts = range_header[6:].split("-")
            start = int(parts[0]) if parts[0] else 0
            end = int(parts[1]) if len(parts) > 1 and parts[1] else len(data) - 1
            end = min(end, len(data) - 1)
            chunk = data[start : end + 1]
            return Response(
                content=chunk,
                status_code=206,
                headers={
                    "Content-Range": f"bytes {start}-{end}/{len(data)}",
                    "Accept-Ranges": "bytes",
                    "ETag": etag,
                },
            )
        return Response(content=data, status_code=200, headers={"Accept-Ranges": "bytes", "ETag": etag})

    return app
