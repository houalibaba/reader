#!/usr/bin/env python3
"""
tools/verify_e2e.py
Pstack-Workflow (Poteto Mode) Automated Verification & Reproduction Suite.
"""

import json
import os
import tempfile
import unittest
from pathlib import Path

from fastapi.testclient import TestClient

from dev_server import ReaderAppContext, create_app
from story_packager import create_story_zip
from vocab_planner import generate_vocabulary_plan, verify_draft_against_plan


class TestChineseAdaptiveReaderE2E(unittest.TestCase):
    def setUp(self) -> None:
        self.temp_dir = tempfile.TemporaryDirectory()
        self.root = Path(self.temp_dir.name)
        self.cfg_path = self.root / "config.json"
        self.cfg = {
            "listen": "127.0.0.1:8380",
            "data_dir": str(self.root / "reader_disk"),
            "session_timeout_minutes": 30,
            "guardian_pin": "1234",
            "backup": {"time": "03:30", "keep_daily": 7, "keep_weekly": 4},
            "upload_max_mb": 200,
            "min_free_disk_mb": 1,
        }
        self.cfg_path.write_text(json.dumps(self.cfg), encoding="utf-8")

    def tearDown(self) -> None:
        self.temp_dir.cleanup()

    def test_01_disk_mount_marker_protection(self) -> None:
        """Refuses to start when .reader-disk marker file is absent (prevents writing to router flash)."""
        os.makedirs(self.cfg["data_dir"], exist_ok=True)
        with self.assertRaises(RuntimeError) as ctx:
            ReaderAppContext(str(self.cfg_path))
        self.assertIn(".reader-disk", str(ctx.exception))

    def test_02_full_lifecycle_and_acceptance_scenarios(self) -> None:
        disk_dir = Path(self.cfg["data_dir"])
        disk_dir.mkdir(parents=True, exist_ok=True)
        (disk_dir / ".reader-disk").write_text("", encoding="utf-8")

        app = create_app(str(self.cfg_path))
        client = TestClient(app)

        # 1. Health check
        r = client.get("/api/v1/health")
        self.assertEqual(r.status_code, 200)
        self.assertTrue(r.json()["disk_space_ok"])

        # 2. Package & import 《小熊过河》 zip bundle
        zip_path = self.root / "story_xiaoxiong_v1.zip"
        create_story_zip(str(zip_path))

        # Unauthorized import without PIN must fail
        with open(zip_path, "rb") as f:
            r_unauth = client.post("/api/v1/admin/import", files={"package": ("story.zip", f, "application/zip")})
        self.assertEqual(r_unauth.status_code, 401)

        # Authorized import with X-Guardian-Pin
        with open(zip_path, "rb") as f:
            r_imp = client.post(
                "/api/v1/admin/import",
                headers={"X-Guardian-Pin": "1234"},
                files={"package": ("story.zip", f, "application/zip")},
            )
        self.assertEqual(r_imp.status_code, 200, r_imp.text)
        self.assertEqual(r_imp.json()["imported_story_id"], "story_xiaoxiong_guohe")
        self.assertEqual(r_imp.json()["sentence_count"], 5)

        # 3. Verify story version details & context-sensitive polyphone annotations
        r_ver = client.get("/api/v1/stories/story_xiaoxiong_guohe/versions/ver_xiaoxiong_v1")
        self.assertEqual(r_ver.status_code, 200)
        story_detail = r_ver.json()
        sent0 = story_detail["sentences"][0]
        self.assertEqual(sent0["text"], "小熊来到一条小河边。")

        he_tokens = [t for t in sent0["tokens"] if t["character"] == "河"]
        self.assertEqual(len(he_tokens), 1)
        he_tok = he_tokens[0]
        self.assertEqual(he_tok["word"], "小河")
        self.assertEqual(he_tok["pinyin"], "hé")

        # Verify polyphone "行" disambiguation across Sentence 3 ("通行" -> xíng) and Sentence 4 ("一行" -> háng, "行走" -> xíng)
        sent2_tokens = story_detail["sentences"][2]["tokens"]
        sent3_tokens = story_detail["sentences"][3]["tokens"]
        tongxing_xing = [t for t in sent2_tokens if t["character"] == "行"][0]
        yihang_hang = [t for t in sent3_tokens if t["character"] == "行" and t["word"] == "一行"][0]
        xingzou_xing = [t for t in sent3_tokens if t["character"] == "行" and t["word"] == "行走"][0]
        self.assertEqual(tongxing_xing["pinyin"], "xíng")
        self.assertEqual(yihang_hang["pinyin"], "háng")
        self.assertEqual(xingzou_xing["pinyin"], "xíng")

        # 4. Create Child Profile
        r_child = client.post("/api/v1/children", json={"name": "朵朵", "avatar": "🐰"})
        self.assertEqual(r_child.status_code, 201)
        child_id = r_child.json()["id"]

        # 5. Start Reading Session
        r_sess = client.post(
            "/api/v1/sessions",
            json={"child_id": child_id, "story_version_id": "ver_xiaoxiong_v1", "mode": "solo"},
        )
        self.assertEqual(r_sess.status_code, 201)
        sess_id = r_sess.json()["id"]
        self.assertFalse(r_sess.json()["resumed"])

        # 6. Acceptance Scenario 1 + 2 + 3
        batch_payload = {
            "current_sentence_index": 2,
            "session_status": "active",
            "sentence_views": [
                {
                    "id": "sv_test_01",
                    "sentence_id": sent0["id"],
                    "entered_at": "2026-11-12T08:00:00Z",
                    "exited_at": "2026-11-12T08:05:06Z",
                    "total_elapsed_ms": 306000,
                    "is_revisit": False,
                }
            ],
            "visibility_events": [
                {
                    "id": "ve_test_01",
                    "sentence_view_id": "sv_test_01",
                    "state": "visible",
                    "occurred_at": "2026-11-12T08:05:02Z",
                    "background_duration_ms": 300000,
                }
            ],
            "character_events": [
                {
                    "id": "ce_test_01",
                    "sentence_view_id": "sv_test_01",
                    "token_id": he_tok["id"],
                    "character": "河",
                    "pinyin_played": "hé",
                    "clicked_at": "2026-11-12T08:00:02Z",
                },
                {
                    "id": "ce_test_02",
                    "sentence_view_id": "sv_test_01",
                    "token_id": he_tok["id"],
                    "character": "河",
                    "pinyin_played": "hé",
                    "clicked_at": "2026-11-12T08:00:04Z",
                },
                {
                    "id": "ce_test_03",
                    "sentence_view_id": "sv_test_01",
                    "token_id": he_tok["id"],
                    "character": "河",
                    "pinyin_played": "hé",
                    "clicked_at": "2026-11-12T08:00:05Z",
                },
            ],
            "audio_events": [
                {
                    "id": "ae_test_01",
                    "sentence_view_id": "sv_test_01",
                    "audio_type": "sentence",
                    "played_at": "2026-11-12T08:05:03Z",
                    "play_duration_ms": 2800,
                    "completed": True,
                }
            ],
        }

        r_ev1 = client.post(f"/api/v1/sessions/{sess_id}/events", json=batch_payload)
        self.assertEqual(r_ev1.status_code, 200)
        r_ev2 = client.post(f"/api/v1/sessions/{sess_id}/events", json=batch_payload)
        self.assertEqual(r_ev2.status_code, 200)

        # Verify Scenario 2: Re-opening story resumes at sentence index 2
        r_resume = client.post(
            "/api/v1/sessions",
            json={"child_id": child_id, "story_version_id": "ver_xiaoxiong_v1", "mode": "solo"},
        )
        self.assertEqual(r_resume.status_code, 200)
        self.assertTrue(r_resume.json()["resumed"])
        self.assertEqual(r_resume.json()["current_sentence_index"], 2)

        # Verify Scenario 3 & Vocabulary View: "河" clicked 3 times -> needs_support
        r_vocab = client.get(f"/api/v1/children/{child_id}/vocabulary")
        self.assertEqual(r_vocab.status_code, 200)
        vocab_list = r_vocab.json()["vocabulary"]
        vocab_map = {f"{v['character']}_{v['pinyin']}": v for v in vocab_list}
        self.assertEqual(vocab_map["河_hé"]["help_count"], 3)
        self.assertEqual(vocab_map["河_hé"]["estimated_status"], "needs_support")

        # 6b. Verify Web API `/api/v1/children/:id/story-plan`
        r_plan = client.get(f"/api/v1/children/{child_id}/story-plan")
        self.assertEqual(r_plan.status_code, 200)
        plan = r_plan.json()
        self.assertEqual(len(plan["review_targets"]), 1)
        self.assertEqual(plan["review_targets"][0]["character"], "河")

        # 6c. Verify Full-Automatic Story Generation & Immediate Publishing (`POST /api/v1/admin/auto-generate-story`)
        r_auto = client.post(
            "/api/v1/admin/auto-generate-story",
            headers={"X-Guardian-Pin": "1234"},
            json={"child_id": child_id, "theme": "森林探险", "auto_publish": True},
        )
        self.assertEqual(r_auto.status_code, 200)
        auto_data = r_auto.json()
        self.assertTrue(auto_data["published"])
        self.assertGreaterEqual(auto_data["sentence_count"], 5)
        check_auto = verify_draft_against_plan(auto_data["sentences"], plan)
        self.assertTrue(check_auto["passed"], f"Auto-generated draft failed plan validation: {check_auto}")

        # 6d. Verify Zero-CLI Online Quick Story Updating (`POST /api/v1/admin/quick-story`)
        r_quick_update = client.post(
            "/api/v1/admin/quick-story",
            headers={"X-Guardian-Pin": "1234"},
            json={
                "story_id": "story_xiaoxiong_guohe",
                "title": "小熊过河（复习增强版）",
                "theme": "勇气与探索",
                "raw_text": "小熊来到一条小河边。河水哗哗地流淌。小熊稳稳地走过小河对岸！",
            },
        )
        self.assertEqual(r_quick_update.status_code, 200)
        self.assertEqual(r_quick_update.json()["version_number"], 2)


        # 6e. Verify Story Plan Prompt includes ALL mastered characters (no truncation) + Forgetting Curve + 10-min ratio
        synthetic_vocab = [
            {
                "character": ch,
                "pinyin": "py",
                "exposure_count": 6,
                "helped_view_count": 0,
                "help_count": 0,
                "last_seen_at": "2026-11-13T07:00:00Z",
                "estimated_status": "likely_familiar",
            }
            for ch in ["小", "熊", "来", "到", "一", "条", "边", "看", "着", "的", "地", "说", "前", "面", "有"]
        ] + [
            {
                "character": "河",
                "pinyin": "hé",
                "exposure_count": 3,
                "helped_view_count": 2,
                "help_count": 4,
                "last_help_at": "2026-11-10T07:00:00Z",
                "estimated_status": "needs_support",
            },
            {
                "character": "桥",
                "pinyin": "qiáo",
                "exposure_count": 3,
                "helped_view_count": 1,
                "help_count": 1,
                "last_seen_at": "2026-10-25T07:00:00Z",
                "estimated_status": "likely_familiar",
            },
        ]
        synth_plan = generate_vocabulary_plan(synthetic_vocab)
        self.assertEqual(len(synth_plan["familiar_anchors"]), 15)
        review_chars_in_plan = [t["character"] for t in synth_plan["review_targets"]]
        self.assertIn("河", review_chars_in_plan)
        self.assertIn("桥", review_chars_in_plan)
        for ch in ["小", "熊", "来", "到", "一", "条", "边", "看", "着", "的", "地", "说", "前", "面", "有"]:
            self.assertIn(f"{ch}(", synth_plan["story_generation_prompt"])
        self.assertIn("10分钟阅读量", synth_plan["story_generation_prompt"])
        self.assertIn("艾宾浩斯遗忘曲线", synth_plan["story_generation_prompt"])


        # 6g. Verify 100% real Pinyin annotation (never falling back to character itself) & dummy audio stripping
        auto_story_id = auto_data["imported_story_id"]
        auto_ver_detail = client.get(
            f"/api/v1/stories/{auto_story_id}/versions/{auto_data['imported_version_id']}"
        ).json()
        punct = set("，。！？、：；“”‘’（）")
        for sent in auto_ver_detail["sentences"]:
            self.assertEqual(sent["audio_url"], "")
            for tok in sent["tokens"]:
                if tok["character"] not in punct:
                    self.assertNotEqual(
                        tok["pinyin"],
                        tok["character"],
                        f"Character {tok['character']} fell back to character instead of pinyin!",
                    )

        # 6f. Verify Story Deletion removes story from bookshelf while preserving child vocabulary history
        auto_story_id = auto_data["imported_story_id"]
        r_del_story = client.delete(f"/api/v1/admin/stories/{auto_story_id}", headers={"X-Guardian-Pin": "1234"})
        self.assertEqual(r_del_story.status_code, 200)
        stories_after_del = [s["id"] for s in client.get("/api/v1/stories").json()["stories"]]
        self.assertNotIn(auto_story_id, stories_after_del)

        # Mark ce_test_03 as accidental touch via Guardian endpoint -> help_count drops to 2
        r_mark = client.post(
            "/api/v1/admin/events/ce_test_03/validity",
            headers={"X-Guardian-Pin": "1234"},
            json={"is_valid": False, "reason": "accidental_tap"},
        )
        self.assertEqual(r_mark.status_code, 200)
        r_vocab_after = client.get(f"/api/v1/children/{child_id}/vocabulary")
        vocab_map_after = {f"{v['character']}_{v['pinyin']}": v for v in r_vocab_after.json()["vocabulary"]}
        self.assertEqual(vocab_map_after["河_hé"]["help_count"], 2)
        self.assertEqual(vocab_map_after["河_hé"]["estimated_status"], "insufficient_evidence")

        # 7. Verify Media HTTP Range & ETag
        r_media = client.get(
            "/media/audio/ver_xiaoxiong_v1_s00_full.mp3",
            headers={"Range": "bytes=0-15"},
        )
        self.assertEqual(r_media.status_code, 206)
        self.assertEqual(len(r_media.content), 16)

        # 8. Verify Guardian Export & Child Deletion
        r_exp = client.get("/api/v1/admin/export", headers={"X-Guardian-Pin": "1234"})
        self.assertEqual(r_exp.status_code, 200)
        self.assertEqual(len(r_exp.json()["character_events"]), 3)

        r_del = client.delete(f"/api/v1/children/{child_id}", headers={"X-Guardian-Pin": "1234"})
        self.assertEqual(r_del.status_code, 200)
        r_vocab_deleted = client.get(f"/api/v1/children/{child_id}/vocabulary")
        self.assertEqual(r_vocab_deleted.json()["vocabulary"], [])


if __name__ == "__main__":
    unittest.main(verbosity=2)
