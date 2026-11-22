#!/usr/bin/env python3
import unittest
from datetime import datetime, timedelta, timezone
import sys, os

sys.path.insert(0, os.path.dirname(__file__))
import vocab_planner as vp


class TestVocabPlannerBudgetAndValidation(unittest.TestCase):
    def test_budget_strictly_capped_at_10(self):
        now = datetime(2026, 11, 21, 0, 0, 0, tzinfo=timezone.utc)
        old_ts = (now - timedelta(hours=200)).strftime("%Y-%m-%dT%H:%M:%SZ")
        records = []
        for ch in "甲乙丙丁戊己庚":
            records.append({
                "character": ch, "pinyin": "a", "exposure_count": 4,
                "helped_view_count": 3, "help_count": 4, "post_help_unhelped": 0,
                "last_help_at": old_ts, "last_seen_at": old_ts, "estimated_status": "needs_support",
            })
        for ch in "辛壬癸子":
            records.append({
                "character": ch, "pinyin": "b", "exposure_count": 1,
                "helped_view_count": 0, "help_count": 0, "post_help_unhelped": 1,
                "last_help_at": "", "last_seen_at": now.strftime("%Y-%m-%dT%H:%M:%SZ"),
                "estimated_status": "insufficient_evidence",
            })

        plan = vp.generate_vocabulary_plan(records)
        total = len(plan["review_targets"]) + len(plan["observe_targets"]) + len(plan["new_candidates"])
        self.assertEqual(len(plan["review_targets"]), 7)
        self.assertEqual(len(plan["observe_targets"]), 3)
        self.assertEqual(len(plan["new_candidates"]), 0)
        self.assertLessEqual(total, 10)

    def test_negative_budget_params_clamped_to_zero(self):
        rev, obs, new_q = vp.allocate_target_budget(
            [{"character": "河"}, {"character": "桥"}],
            [{"character": "雪"}],
            total_budget=-5, max_review=-1, max_observe=-2, max_new=-3,
        )
        self.assertEqual(rev, [])
        self.assertEqual(obs, [])
        self.assertEqual(new_q, 0)

        rev2, obs2, new_q2 = vp.allocate_target_budget(
            [{"character": "河"}, {"character": "桥"}],
            [{"character": "雪"}],
            total_budget=10, max_review=-1, max_observe=-1, max_new=-1,
        )
        self.assertEqual(rev2, [])
        self.assertEqual(obs2, [])
        self.assertEqual(new_q2, 0)

    def test_scaffold_chars_excluded_from_new_candidates(self):
        plan = vp.generate_vocabulary_plan([])
        scaffold = set(vp.FOUNDATIONAL_SCAFFOLD_WORDS.replace("、", ""))
        for cand in plan["new_candidates"]:
            self.assertNotIn(cand["character"], scaffold)

    def test_verify_rejects_short_missing_observe_and_single_sentence_cramming(self):
        plan = {
            "review_targets": [{"character": "河", "target_repetitions": 3}],
            "observe_targets": [{"character": "桥", "target_repetitions": 2}],
            "new_candidates": [{"character": "雪", "target_repetitions": 3}],
        }
        short_sents = [
            "小熊在河边看到飘落的白雪。",
            "他顺着小河向前走，白雪真美。",
            "最后他回望小河和白雪笑了。",
        ]
        res_short = vp.verify_draft_against_plan(short_sents, plan)
        self.assertFalse(res_short["passed"])
        self.assertEqual(len(res_short["missing_observe_targets"]), 1)

        crammed_sents = ["小熊在小河边看河水又望见一条大河，走过木桥赏白雪。"] + [
            "清晨小熊沿着森林小路走过坚固木桥欣赏飘落白雪。"
        ] * 15
        res_cram = vp.verify_draft_against_plan(crammed_sents, plan)
        self.assertFalse(res_cram["passed"])
        self.assertEqual(len(res_cram["missing_review_targets"]), 1)
        self.assertEqual(res_cram["missing_review_targets"][0]["actual_sentences"], 1)

        full_sents = ["清晨小熊沿着清澈的小河走过坚固的木桥赏白雪。"] * 16
        res_full = vp.verify_draft_against_plan(full_sents, plan)
        self.assertTrue(res_full["passed"], res_full["failure_reasons"])



    def test_verify_rejects_negative_target_repetitions(self):
        plan = {
            "review_targets": [{"character": "河", "target_repetitions": -3}],
            "observe_targets": [],
            "new_candidates": [],
        }
        sents = ["清晨小熊沿着森林小路走过坚固木桥欣赏飘落白雪。"] * 16
        res = vp.verify_draft_against_plan(sents, plan)
        self.assertFalse(res["passed"])
        self.assertEqual(len(res["missing_review_targets"]), 1)
        self.assertTrue(any("负数" in r for r in res["failure_reasons"]))

    def test_parse_bool_flag_rejects_false_string(self):
        import dev_server as ds
        self.assertFalse(ds.parse_bool_flag("false"))
        self.assertFalse(ds.parse_bool_flag("False"))
        self.assertFalse(ds.parse_bool_flag("0"))
        self.assertFalse(ds.parse_bool_flag(""))
        self.assertFalse(ds.parse_bool_flag(None))
        self.assertTrue(ds.parse_bool_flag(True))
        self.assertTrue(ds.parse_bool_flag("true"))
        self.assertTrue(ds.parse_bool_flag("1"))


if __name__ == "__main__":
    unittest.main()
