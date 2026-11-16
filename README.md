# 中文儿童自适应阅读系统 (`reader`) — iStoreOS 局域网部署版

本项目依据 [中文儿童自适应阅读系统需求文档](https://bberg.notion.site/a1c98ca4bb52466eb62621d20c113bb9?source=copy_link) 与 [中文儿童自适应阅读系统技术方案 (iStoreOS 局域网部署)](https://bberg.notion.site/iStoreOS-5ca7b34742e44ec480a389a67764f408) 构建，并遵循 `pstack-workflow`（Poteto Mode）工程规范完成实现与端到端验证。

> 🤖 **给其它 AI Agent 的部署入口**：请直接阅读根目录下的 **`DEPLOY_ISTOREOS.md`**（或执行 `deploy/deploy_to_istoreos.sh`），其中包含面向 iStoreOS（2 GB 内存、无 Swap、BusyBox、`/mnt/sda1/reader` 外接硬盘挂载、LAN `192.168.100.1:8380`）的完整探测、交叉编译、上传、`procd` 守护进程注册与部署后验收断言流程。

## 1. 核心架构与设计约束落实

- **硬件与内存约束**：面向 2 GB 内存、无 Swap、BusyBox 的 iStoreOS 环境设计；采用 Go 单体静态二进制（内嵌前端构建产物）+ SQLite 3（WAL 模式，限制最大连接数与页面缓存），并通过 `procd` 设置 `GOMEMLIMIT=120MiB GOGC=50`，常驻内存控制在 `< 30 MB`。
- **外接硬盘挂载保护**：启动时强制检查 `/mnt/sda1/reader/.reader-disk` 挂载标记文件；未挂载时拒绝启动，并将 `TMPDIR` 与 SQLite `temp_store` 指向外接硬盘 `/mnt/sda1/reader/tmp`，防止误写路由器闪存或内存盘。
- **完整应用升级包**：GitHub Release 同时提供 amd64/arm64 服务二进制、`reader-runtime.tar.gz` 和统一 `checksums.txt`。前端、Go 服务及 SQL 迁移随静态二进制交付；`tools/*.py`、`reader_menu.sh` 与部署辅助文件随版本化运行时包交付。更新器在外接盘暂存并分别校验 SHA256，预检新二进制后原子切换；健康检查失败时回滚二进制与运行时指针。升级不覆盖 `config.json`、`data/`、`media/` 或 `backups/`。
- **局域网离线队列（IndexedDB）**：不依赖公网 HTTPS 与 Service Worker，前端使用 `IndexedDB`（`web/src/queue/offlineQueue.ts`）缓存 `sentence_views`、`character_events`、`audio_events`、`visibility_events`，断网重连后自动批量幂等同步。
- **多音字上下文注音与不可变故事版本**：`tools/story_packager.py` 在内容生产阶段完成分词、变调/轻声及多音字语境校验（如“通行/行走 `xíng`” vs “一行脚印/银行 `háng`”；“看着 `zhe`” vs “着急 `zháo`”），并在入库时校验 `story_versions.checksum` 防止历史行为指向被篡改的文本。
- **原始行为与派生能力分离**：保留原始点击事件，支持监护人将误触标记为无效（`is_valid = 0`，记录 `invalid_reason`），通过 `vocabulary_view` 视图动态计算 `needs_support`、`insufficient_evidence` 与 `likely_familiar` 状态。

## 2. 仓库目录结构

```text
reader/
├── DEPLOY_ISTOREOS.md                      # 供 AI Agent 自动部署到 iStoreOS 的标准操作规范 (SOP)
├── README.md                               # 项目总体说明
├── Makefile                                # 交叉编译 (arm64/amd64)、打包与自动化验证
├── server/
│   ├── cmd/reader/main.go                  # Go 服务端入口（挂载校验、TMPDIR 重定向、优雅退出）
│   ├── internal/
│   │   ├── config/config.go                # 配置文件解析、.reader-disk 校验、磁盘空间检查
│   │   ├── db/db.go                        # SQLite 连接池、WAL/FOREIGN_KEYS PRAGMA 与自动迁移
│   │   ├── migrations/
│   │   │   ├── 0001_init.sql               # 完整 DDL 表结构、索引与 vocabulary_view 视图
│   │   │   └── embed.go                    # SQL 迁移文件嵌入
│   │   ├── api/api.go                      # RESTful JSON 接口与监护人 PIN 鉴权中间件
│   │   ├── events/
│   │   │   ├── events.go                   # 批量事件幂等写入、后台时长扣除与异常停留过滤
│   │   │   └── events_test.go              # Go 单元测试
│   │   ├── content/content.go              # 离线故事包 (.zip) 校验、防路径穿越解压与版本控制
│   │   ├── media/media.go                  # 静态媒体文件服务（支持 HTTP Range 206 与 ETag 304）
│   │   ├── jobs/jobs.go                    # 超时会话自动收敛与每日 VACUUM INTO 在线备份
│   │   └── web/web.go                      # go:embed 前端静态构建产物
│   └── go.mod
├── web/                                    # Vite + Preact + TypeScript 单页应用
│   ├── index.html
│   ├── package.json
│   ├── tsconfig.json
│   ├── vite.config.ts
│   └── src/
│       ├── main.tsx                        # 书架首页、儿童档案切换与阅读模式选择
│       ├── styles.css                      # 儿童友好大字号与防重叠热区样式
│       ├── api/client.ts                   # REST API 客户端封装
│       ├── audio/player.ts                 # 整句朗读与单字上下文发音播放器
│       ├── components/SentenceDisplay.tsx  # 逐字独立热区与所属词高亮组件
│       ├── pages/ReaderPage.tsx            # 阅读器主视图（含 Page Visibility 前后台计时与断点续读）
│       ├── pages/GuardianPage.tsx          # 监护人管理端（PIN 保护、导入导出、误触修正、数据清除）
│       └── queue/offlineQueue.ts           # IndexedDB 离线事件队列与重连批量上报
├── tools/
│   ├── build_runtime_bundle.py             # 生成带清单的运行时发布包及首升迁移资源
│   ├── story_packager.py                   # 离线内容分词、多音字注音校验与 .zip 故事包生成工具
│   ├── dev_server.py                       # Python + SQLite 本地开发与自动化测试参考服务端
│   └── verify_e2e.py                       # 端到端全链路验收测试套件
└── deploy/
    ├── deploy_to_istoreos.sh               # 一键部署到 iStoreOS 的自动化脚本
    ├── reader.init                         # iStoreOS / OpenWrt procd 守护进程脚本 (/etc/init.d/reader)
    ├── 99-reader-hotplug                   # 外接硬盘挂载自动启动脚本 (/etc/hotplug.d/block/99-reader)
    └── config.json                         # iStoreOS 生产环境配置示例
```

## 3. 编译、验证与部署命令

```bash
# 1. 运行完整自动化测试（构建运行时包，验证 Python 端到端与 Go 更新器测试）
make test

# 或单独生成版本化运行时包（构建二进制时会自动生成）
make runtime-bundle

# 2. 运行端到端自动化验证套件（仅 Python 侧）
PYTHONPATH=tools python3 tools/verify_e2e.py

# 3. 生成样例故事离线包《小熊过河》
python3 tools/story_packager.py --output dist/story_xiaoxiong_v1.zip

# 4. 编译前端与 iStoreOS 静态二进制 (ARM64 / AMD64)
make web
make build-arm64
make build-amd64

# 5. 一键自动化部署到 iStoreOS (详见 DEPLOY_ISTOREOS.md)
ROUTER_IP=192.168.100.1 READER_BASE=/mnt/sda1/reader sh deploy/deploy_to_istoreos.sh
```
