# 中文儿童自适应阅读系统 (`reader`) — 升级与更新说明 (v0.2.0 & v0.3.0)

本升级包整合了系统的**全部代码更新、升级说明文档与两篇完整离线故事包**（含新创作的《我的世界：小猪鲁本与五彩护身符》小学一年级 10 分钟读本与 4 幕高清 3D 方块插画）。

---

## 一、本次打包包含的核心内容清单

1. **v0.3.0 更新（修复点击阅读无反应 & 开启 Tailscale 访问）**
   - `web/src/main.tsx`：修复首次安装未手动创建儿童档案时，点击“开始阅读”在 `if (!activeChild) return;` 处静默退出的问题；现会自动初始化并选中默认儿童档案 `🐻 六六`，并增加按钮加载态与顶部错误提示横幅。
   - `server/internal/migrations/0001_init.sql`：数据库初始化时自动写入默认儿童档案种子数据（`child_default_liuliu` / `六六`）。
   - `deploy/config.json`：将默认监听地址从 `"192.168.100.1:8380"` 更新为 `":8380"`，使服务同时响应局域网（`192.168.100.1:8380`）与 Tailscale 虚拟网卡（`100.x.y.z:8380`）。
2. **v0.2.0 更新（网页端全自动故事工作台、主题选择、无字剧透绘本插图与电脑端数字菜单）**
   - `web/src/pages/GuardianPage.tsx` & `web/src/api/client.ts`：网页端“🔒 监护人管理”新增 6 大预设主题 + 自定义主题选择器、一键全自动生成故事（含配套插图）并上架按钮，以及在线粘贴文本自动分句注音表单。
   - `server/internal/content/autogen.go` & `server/internal/api/api.go`：新增 `/api/v1/children/:id/story-plan`、`/api/v1/admin/quick-story`、`/api/v1/admin/auto-generate-story` 接口及“一句一图”无文字剧透插图渲染引擎。
   - `tools/menu.py` & `reader_menu.sh`：电脑端免命令行交互式数字菜单（输入数字 `0~7` 即可完成全部操作）。
3. **内置离线故事包 (`stories/`)**
   - `stories/story_mc_story_mode_v1.zip`：《我的世界：小猪鲁本与五彩护身符》（共 4 幕 20 句，小学一年级识字水平，10 分钟读完，含 4 幅 3D 方块动画风插画与完整多音字注音校验）。
   - `stories/story_xiaoxiong_v1.zip`：《小熊过河》（MVP 标准验收故事包，共 5 句）。

---

## 二、如何升级到现有 iStoreOS（逐步操作说明）

### 第一步：解压并执行一键部署/升级脚本
在电脑端解压 `reader_full_update_and_stories_v0.3.0.zip` 进入 `reader/` 目录，运行：
```bash
ROUTER_IP=192.168.100.1 READER_BASE=/mnt/sda1/reader sh deploy/deploy_to_istoreos.sh
```
*(或者如果你已经在 iStoreOS 上运行了旧版，只需在路由器 SSH 执行：`sed -i 's/"listen": *"[^"]*:8380"/"listen": ":8380"/' /mnt/sda1/reader/config.json && /etc/init.d/reader restart`)*

### 第二步：在网页端一键导入《我的世界：故事模式》故事包
打开 `http://192.168.100.1:8380`（或你的 Tailscale IP `http://100.x.y.z:8380`），点击右上角 **“🔒 监护人管理”**（默认 PIN `1234`），点击 **“📦 导入离线故事包 (.zip)”**，选择 `story_mc_story_mode_v1.zip` 即可上架阅读！
