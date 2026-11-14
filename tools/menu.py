#!/usr/bin/env python3
"""
tools/menu.py
Zero-Command Interactive Numbered Menu for Chinese Children Adaptive Reading System.
"""

import hashlib
import json
import os
import subprocess
import urllib.request
from pathlib import Path
from typing import Any, Dict, List, Optional

from story_packager import create_story_zip, segment_and_annotate
from vocab_planner import BUILTIN_THEMES, generate_vocabulary_plan

DEFAULT_SERVER = os.environ.get("READER_SERVER", "http://192.168.100.1:8380")
DEFAULT_PIN = os.environ.get("GUARDIAN_PIN", "1234")


def http_json(
    url: str,
    method: str = "GET",
    payload: Optional[Dict[str, Any]] = None,
    headers: Optional[Dict[str, str]] = None,
) -> Any:
    hdrs = {"Accept": "application/json"}
    if headers:
        hdrs.update(headers)
    data_bytes = None
    if payload is not None:
        data_bytes = json.dumps(payload).encode("utf-8")
        hdrs["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data_bytes, headers=hdrs, method=method)
    with urllib.request.urlopen(req, timeout=20) as resp:
        return json.loads(resp.read().decode("utf-8"))


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


def choose_child(server_url: str) -> Optional[Dict[str, Any]]:
    try:
        data = http_json(f"{server_url}/api/v1/children")
        children = data.get("children", [])
    except Exception as e:
        print(f"\n[!] 无法连接阅读器服务 ({server_url})：{e}")
        return None

    if not children:
        print("\n[!] 当前还没有创建儿童档案，请先在网页端创建儿童档案。")
        return None

    print("\n--- 儿童档案列表 ---")
    for idx, c in enumerate(children, 1):
        print(f"  [{idx}] {c.get('avatar', '🐻')} {c['name']} (ID: {c['id']})")
    choice = input("请选择儿童序号 (默认 1): ").strip() or "1"
    try:
        return children[int(choice) - 1]
    except Exception:
        return children[0]


def choose_theme() -> str:
    print("\n--- 🎨 请选择新故事主题 ---")
    for idx, t in enumerate(BUILTIN_THEMES, 1):
        print(f"  [{idx}] {t['name']} — {t['description']}")
    print(f"  [{len(BUILTIN_THEMES) + 1}] ✍️  输入自定义主题")
    choice = input("请选择主题序号 (默认 1): ").strip() or "1"
    try:
        c_idx = int(choice)
        if 1 <= c_idx <= len(BUILTIN_THEMES):
            return BUILTIN_THEMES[c_idx - 1]["name"]
    except Exception:
        pass
    custom = input("请输入自定义主题名称: ").strip()
    return custom or BUILTIN_THEMES[0]["name"]


def action_1_view_history(server_url: str) -> None:
    child = choose_child(server_url)
    if not child:
        return
    data = http_json(f"{server_url}/api/v1/children/{child['id']}/vocabulary")
    vocab = data.get("vocabulary", [])
    print(f"\n=== 【{child['name']}】字音掌握度推测明细（共 {len(vocab)} 项）===")
    if not vocab:
        print("  暂无有效独立阅读曝光记录。")
        return
    print(f"  {'汉字':<6} {'读音':<10} {'有效曝光':<10} {'求助次数':<10} {'推测状态'}")
    print("  " + "-" * 54)
    for v in vocab:
        status_cn = {
            "needs_support": "需要更多支持 (重点复习)",
            "insufficient_evidence": "证据不足 (继续观察)",
            "likely_familiar": "推测较熟悉",
        }.get(v["estimated_status"], v["estimated_status"])
        print(
            f"  {v['character']:<6} {v['pinyin']:<10} {v['exposure_count']:<10} {v['help_count']:<10} {status_cn}"
        )


def action_2_full_auto_story(server_url: str, pin: str) -> None:
    child = choose_child(server_url)
    if not child:
        return
    theme = choose_theme()
    try:
        res = http_json(
            f"{server_url}/api/v1/admin/auto-generate-story",
            method="POST",
            payload={
                "child_id": child["id"],
                "theme": theme,
                "auto_publish": True,
            },
            headers={"X-Guardian-Pin": pin},
        )
        print("\n" + "=" * 68)
        print(f"🎉 全自动生成并上架成功！《{res['title']}》（主题：{res['theme']}，版本 v{res.get('version_number', 1)}）")
        print(f"   重点复习词：{', '.join(res.get('review_words') or ['基础核心词'])}")
        print("-" * 68)
        print(res["raw_text"])
        print("=" * 68)
        print(f"✅ 已自动完成分词、多音字注音、配套绘本插图生成并发布到书架（故事 ID: {res.get('imported_story_id')}）！")
    except Exception as e:
        print(f"[!] 全自动生成失败：{e}")


def action_3_generate_plan(server_url: str) -> Optional[Dict[str, Any]]:
    child = choose_child(server_url)
    if not child:
        return None
    theme = choose_theme()
    data = http_json(f"{server_url}/api/v1/children/{child['id']}/vocabulary")
    vocab = data.get("vocabulary", [])
    plan = generate_vocabulary_plan(vocab)

    out_dir = Path("dist")
    out_dir.mkdir(exist_ok=True)
    out_file = out_dir / f"vocab_plan_{child['id']}.json"
    out_file.write_text(json.dumps(plan, ensure_ascii=False, indent=2), encoding="utf-8")

    print("\n" + "=" * 68)
    print(f"✨ 已根据【{child['name']}】的阅读历史生成【{theme}】选词方案与 AI 提示词：")
    print("=" * 68)
    print(f"【故事主题】：{theme}\n" + plan["story_generation_prompt"])
    print("=" * 68)
    print(f"📁 结构化选词方案已保存至：{out_file}")
    return plan


def action_4_custom_story(server_url: str, pin: str) -> None:
    print("\n--- 手动粘贴文本创建新故事 / 更新已有故事 ---")
    stories: List[Dict[str, Any]] = []
    try:
        stories = http_json(f"{server_url}/api/v1/stories").get("stories", [])
    except Exception:
        pass

    story_id = ""
    version_number = 1
    default_title = ""

    if stories:
        print("  [0] ➕ 创建全新故事")
        for idx, s in enumerate(stories, 1):
            print(
                f"  [{idx}] 🔄 更新已有故事：《{s['title']}》(当前版本 v{s['version_number']} -> 将升级为 v{s['version_number'] + 1})"
            )
        mode_choice = input("请选择操作序号 (默认 0 创建新故事): ").strip() or "0"
        if mode_choice != "0":
            try:
                picked = stories[int(mode_choice) - 1]
                story_id = picked["id"]
                version_number = int(picked["version_number"]) + 1
                default_title = picked["title"]
            except Exception:
                pass

    theme = choose_theme()
    title = input(f"请输入故事标题 [{default_title}]: ").strip() or default_title
    if not title:
        print("[!] 标题不能为空。")
        return
    desc = input("请输入一句话简介 (可选): ").strip()

    print("\n请粘贴或输入故事正文（支持多行，输入空行并按回车结束）：")
    lines = []
    while True:
        try:
            line = input()
        except EOFError:
            break
        if line.strip() == "":
            if lines:
                break
            continue
        lines.append(line)

    raw_text = "\n".join(lines).strip()
    raw_sentences = split_sentences(raw_text)
    if not raw_sentences:
        print("[!] 未检测到有效句子。")
        return

    if not story_id:
        story_id = f"story_{hashlib.md5(title.encode('utf-8')).hexdigest()[:8]}"
    version_id = f"{story_id}_v{version_number}"

    sentences_manifest = []
    for idx, s_text in enumerate(raw_sentences):
        sent_id = f"{version_id}_s{idx:02d}"
        tokens = segment_and_annotate(sent_id, s_text)
        sentences_manifest.append(
            {
                "id": sent_id,
                "sequence_index": idx,
                "text": s_text,
                "image_url": f"/media/stories/{story_id}/{version_id}/page_{idx:02d}.svg",
                "audio_url": f"/media/audio/{sent_id}_full.mp3",
                "tokens": tokens,
            }
        )

    full_text = "\n".join(raw_sentences)
    checksum = hashlib.sha256(full_text.encode("utf-8")).hexdigest()
    manifest = {
        "story_id": story_id,
        "title": title,
        "theme": theme,
        "description": desc,
        "cover_url": f"/media/stories/{story_id}/{version_id}/page_00.svg",
        "version_id": version_id,
        "version_number": version_number,
        "checksum": checksum,
        "status": "published",
        "sentences": sentences_manifest,
    }

    images_dir = input("请输入你准备好的配图文件夹路径 (直接按回车则自动生成绘本插图): ").strip() or None
    out_zip = Path("dist") / f"{version_id}.zip"
    create_story_zip(str(out_zip), manifest, images_dir=images_dir)
    print(f"\n✅ 离线故事包（含无文字剧透绘本插图）已打包至：{out_zip}")

    upload_now = input(f"是否立即上传并发布到阅读器 ({server_url})？[Y/n]: ").strip().lower()
    if upload_now in ("", "y", "yes"):
        try:
            res = http_json(
                f"{server_url}/api/v1/admin/quick-story",
                method="POST",
                payload={
                    "story_id": story_id,
                    "title": title,
                    "theme": theme,
                    "description": desc,
                    "raw_text": raw_text,
                },
                headers={"X-Guardian-Pin": pin},
            )
            print(
                f"🎉 上架成功！故事 ID: {res['imported_story_id']}，版本: {res['imported_version_id']}（共 {res['sentence_count']} 句）"
            )
        except Exception as e:
            print(f"[!] 在线发布失败（你仍可在网页端上传 {out_zip}）：{e}")


def action_5_export_backup(server_url: str, pin: str) -> None:
    try:
        data = http_json(f"{server_url}/api/v1/admin/export", headers={"X-Guardian-Pin": pin})
        out_dir = Path("dist")
        out_dir.mkdir(exist_ok=True)
        out_file = out_dir / "reader_backup_export.json"
        out_file.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
        print(f"\n✅ 数据备份已导出至：{out_file}")
    except Exception as e:
        print(f"\n[!] 导出失败：{e}")


def action_6_deploy_istoreos() -> None:
    router_ip = input("请输入 iStoreOS 路由器 LAN IP [默认 192.168.100.1]: ").strip() or "192.168.100.1"
    reader_base = input("请输入外接硬盘挂载目录 [默认 /mnt/sda1/reader]: ").strip() or "/mnt/sda1/reader"
    env = {**os.environ, "ROUTER_IP": router_ip, "READER_BASE": reader_base}
    subprocess.run(["sh", "deploy/deploy_to_istoreos.sh"], env=env, check=False)



def action_8_delete_story(server_url: str, pin: str) -> None:
    try:
        data = http_json(f"{server_url}/api/v1/stories")
        stories = data.get("stories", [])
    except Exception as e:
        print(f"\n[!] 获取故事列表失败：{e}")
        return

    if not stories:
        print("\n[!] 当前书架上没有可删除的故事。")
        return

    print("\n--- 📚 已上架故事列表 ---")
    for idx, st in enumerate(stories, 1):
        print(f"  [{idx}] 《{st['title']}》 (ID: {st['id']}, v{st['version_number']}, 共 {st['sentence_count']} 句)")
    choice = input("请输入要删除的故事序号 (直接回车取消): ").strip()
    if not choice:
        return
    try:
        target = stories[int(choice) - 1]
    except Exception:
        print("[!] 序号无效。")
        return

    try:
        res = http_json(
            f"{server_url}/api/v1/admin/stories/{target['id']}",
            method="DELETE",
            headers={"X-Guardian-Pin": pin},
        )
        print(f"\n🗑️ 已删除故事：《{res.get('title', target['title'])}》 (ID: {res['deleted']})")
    except Exception as e:
        print(f"\n[!] 删除失败：{e}")


def main() -> None:
    server_url = DEFAULT_SERVER
    pin = DEFAULT_PIN

    while True:
        print("\n" + "=" * 64)
        print(" 📖 奇妙中文阅读屋 — 监护人与内容制作交互菜单 (免打命令)")
        print(f"    当前连接服务: {server_url} | 监护人 PIN: {'*' * len(pin)}")
        print("=" * 64)
        print("  [1] 查看儿童档案与字音掌握度历史 (vocabulary_view)")
        print("  [2] ⚡ 选择主题 -> 根据阅读历史【一键全自动生成新故事+插图并上架】")
        print("  [3] ✨ 选择主题 -> 生成【选词方案 & AI 写故事提示词】")
        print("  [4] 📝 手动粘贴文本创建/更新故事（自动多音字注音、生成插图并上架）")
        print("  [5] 💾 导出阅读器完整数据备份 (JSON)")
        print("  [6] 🚀 一键编译并部署系统到 iStoreOS 路由器")
        print("  [7] ⚙️  修改连接地址或监护人 PIN 码")
        print("  [8] 🗑️  删除书架上的故事（自动保留孩子已学字词记录）")
        print("  [0] 退出菜单")
        print("-" * 64)

        choice = input("请输入数字选择功能 [0-8]: ").strip()
        if choice == "1":
            action_1_view_history(server_url)
        elif choice == "2":
            action_2_full_auto_story(server_url, pin)
        elif choice == "3":
            action_3_generate_plan(server_url)
        elif choice == "4":
            action_4_custom_story(server_url, pin)
        elif choice == "5":
            action_5_export_backup(server_url, pin)
        elif choice == "6":
            action_6_deploy_istoreos()
        elif choice == "7":
            server_url = input(f"请输入服务地址 [{server_url}]: ").strip() or server_url
            pin = input(f"请输入监护人 PIN 码 [{pin}]: ").strip() or pin
        elif choice == "8":
            action_8_delete_story(server_url, pin)
        elif choice == "0":
            print("再见！")
            break
        else:
            print("[!] 请输入 0 到 7 之间的数字。")


if __name__ == "__main__":
    main()
