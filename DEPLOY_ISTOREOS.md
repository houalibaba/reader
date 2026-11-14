# iStoreOS 自动化部署手册（Agent 专用执行规范 / SOP）

> **致部署 Agent（如 OpenCode / Claude Code / Cursor / 自动化脚本 Agent）**：
> 本手册是专为 AI Agent 编写的**确定性、可验证部署指南**。请严格按本文档的 **Phase 0 → Phase 4** 顺序执行，每完成一步必须运行对应的验证断言（Verification Gate），禁止跳过挂载校验或将任何临时文件/数据库写入路由器闪存（`/` 或 `/tmp`）。

---

## 1. 目标 iStoreOS 环境与硬性约束

根据 [中文儿童自适应阅读系统技术方案 (iStoreOS 局域网部署)](https://bberg.notion.site/iStoreOS-5ca7b34742e44ec480a389a67764f408) 及 [中文儿童自适应阅读系统需求文档](https://bberg.notion.site/a1c98ca4bb52466eb62621d20c113bb9?source=copy_link)，目标设备环境参数如下：

| 维度 | 目标环境参数与约束 | Agent 操作铁律 |
| :--- | :--- | :--- |
| **操作系统** | iStoreOS（基于 OpenWrt / BusyBox，无完整 glibc 开发链） | 必须在宿主机交叉编译为**静态单文件二进制**（`-extldflags '-static'`），禁止在路由器上跑编译任务 |
| **内存与 Swap** | 总内存 `1.95 GB`，可用约 `1.11 GB`，**无 Swap** | 进程设置 `GOMEMLIMIT=120MiB GOGC=50`，常驻内存目标 `< 30 MB`（硬上限 `80 MB`），严禁在设备上运行 TTS/AI/图片转码 |
| **外接存储路径** | 默认 `/mnt/sda1/reader`（若盘符为 `sdb1` 则对应 `/mnt/sdb1/reader`） | 必须先探测实际挂载点并创建 `.reader-disk` 标记文件；**未检测到 `.reader-disk` 严禁启动服务** |
| **临时目录 (`TMPDIR`)** | 路由器默认 `/tmp` 为内存盘（tmpfs） | 必须将环境变量 `TMPDIR` 指向外接硬盘 `/mnt/sda1/reader/tmp`，防止上传/解压撑爆内存触发 OOM |
| **网络与安全** | 仅监听局域网 LAN IP（默认 `192.168.100.1:8380`） | 严禁绑定到公网 WAN 接口；监护人接口（`/api/v1/admin/*`）受 `guardian_pin`（默认 `1234`）保护 |

---

## 2. 部署前环境探测（Phase 0：Target Inspection）

Agent 通过 SSH 登录 iStoreOS（默认 `root@192.168.100.1`）后，**第一步必须执行以下只读探测命令**，确认 CPU 架构、LAN IP 和外接硬盘挂载路径：

```sh
# 1. 探测 CPU 架构 (aarch64 -> build-arm64; x86_64 -> build-amd64)
uname -m

# 2. 探测内存与 Swap 状态 (确认 Available > 200MB, Swap = 0)
free -m

# 3. 探测 iStoreOS LAN 口实际 IP (通常为 192.168.100.1)
uci get network.lan.ipaddr || ip -4 addr show br-lan

# 4. 探测外接硬盘挂载点与剩余空间 (确认 /mnt/sda1 或 /mnt/sdb1 已挂载且剩余空间 > 1024MB)
df -m | grep -E '/mnt/sd|/mnt/nvme|/mnt/mmc'
```

**判断准则**：
- 若 `uname -m` 输出 `aarch64`，后续编译选择 `arm64`；若输出 `x86_64`，后续编译选择 `amd64`。
- 若外接硬盘挂载点不是 `/mnt/sda1`（例如是 `/mnt/sdb1`），请将下文所有 `/mnt/sda1/reader` 同步替换为实际挂载点路径，并同步修改 `config.json` 与 `/etc/init.d/reader` 中的 `BASE` 变量。
- 若路由器 LAN IP 不是 `192.168.100.1`，请将 `config.json` 中的 `"listen": "192.168.100.1:8380"` 修改为 `"<实际_LAN_IP>:8380"`。

---

## 3. 宿主机编译与离线故事打包（Phase 1：Build on Host）

在拥有 Node.js、Go（支持 CGO 交叉编译）和 Python 3 的开发机/宿主机上，进入 `reader` 根目录执行以下步骤：

```bash
# Step 1.1: 运行端到端自动化验证套件，确保代码与 SQL 迁移逻辑 100% 通过
PYTHONPATH=tools python3 tools/verify_e2e.py

# Step 1.2: 构建前端单页应用 (Preact + Vite -> 输出至 server/internal/web/dist)
cd web && npm install && npm run build && cd ..

# Step 1.3: 根据 Phase 0 探测到的路由器架构，交叉编译静态单文件二进制
# 若 iStoreOS 为 ARM64 (aarch64，如 RK3568 / RK3588 / R2S / R4S / R5S 等):
make build-arm64
# 若 iStoreOS 为 x86_64 (软路由 N100 / N5105 / J4125 等):
make build-amd64

# Step 1.4: 生成首篇含多音字校验的样例故事包《小熊过河》
mkdir -p dist
python3 tools/story_packager.py --output dist/story_xiaoxiong_v1.zip
```

**编译产物清单**：
- `dist/reader-linux-arm64`（或 `dist/reader-linux-amd64`）：内嵌前端 SPA 与 SQL 迁移脚本的静态二进制文件。
- `dist/story_xiaoxiong_v1.zip`：离线故事包（含 `manifest.json`、SVG 插图与音频资源）。
- `deploy/config.json`：服务配置文件。
- `deploy/reader.init`：OpenWrt/iStoreOS `procd` 守护进程脚本。
- `deploy/99-reader-hotplug`：USB/SATA 外接硬盘热插拔自动启动脚本。

---

## 4. 外接硬盘目录初始化与文件上传（Phase 2：Disk Layout & Transfer）

### 4.1 在 iStoreOS 外接硬盘创建标准目录结构与挂载标记文件

在 iStoreOS 上执行（以 `/mnt/sda1/reader` 为例）：

```sh
export READER_BASE="/mnt/sda1/reader"

# 必须先确认 /mnt/sda1 是已挂载的独立分区，严禁在未挂载时写入根分区闪存！
mountpoint -q /mnt/sda1 || { echo "ERROR: /mnt/sda1 is NOT mounted! Aborting."; exit 1; }

# 创建目录规范与 .reader-disk 挂载校验标记文件
mkdir -p "$READER_BASE/data" \
         "$READER_BASE/media/stories" \
         "$READER_BASE/media/audio" \
         "$READER_BASE/tmp" \
         "$READER_BASE/backups"

touch "$READER_BASE/.reader-disk"
```

### 4.2 从宿主机上传编译产物与配置到 iStoreOS

在宿主机执行 `scp`（将 `ARCH_BIN` 替换为对应的 `reader-linux-arm64` 或 `reader-linux-amd64`，将 `192.168.100.1` 替换为实际 LAN IP）：

```bash
ROUTER_IP="192.168.100.1"
READER_BASE="/mnt/sda1/reader"
ARCH_BIN="dist/reader-linux-arm64" # 若为 x86_64 则改为 dist/reader-linux-amd64

# 1. 上传二进制与配置文件到外接硬盘
scp "$ARCH_BIN" "root@${ROUTER_IP}:${READER_BASE}/reader"
scp deploy/config.json "root@${ROUTER_IP}:${READER_BASE}/config.json"
scp dist/story_xiaoxiong_v1.zip "root@${ROUTER_IP}:${READER_BASE}/tmp/story_xiaoxiong_v1.zip"

# 2. 上传 procd 启动脚本与 hotplug 挂载自启脚本
scp deploy/reader.init "root@${ROUTER_IP}:/etc/init.d/reader"
scp deploy/99-reader-hotplug "root@${ROUTER_IP}:/etc/hotplug.d/block/99-reader"
```

---

## 5. 配置 Procd 守护进程、热插拔与系统升级保护（Phase 3：Service Registration）

在 iStoreOS 上通过 SSH 执行以下命令，完成权限赋予、固件升级保护与服务自启动注册：

```sh
export READER_BASE="/mnt/sda1/reader"

# 1. 赋予二进制与启动脚本可执行权限
chmod +x "$READER_BASE/reader"
chmod +x /etc/init.d/reader
chmod +x /etc/hotplug.d/block/99-reader

# 2. 将启动脚本与热插拔脚本写入 /etc/sysupgrade.conf，防止 iStoreOS 升级固件后丢失配置
grep -qxF '/etc/init.d/reader' /etc/sysupgrade.conf || echo '/etc/init.d/reader' >> /etc/sysupgrade.conf
grep -qxF '/etc/hotplug.d/block/99-reader' /etc/sysupgrade.conf || echo '/etc/hotplug.d/block/99-reader' >> /etc/sysupgrade.conf

# 3. 启用并启动 reader 服务
/etc/init.d/reader enable
/etc/init.d/reader start
```

### 附：一键无人值守部署脚本（`deploy/deploy_to_istoreos.sh`）

仓库 `deploy/deploy_to_istoreos.sh` 已内置上述完整流程，Agent 也可直接在宿主机运行：

```bash
ROUTER_IP=192.168.100.1 READER_BASE=/mnt/sda1/reader sh deploy/deploy_to_istoreos.sh
```

---

## 6. 部署后健康检查、内容导入与资源审计（Phase 4：Verification Gate）

Agent 在宣布部署完成前，**必须依次执行以下 4 项验证断言**，全部通过才算成功：

### 6.1 断言 1：检查服务进程状态与常驻内存（必须 < 80 MB）

```sh
# 在 iStoreOS 上检查进程与 RSS 内存占用 (单位 KB，应 < 80000 KB，通常在 15000~30000 KB)
ps -w | grep "[r]eader -config"
cat /proc/$(pgrep -f "reader -config")/status | grep -E "VmRSS|Threads"
```

### 6.2 断言 2：验证 `/api/v1/health` 与磁盘空间检查

```sh
curl -sS http://192.168.100.1:8380/api/v1/health
# 期望返回: {"disk_error":"","disk_space_ok":true,"free_disk_mb":...,"min_free_disk_mb":1024,"status":"ok"}
```

### 6.3 断言 3：导入离线故事包《小熊过河》并验证多音字接口

```sh
# 1. 使用监护人 PIN (默认 1234) 导入故事包
curl -sS -X POST http://192.168.100.1:8380/api/v1/admin/import \
  -H "X-Guardian-Pin: 1234" \
  -F "package=@/mnt/sda1/reader/tmp/story_xiaoxiong_v1.zip"
# 期望返回: {"imported_story_id":"story_xiaoxiong_guohe","imported_version_id":"ver_xiaoxiong_v1","sentence_count":5}

# 2. 验证故事列表与句子详情可读
curl -sS http://192.168.100.1:8380/api/v1/stories
curl -sS http://192.168.100.1:8380/api/v1/stories/story_xiaoxiong_guohe/versions/ver_xiaoxiong_v1 | head -c 400

# 3. 验证静态音频支持 HTTP Range (状态码必须为 206 Partial Content)
curl -I -sS -H "Range: bytes=0-15" http://192.168.100.1:8380/media/audio/ver_xiaoxiong_v1_s00_full.mp3 | head -n 5
```

### 6.4 断言 4：验证 SQLite WAL 模式已在外接硬盘生效

```sh
ls -la /mnt/sda1/reader/data/
# 期望看到 reader.db、reader.db-wal、reader.db-shm 均位于 /mnt/sda1/reader/data/ 下
```

---

## 7. 故障排查与回滚指南（Troubleshooting & Rollback）

1. **服务启动失败，系统日志提示 `disk not mounted`**
   - 执行 `logread -e reader` 查看日志。
   - 检查外接硬盘是否已挂载，以及 `/mnt/sda1/reader/.reader-disk` 空文件是否存在。
2. **导入故事包返回 `HTTP 507 Insufficient Storage`**
   - 外接硬盘剩余可用空间低于 `config.json` 中的 `min_free_disk_mb`（默认 `1024` MB），请清理外接硬盘或调整阈值。
3. **局域网平板/手机无法访问 `http://192.168.100.1:8380`**
   - 检查 `config.json` 中的 `listen` 地址是否与 `uci get network.lan.ipaddr` 一致。
   - 检查 iStoreOS 防火墙 LAN 入站规则（默认 LAN zone `input` 为 `ACCEPT`，无需额外开放 WAN 端口）。
4. **安全回滚与卸载**
   - 停止并禁用服务：`/etc/init.d/reader stop && /etc/init.d/reader disable`
   - 移除启动脚本：`rm -f /etc/init.d/reader /etc/hotplug.d/block/99-reader`
   - 外接硬盘中的 `/mnt/sda1/reader/data/reader.db` 及 `backups/` 完整保留，不会丢失任何儿童阅读记录。
