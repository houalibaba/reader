# Reader（昨日基线 v0.1.0/v0.2.0 → v0.3.3）手动增量更新与 AI Agent 执行指南

本文档面向负责在本地环境（Pop!_OS）或 iStoreOS 路由器上应用手动增量更新、编译并部署 `Reader` 的 AI Agent / 自动化助手。

> **兼容性说明**：本增量包已完整覆盖昨日（2026-11-12）最早基线 `v0.1.0` 至昨日最晚快照 `v0.2.0` 之后的所有变更，直接覆盖合并即可升级到最新 **v0.3.3**（含发音与拼音修复、故事删除、10分钟遗忘曲线科学配比提示词、GitHub 自动升级功能）。

---

## 一、相对昨日版本的全部演进与变更清单（共 27 个文件）

本次增量包整合了以下五阶段更新：
1. **v0.2.0（故事工作台与零命令行）**：网页端全自动故事生成、预设+自定义主题、无字剧透 SVG 插图、电脑端数字菜单（`reader_menu.sh` / `tools/menu.py`）、iStoreOS 部署规范（`DEPLOY_ISTOREOS.md` / `deploy_to_istoreos.sh`）。
2. **v0.3.0（开箱即用、自备配图、Tailscale 双栈与定制绘本）**：
   - `0001_init.sql` & `web/src/main.tsx`：首次安装自动初始化默认儿童档案 **“六六”**，修复点击“开始阅读”无响应问题。
   - `server/internal/content/autogen.go` & `GuardianPage.tsx`：支持网页端多选上传自备故事配图并按章节智能均分绑定。
   - `deploy/config.json`：默认监听调整为 `:8380`，同时支持局域网与 Tailscale 访问。
   - `stories/`：新增《我的世界：小猪鲁本与五彩护身符》（`story_mc_story_mode_v1.zip`）与《小熊过河》（`story_xiaoxiong_v1.zip`）。
3. **v0.3.1（GitHub Releases 在线自动升级 + CI/CD）**：
   - `.github/workflows/release.yml`：推送 `main` 或 `v*` 标签自动交叉编译 `amd64`/`arm64` 并发布 GitHub Release。
   - `server/internal/updater/`、`config.go`、`api.go`、`client.ts`、`GuardianPage.tsx`：新增检查更新、SHA256 校验、原子替换二进制、配合 `procd` 自动重启及网页端一键升级面板。
4. **v0.3.2（故事删除功能 + 10分钟阅读量与艾宾浩斯遗忘曲线科学配比提示词）**：
   - **故事一键删除（保留识字历史）**：新增 `DELETE /api/v1/admin/stories/{story_id}`、监护人面板 **“📚 已上架故事管理（支持一键删除）”** 与编辑器删除按钮、以及 `tools/menu.py` 选项 `[8]`。
   - **10分钟科学阅读量 + 艾宾浩斯遗忘曲线提示词重构**：100% 完整列出孩子已掌握的全部字词（90%~92% 主干），结合艾宾浩斯遗忘曲线公式 $R(t) = e^{-\Delta t / S}$ 筛选复习生字并动态配比新生字（8%~10%）。
5. **v0.3.3（修复部分故事无发音 & 拼音显示为小号汉字 Bug）**：
   - **根因 1（部分故事无声音）**：离线 `.zip` 故事包此前写入了 266 字节的空白 `ID3` 占位 `.mp3`，导致浏览器播放了 0 秒静音且不触发语音合成回退；同时部分 Linux/移动端浏览器缺失本地中文 `speechSynthesis` 语音库。
     - **修复**：后端自动过滤 `<= 512B` 的空白占位音频并清理内置 `.zip` 故事包；新增 `GET /api/v1/tts?text=...` 服务端中文语音合成与外置盘缓存接口（缓存至 `/mnt/usb2-1/reader/media/audio/tts_<hash>.mp3`），前端 `player.ts` 实现“真实录音 -> 服务端缓存语音流 -> 本地 Web Speech 防冲突降级”三级保障。
   - **根因 2（拼音标注变成小号汉字）**：原先 `content.go` / `story_packager.py` 的内置单字字典仅有 39 个汉字，未命中的字回退为汉字本身（`py = ch`），导致汉字上方渲染了更小的同一个汉字。
     - **修复**：新增 `server/internal/content/pinyin_dict.go`（内置 3,600+ 常用汉字与 180+ 多音字词组）并在 GitHub Actions 构建时通过 `pypinyin` 自动注入全部 **20,902 个 Unicode 汉字**；同时后端在读取旧故事时**自动检测并原地修复数据库中未注音的旧记录**，无需重新建故事即可恢复正确拼音。

### 完整文件清单（27 个：仅含新增与修改，无未变更文件）

* **CI/CD 与文档**：`.github/workflows/release.yml`、`CHANGELOG_UPDATES.md`、`DEPLOY_ISTOREOS.md`、`README.md`、`reader_menu.sh`
* **部署配置**：`deploy/config.json`、`deploy/deploy_to_istoreos.sh`
* **Go 后端**：
  * `server/internal/api/api.go`
  * `server/internal/config/config.go`
  * `server/internal/content/autogen.go`
  * `server/internal/content/content.go`
  * `server/internal/content/pinyin_dict.go`
  * `server/internal/migrations/0001_init.sql`
  * `server/internal/updater/updater.go`
  * `server/internal/updater/updater_test.go`
* **前端 SPA**：`web/src/api/client.ts`、`web/src/audio/player.ts`、`web/src/components/SentenceDisplay.tsx`、`web/src/main.tsx`、`web/src/pages/GuardianPage.tsx`、`web/src/styles.css`
* **工具脚本**：`tools/dev_server.py`、`tools/menu.py`、`tools/story_packager.py`、`tools/verify_e2e.py`、`tools/vocab_planner.py`
* **离线故事包**：`stories/story_mc_story_mode_v1.zip`、`stories/story_xiaoxiong_v1.zip`

---

## 二、AI Agent 标准执行步骤

### Step 1：覆盖合并增量文件
```bash
cp -r reader/* /path/to/existing/reader/
cp -r reader/.github /path/to/existing/reader/
chmod +x /path/to/existing/reader/reader_menu.sh /path/to/existing/reader/deploy/deploy_to_istoreos.sh /path/to/existing/reader/tools/menu.py /path/to/existing/reader/tools/vocab_planner.py
```

### Step 2：运行测试与构建前端 + 后端二进制
```bash
cd /path/to/existing/reader

# 1. 运行 Python 端到端校验与 Go 单元测试
PYTHONPATH=tools python3 tools/verify_e2e.py
cd server && go mod tidy && go test -v ./... && cd ..

# 2. 构建前端 SPA（输出至 server/internal/web/dist/ 供 Go embed）
make web

# 3. 交叉编译注入版本号的静态二进制（锁定 GOAMD64=v1 并禁用 AVX/BMI2，严格兼容 Intel Atom x5-Z8350 Cherry Trail）
VERSION=v0.3.8 make build-amd64
```

### Step 3：部署更新到 iStoreOS（保留现有数据与配置）
```bash
scp dist/reader-linux-amd64 root@192.168.100.1:/mnt/usb2-1/reader/reader.new
ssh root@192.168.100.1 "
  chmod +x /mnt/usb2-1/reader/reader.new &&
  /etc/init.d/reader stop &&
  mv /mnt/usb2-1/reader/reader.new /mnt/usb2-1/reader/reader &&
  /etc/init.d/reader start
"
```

---

## 三、部署后验收检查清单（Verification Gates）

1. 打开任意已有故事或新建故事，勾选“显示全句拼音提示”或点字，确认所有汉字均显示正确的带声调拼音（旧故事也会在打开时自动修复数据库拼音）。
2. 点击任意故事的单字与“🔊 整句朗读”，确认均有清晰中文发音（并自动缓存在 `/mnt/usb2-1/reader/media/audio/` 下）。
