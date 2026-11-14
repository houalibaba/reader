#!/usr/bin/env python3
"""
tools/vocab_planner.py
Adaptive Vocabulary Planner (Ebbinghaus Forgetting Curve + 10-Minute ZPD Scientific Ratio),
Multi-Theme Full-Auto Story Generator & Programmatic Validator.
"""

import argparse
import json
import math
import urllib.request
from datetime import datetime, timezone
from typing import Any, Dict, List, Optional, Tuple

from story_packager import CONTEXT_WORD_DICT

BUILTIN_THEMES: List[Dict[str, str]] = [
    {"id": "courage_river", "name": "勇气与探索（森林过河）", "description": "小熊在森林与小河边克服困难、勇敢探索"},
    {"id": "space_journey", "name": "太空与星际（月亮奇遇）", "description": "乘坐小飞船在月亮、星星与银河间旅行"},
    {"id": "ocean_treasure", "name": "海洋与沙滩（寻宝之旅）", "description": "在蔚蓝海边与小鱼、海浪一起寻找贝壳"},
    {"id": "dino_valley", "name": "恐龙与远古（高山探秘）", "description": "和温和的小恐龙一起穿越大山与森林"},
    {"id": "friendship_orchard", "name": "友谊与分享（果园野餐）", "description": "和好朋友在春风与花草间分享美食与快乐"},
    {"id": "daily_habits", "name": "日常与好习惯（快乐成长）", "description": "早起看日出、爱护玩具与独立完成小任务"},
    {"id": "minecraft_story", "name": "我的世界·故事模式", "description": "在方块世界里搭木桥、点亮火把、勇敢探险"},
]

# Expanded 1st-grade core character & word pool for scientific new-character introduction
COMMON_STARTER_CHARS: List[Tuple[str, str, str]] = [
    ("水", "shuǐ", "河水"),
    ("火", "huǒ", "火把"),
    ("山", "shān", "高山"),
    ("木", "mù", "木桥"),
    ("日", "rì", "日出"),
    ("月", "yuè", "月亮"),
    ("大", "dà", "大家"),
    ("小", "xiǎo", "小熊"),
    ("上", "shàng", "上面"),
    ("下", "xià", "下面"),
    ("左", "zuǒ", "左边"),
    ("右", "yòu", "右边"),
    ("风", "fēng", "春风"),
    ("雨", "yǔ", "大雨"),
    ("花", "huā", "花朵"),
    ("草", "cǎo", "青草"),
    ("鸟", "niǎo", "小鸟"),
    ("鱼", "yú", "小鱼"),
    ("石", "shí", "石头"),
    ("光", "guāng", "星光"),
    ("桥", "qiáo", "木桥"),
    ("林", "lín", "森林"),
    ("灯", "dēng", "红灯"),
    ("宝", "bǎo", "宝石"),
    ("船", "chuán", "飞船"),
    ("海", "hǎi", "大海"),
    ("星", "xīng", "星星"),
    ("云", "yún", "白云"),
    ("路", "lù", "小路"),
    ("家", "jiā", "回家"),
]

# Foundational sight-word scaffold so 10-minute stories always have sufficient grammar connectors
FOUNDATIONAL_SCAFFOLD_WORDS = (
    "的、一、是、在、有、和、小熊、六六、我、你、他、她、它、们、这、那、来、到、去、走、"
    "看、见、说、听、想、要、会、能、可、以、好、大、小、多、少、上、下、里、外、前、后、"
    "左、右、中、天、地、人、手、心、开、关、高、兴、快、乐、平、安、朋、友"
)

# 10-Minute Reading Scientific Parameters (1st Grade / Early Reader: ~38-42 chars/min)
TEN_MIN_READING_CONFIG = {
    "reading_duration_minutes": 10,
    "target_total_chars_min": 360,
    "target_total_chars_max": 450,
    "target_sentences_min": 16,
    "target_sentences_max": 20,
    "target_chars_per_sentence": "18~24",
    "mastered_ratio_pct": "90%~92%",
    "unfamiliar_ratio_pct": "8%~10%",
    "target_unfamiliar_char_budget": 10,  # ~10 distinct review+new chars × 3-4 reps = ~34 char occurrences (~8.5% of 400 chars)
}


def match_words_for_char_pinyin(character: str, pinyin: str) -> List[str]:
    matched: List[str] = []
    for word, (pinyins, _) in CONTEXT_WORD_DICT.items():
        for ch, py in zip(word, pinyins):
            if ch == character and py == pinyin:
                matched.append(word)
                break
    return matched if matched else [character]


def parse_iso_timestamp(ts_str: str) -> Optional[datetime]:
    if not ts_str or not ts_str.strip():
        return None
    s = ts_str.strip()
    if s.endswith("Z"):
        s = s[:-1] + "+00:00"
    try:
        dt = datetime.fromisoformat(s)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt
    except Exception:
        return None


def compute_forgetting_curve_metrics(record: Dict[str, Any], now: Optional[datetime] = None) -> Dict[str, Any]:
    """
    Computes Ebbinghaus Forgetting Curve retention R(t) = exp(-delta_t / S)
    where S (stability in hours) grows with unhelped solo exposures and shrinks with click-help count.
    """
    if now is None:
        now = datetime.now(timezone.utc)

    exp_count = max(1, int(record.get("exposure_count") or 1))
    helped_views = int(record.get("helped_view_count") or 0)
    help_count = int(record.get("help_count") or 0)
    unhelped_count = max(0, exp_count - helped_views)

    last_ts = parse_iso_timestamp(record.get("last_help_at") or "") or parse_iso_timestamp(
        record.get("last_seen_at") or ""
    )
    if last_ts is not None:
        elapsed_hours = max(0.25, (now - last_ts).total_seconds() / 3600.0)
    else:
        elapsed_hours = 12.0 if help_count > 0 else 4.0

    # Ebbinghaus stability S (hours): base 18h, scales by 2.2^unhelped, penalized by help_count
    stability_hours = (18.0 * (2.2 ** min(unhelped_count, 6))) / (1.0 + 0.85 * help_count)
    retention = math.exp(-elapsed_hours / max(1.0, stability_hours))
    if help_count > 2:
        # High click-help count forces immediate forgetting-curve review priority
        retention = min(retention, 0.45 / (1.0 + 0.15 * (help_count - 2)))
    elif exp_count < 3 and unhelped_count < 2:
        retention = min(retention, 0.72)

    retention_pct = max(5, min(99, int(round(retention * 100))))
    urgency = (1.0 - retention) + 0.25 * help_count + (0.20 if exp_count < 3 else 0.0)

    if help_count > 2:
        stage = f"高频求助易错期（求助 {help_count} 次，记忆保留率 {retention_pct}%）"
    elif retention < 0.60:
        stage = f"艾宾浩斯遗忘临界期（记忆保留率 {retention_pct}%）"
    elif exp_count < 3:
        stage = f"艾宾浩斯初次巩固期（独立曝光 {exp_count} 次，保留率 {retention_pct}%）"
    elif retention < 0.78:
        stage = f"艾宾浩斯周期复习节点（记忆保留率 {retention_pct}%）"
    else:
        stage = f"已稳固掌握（记忆保留率 {retention_pct}%）"

    return {
        "retention": retention,
        "retention_pct": retention_pct,
        "elapsed_hours": round(elapsed_hours, 1),
        "stability_hours": round(stability_hours, 1),
        "urgency": round(urgency, 3),
        "stage": stage,
    }


def fetch_vocabulary_from_api(server_url: str, child_id: str) -> List[Dict[str, Any]]:
    url = f"{server_url.rstrip('/')}/api/v1/children/{child_id}/vocabulary"
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        data = json.loads(resp.read().decode("utf-8"))
        return data.get("vocabulary", [])


def generate_vocabulary_plan(
    vocab_records: List[Dict[str, Any]],
    max_review_chars: int = 7,
    max_observe_chars: int = 4,
    max_new_chars: int = 6,
) -> Dict[str, Any]:
    now = datetime.now(timezone.utc)
    seen_chars = {r["character"] for r in vocab_records}

    forgetting_review_list: List[Dict[str, Any]] = []
    forgetting_observe_list: List[Dict[str, Any]] = []
    familiar_anchors: List[Dict[str, Any]] = []

    for r in vocab_records:
        ch = r["character"]
        py = r["pinyin"]
        status = r.get("estimated_status") or "insufficient_evidence"
        words = match_words_for_char_pinyin(ch, py)
        fc = compute_forgetting_curve_metrics(r, now)

        item = {
            "character": ch,
            "pinyin": py,
            "help_count": int(r.get("help_count") or 0),
            "exposure_count": int(r.get("exposure_count") or 0),
            "recommended_words": words,
            "retention_pct": fc["retention_pct"],
            "forgetting_stage": fc["stage"],
            "urgency": fc["urgency"],
        }

        if status == "needs_support" or (status == "likely_familiar" and fc["retention"] < 0.78):
            item["target_repetitions"] = 3
            item["reason"] = f"{fc['stage']}，需在 10 分钟故事不同章节自然复现 ≥3 次"
            forgetting_review_list.append(item)
        elif status == "insufficient_evidence":
            item["target_repetitions"] = 2
            item["reason"] = f"{fc['stage']}，需在故事中补充复现 ≥2 次以跨过遗忘拐点"
            forgetting_observe_list.append(item)
        else:
            item["target_repetitions"] = 1
            item["reason"] = f"{fc['stage']}，作为已掌握熟字全面复用"
            familiar_anchors.append(item)

    forgetting_review_list.sort(key=lambda x: x["urgency"], reverse=True)
    forgetting_observe_list.sort(key=lambda x: x["urgency"], reverse=True)

    review_targets = forgetting_review_list[:max_review_chars]
    observe_targets = forgetting_observe_list[:max_observe_chars]

    # Scientific 10-minute reading unfamiliar character budget (~10 distinct unfamiliar/review chars total)
    total_budget = TEN_MIN_READING_CONFIG["target_unfamiliar_char_budget"]
    forgetting_curve_count = len(review_targets) + len(observe_targets)
    scientific_new_quota = max(3, min(max_new_chars, total_budget - forgetting_curve_count))

    new_candidates: List[Dict[str, Any]] = []
    for ch, py, sample_word in COMMON_STARTER_CHARS:
        if ch not in seen_chars:
            words = match_words_for_char_pinyin(ch, py)
            if sample_word not in words:
                words = [sample_word] + words
            new_candidates.append(
                {
                    "character": ch,
                    "pinyin": py,
                    "recommended_words": words,
                    "target_repetitions": 3,
                    "reason": "按 10 分钟阅读科学比例（8%~10% 生字率）新引入的拓展生字，建议跨章节复现 ≥3 次",
                }
            )
            if len(new_candidates) >= scientific_new_quota:
                break

    # Include ALL mastered characters and words without any truncation!
    all_mastered_items_str = "、".join(
        f"{t['character']}({t['pinyin']}：{'/'.join(t['recommended_words'])})" for t in familiar_anchors
    )

    forgetting_all = review_targets + observe_targets

    prompt_lines = [
        "请为正在学习中文阅读的儿童编写一篇【10分钟阅读量（约 360~450 字，共 4 个章节、16~20 句话）】的趣味分级绘本故事。",
        "",
        "【一、十分钟阅读科学字数与最近发展区（ZPD）配比】",
        "- 目标阅读时长：10 分钟（按小学一年级儿童点读与理解速度 36~45 字/分钟测算）。",
        "- 篇幅与句长：共 4 个章节、16~20 句话（每页 1 句话，每句 18~24 个汉字，总字数 360~450 字）。",
        "- 科学识字配比（90%~92% 已掌握熟字 + 8%~10% 目标生字）：",
        f"  1) 已掌握字词基座（占全篇总字数的 90%~92%，约 330~410 字次）：完整复用下方列出的全部已掌握字词（共 {len(familiar_anchors)} 个历史已掌握字音 + 基础高频熟字基座）。",
        f"  2) 科学比例生字池（占全篇总字数的 8%~10%，约 30~40 字次）：由系统根据【艾宾浩斯遗忘曲线】算出的 {len(forgetting_all)} 个待复习生字 + {len(new_candidates)} 个全新拓展生字组成，每个生字在文中不同章节自然复现 2~3 次。",
        "",
        "【二、孩子已掌握的全部字词库（占全篇 90%~92%，请全面复用作为故事主干，切勿超纲使用未学过的生僻字）】",
    ]

    if familiar_anchors:
        prompt_lines.append(f"- 历史已掌握的全部字词（共 {len(familiar_anchors)} 项，完整列出）：{all_mastered_items_str}")
    else:
        prompt_lines.append("- 历史已掌握字词：当前为初始建档阶段，尚无完整历史熟字记录，请直接使用下方基础高频熟字基座。")
    prompt_lines.append(f"- 基础高频熟字与语法脚手架：{FOUNDATIONAL_SCAFFOLD_WORDS}")

    prompt_lines.append("")
    prompt_lines.append("【三、十分钟阅读科学比例生字池（占全篇 8%~10%：艾宾浩斯遗忘曲线复习字 + 新拓展生字）】")

    if review_targets:
        review_str = "、".join(
            f"「{t['character']}({t['pinyin']})」（推荐词组：{'/'.join(t['recommended_words'])}；状态：{t.get('forgetting_stage', '需重点复习')}；要求自然复现 ≥{t['target_repetitions']} 次）"
            for t in review_targets
        )
        prompt_lines.append(f"1. 遗忘曲线急需复习生字（高频求助/接近遗忘临界点）：{review_str}")

    if observe_targets:
        obs_str = "、".join(
            f"「{t['character']}({t['pinyin']})」（推荐词组：{'/'.join(t['recommended_words'])}；状态：{t.get('forgetting_stage', '初次巩固期')}；要求自然复现 ≥{t['target_repetitions']} 次）"
            for t in observe_targets
        )
        prompt_lines.append(f"2. 遗忘曲线待巩固生字（短期记忆巩固节点）：{obs_str}")

    if not review_targets and not observe_targets:
        prompt_lines.append("1. 遗忘曲线待复习生字：当前无处于遗忘临界点的历史生字。")

    if new_candidates:
        new_str = "、".join(
            f"「{t['character']}({t['pinyin']}，推荐词组：{'/'.join(t['recommended_words'][:2])})」（要求跨章节自然复现 ≥{t['target_repetitions']} 次）"
            for t in new_candidates
        )
        prompt_lines.append(
            f"3. 按科学比例新引入的拓展生字（共 {len(new_candidates)} 个，占全篇约 4%~5% 字次）：{new_str}"
        )

    prompt_lines.extend(
        [
            "",
            "【四、排版与多音字语境强约束】",
            "- 请直接输出故事正文，每行严格对应 1 句话（共 16~20 行，或精简模式至少 5~8 行），不要输出多余解释。",
            "- 多音字必须严格匹配上述指定读音的语境词组（例如要求复习 háng 时使用“一行/银行”，要求复习 xíng 时使用“行走/通行”）。",
            "- 遗忘曲线复习字与新拓展生字应分布在故事的起承转合不同段落中（首次出现有上下文线索、中段复现、结尾巩固），严禁生硬堆砌。",
        ]
    )

    return {
        "summary": {
            "total_tracked_char_pinyins": len(vocab_records),
            "needs_support_count": len(review_targets),
            "insufficient_evidence_count": len(observe_targets),
            "likely_familiar_count": len(familiar_anchors),
            "forgetting_curve_total": len(forgetting_all),
            "new_candidates_count": len(new_candidates),
            "reading_duration_minutes": 10,
            "target_total_chars": "360~450",
        },
        "reading_science": TEN_MIN_READING_CONFIG,
        "available_themes": BUILTIN_THEMES,
        "review_targets": review_targets,
        "observe_targets": observe_targets,
        "familiar_anchors": familiar_anchors,
        "new_candidates": new_candidates,
        "story_generation_prompt": "\n".join(prompt_lines),
    }


def build_consistent_image_prompts(theme: str, sentences: List[str]) -> List[str]:
    style_anchor = (
        "Children's picture book watercolor illustration, warm Ghibli-inspired pastel palette, "
        "consistent protagonist: a cute round brown bear cub wearing a small yellow scarf, "
        "clean composition, text-free, no letters, no words, no watermarks"
    )
    return [f"{style_anchor}. Theme: {theme}. Scene {i + 1} action: {s}" for i, s in enumerate(sentences)]


def auto_generate_story_from_plan(
    plan: Dict[str, Any],
    theme: str = "勇气与探索（森林过河）",
    llm_api_url: str = "",
    llm_api_key: str = "",
    llm_model: str = "nvidia/nemotron-3-ultra-550b-a55b:free",
) -> Dict[str, Any]:
    theme = (theme or "勇气与探索（森林过河）").strip()
    review_words: List[str] = []
    review_chars: List[str] = []
    for t in plan.get("review_targets", []):
        review_chars.append(t["character"])
        review_words.extend(t.get("recommended_words") or [t["character"]])

    observe_words: List[str] = []
    for t in plan.get("observe_targets", []):
        observe_words.extend(t.get("recommended_words") or [t["character"]])

    new_words: List[str] = []
    for t in plan.get("new_candidates", []):
        new_words.extend(t.get("recommended_words") or [t["character"]])

    if llm_api_key and llm_api_url:
        try:
            payload = {
                "model": llm_model or "nvidia/nemotron-3-ultra-550b-a55b:free",
                "messages": [
                    {
                        "role": "system",
                        "content": (
                            "你是一位专业的中文儿童分级阅读作家。请严格按照用户提供的【已掌握全部字词库】与【遗忘曲线+新字科学配比】要求，"
                            "仅输出纯文本故事正文（每句一行，不要标题、序号或多余解释）。"
                        ),
                    },
                    {"role": "user", "content": f"故事主题：{theme}\n" + plan["story_generation_prompt"]},
                ],
                "temperature": 0.7,
            }
            req = urllib.request.Request(
                llm_api_url,
                data=json.dumps(payload).encode("utf-8"),
                headers={
                    "Content-Type": "application/json",
                    "Authorization": f"Bearer {llm_api_key}",
                },
                method="POST",
            )
            with urllib.request.urlopen(req, timeout=35) as resp:
                resp_data = json.loads(resp.read().decode("utf-8"))
                content_text = resp_data["choices"][0]["message"]["content"].strip()
                sents = [s.strip() for s in content_text.splitlines() if s.strip()]
                if len(sents) >= 3:
                    main_word = review_words[0] if review_words else "小熊"
                    return {
                        "title": f"关于{main_word}的{theme}故事",
                        "theme": theme,
                        "description": f"主题【{theme}】· 基于全部已掌握字词与遗忘曲线科学配比生成的 10 分钟分级故事。",
                        "raw_text": "\n".join(sents),
                        "sentences": sents,
                        "image_prompts": build_consistent_image_prompts(theme, sents),
                        "generator": "llm_api",
                        "review_words": review_words,
                        "review_chars": review_chars,
                    }
        except Exception:
            pass

    w1 = review_words[0] if len(review_words) > 0 else "小河"
    w2 = review_words[1] if len(review_words) > 1 else w1
    w3 = review_words[2] if len(review_words) > 2 else w1
    obs = observe_words[0] if len(observe_words) > 0 else "木桥"
    new_w = new_words[0] if len(new_words) > 0 else "春风"

    if any(k in theme for k in ("太空", "星", "月")):
        title = f"小熊的太空{w1}奇遇"
        sentences = [
            f"夜晚吹着温和的{new_w}，小熊望着月亮想去看美丽的{w1}。",
            f"他坐上奇妙飞船，看着银河里像{w2}一样闪亮的星光。",
            f"鹿阿姨走过来告诉他，前面有一座彩虹{obs}可以安全通行。",
            f"小熊勇敢地迈开脚步走上{obs}，在星空下找到了发光的{w3}。",
            f"小熊稳稳地完成太空探索，带着{w1}的礼物开心极了！",
        ]
    elif any(k in theme for k in ("海", "鱼", "沙")):
        title = f"小熊与海边{w1}寻宝"
        sentences = [
            f"迎着温暖的{new_w}，小熊来到通往大海的{w1}。",
            f"他看着哗哗流淌的{w2}，水里有一群小鱼在快乐游动。",
            f"鹿阿姨温和地告诉他，前面有一座安全的{obs}可以通行。",
            f"小熊迈开脚步走过{obs}，在金黄的沙滩与{w3}捡到了漂亮贝壳。",
            f"小熊把贝壳放进小篮子，回望美丽的{w1}开心极了！",
        ]
    elif any(k in theme for k in ("恐龙", "远古", "山")):
        title = f"小熊与小恐龙的{w1}探秘"
        sentences = [
            f"清晨吹着柔和的{new_w}，小熊和小恐龙来到远古山谷的{w1}。",
            f"他们看着哗哗流淌的{w2}，想要去对岸的森林寻找美味果子。",
            f"小恐龙温和地告诉他，前面有一座坚固的{obs}可以安全通行。",
            f"小熊勇敢地迈开脚步行走，在{obs}与{w3}留下了一行脚印。",
            f"他们稳稳地到达对岸的森林，回望美丽的{w1}开心极了！",
        ]
    elif any(k in theme for k in ("友谊", "分享", "果")):
        title = f"小熊在{w1}的分享野餐"
        sentences = [
            f"沐浴着温暖的{new_w}，小熊带着甜果子来到美丽的{w1}。",
            f"他看着清澈的{w2}，心里盼着好朋友们快点到来。",
            f"鹿阿姨走过来温和地告诉他，朋友们正从前面的{obs}安全通行。",
            f"大家迈开脚步走过{obs}，围坐在风景优美的{w3}一起分享果子。",
            f"小熊和朋友们在欢笑声中度过美好时光，看着{w1}开心极了！",
        ]
    elif any(k in theme for k in ("日常", "习惯")):
        title = f"小熊在{w1}的好习惯"
        sentences = [
            f"太阳升起吹来温和的{new_w}，小熊早早起床来到门前的{w1}。",
            f"他看着干净清澈的{w2}，认真地把小脸和小手洗得干干净净。",
            f"鹿阿姨夸奖他真棒，并告诉他前面的{obs}已经可以安全通行。",
            f"小熊背起小书包走上{obs}，沿着整洁的{w3}稳稳地向前行走。",
            f"小熊按时到达森林学校，想起美丽的{w1}心里开心极了！",
        ]
    else:
        title = f"小熊与{w1}的新冒险"
        sentences = [
            f"小熊来到美丽的{w1}，看到清澈的{new_w}。",
            f"他看着远处的{w2}，心里想要勇敢地向前行走。",
            f"鹿阿姨温和地告诉他，前面有一座安全的{obs}可以通行。",
            f"小熊迈开脚步走上{obs}，低头又看见哗哗流淌的{w3}。",
            f"小熊稳稳地到达对岸的森林，回望{w1}开心极了！",
        ]

    full_joined = "".join(sentences)
    for t in plan.get("review_targets", []):
        ch = t["character"]
        rw = (t.get("recommended_words") or [ch])[0]
        while full_joined.count(ch) < int(t.get("target_repetitions", 3)):
            sentences.append(f"小熊喜欢{rw}，在{rw}旁边留下了开心的脚印。")
            full_joined = "".join(sentences)

    return {
        "title": title,
        "theme": theme,
        "description": (
            f"主题【{theme}】· 结合已掌握字词与遗忘曲线重点词（{'、'.join(review_words[:3])}）生成的自适应故事。"
            if review_words
            else f"主题【{theme}】· 根据当前阅读阶段全自动生成的趣味阅读故事。"
        ),
        "raw_text": "\n".join(sentences),
        "sentences": sentences,
        "image_prompts": build_consistent_image_prompts(theme, sentences),
        "generator": "smart_template",
        "review_words": review_words,
        "review_chars": review_chars,
    }


def verify_draft_against_plan(sentences: List[str], plan: Dict[str, Any], max_sentence_len: int = 28) -> Dict[str, Any]:
    full_text = "\n".join(sentences)
    char_counts: Dict[str, int] = {}
    for ch in full_text:
        char_counts[ch] = char_counts.get(ch, 0) + 1

    missing_reviews = []
    for item in plan.get("review_targets", []):
        ch = item["character"]
        target = int(item.get("target_repetitions", 3))
        actual = char_counts.get(ch, 0)
        if actual < target:
            missing_reviews.append({"character": ch, "expected_min": target, "actual": actual})

    overlong_sentences = []
    for idx, s in enumerate(sentences):
        if len(s) > max_sentence_len:
            overlong_sentences.append({"sentence_index": idx, "length": len(s), "text": s})

    passed = (len(missing_reviews) == 0) and (len(overlong_sentences) == 0) and (len(sentences) >= 3)
    return {
        "passed": passed,
        "sentence_count": len(sentences),
        "missing_review_targets": missing_reviews,
        "overlong_sentences": overlong_sentences,
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Adaptive Vocabulary Planner & Story Prompt Generator")
    parser.add_argument("--server", default="http://127.0.0.1:8380", help="Reader server base URL")
    parser.add_argument("--child-id", required=True, help="Child profile ID")
    args = parser.parse_args()

    vocab = fetch_vocabulary_from_api(args.server, args.child_id)
    plan = generate_vocabulary_plan(vocab)
    print(json.dumps(plan, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
