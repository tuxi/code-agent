# 在 macOS 恢复模式运行 codeagent（诊断模式）

Mac 进不了正常系统时（启动失败、磁盘异常、内核恐慌排查），在恢复模式终端里运行
codeagent TUI，让 agent 帮你分析问题：跑 `diskutil` / `log show` / `fsck`、读挂载卷上的
日志与崩溃报告、给出修复命令。白名单外的命令（如 `diskutil`）默认弹审批卡，每一步可控。

## 环境事实（本方案依赖的约束）

- recoveryOS 是精简版 macOS：终端以 root 运行 bash，根卷只读；`/usr/bin` 自带
  `curl`、`codesign`、`hdiutil`，`/usr/sbin` 有 `diskutil`，Unified log 的 `log`
  命令可用，APFS/HFS+/exFAT 的 fsck 套件齐全
  （参考：eclecticlight.co "Making a good Recovery: So many tools"）
- Apple Silicon 恢复模式无 Rosetta，二进制必须原生架构 → 发布 universal binary
- arm64 要求有效签名（否则 `Killed: 9`），Go 产物自带 ad-hoc 签名；`curl` 下载
  不产生 quarantine xattr，无需 `codesign` 处理
- 恢复模式环境每次重启清零：下载的二进制与会话不持久，属预期行为

## 发布（维护者）

```sh
scripts/release-codeagent.sh v1.6.4
```

构建 universal binary（`CGO_ENABLED=0`，arm64 + x86_64，`lipo` 合并）→ 烟测
（`sessions` 只读入口，不需要 TTY/model/key）→ `gh release create` 到
`tuxi/code-agent`，同时上传 `codeagent.sha256`。

稳定下载链接：

```
https://github.com/tuxi/code-agent/releases/latest/download/codeagent
```

注意：不要把 CLI 发到 `tuxi/code-agent-releases`——该仓库的
`releases/latest/download/codeagentd` 被 Talkify Xcode Build Phase 消费，
CLI-only release 会成为 "latest" 导致那条链路 404。

## 恢复模式运行手册

### 0. 提前准备（可选，在正常系统里做一次）

把 API key 写到数据卷上，恢复模式里 source 即可，免去手敲长字符串：

```sh
echo 'export DEEPSEEK_API_KEY=sk-...' > "/Volumes/Macintosh HD - Data/codeagent-env"
chmod 600 "/Volumes/Macintosh HD - Data/codeagent-env"
```

同时建议拷一份 TLS 根证书作兜底（见故障排查）：

```sh
cp /etc/ssl/cert.pem "/Volumes/Macintosh HD - Data/etc-ssl-cert.pem"
```

### 1. 进入恢复模式并联网

- Apple Silicon：长按电源键直到出现"正在载入启动选项"→ 选择选项 → 继续
- Intel：开机按 Cmd+R
- 菜单栏 Wi-Fi 图标连接网络（需要能直连外网； captive portal 网络不可用）

### 2. 校时

```sh
date -u
# 若偏差大（TLS 握手依赖时钟，下载和 API 都会失败）：
date -u 202609021200    # 格式 YYYYMMDDhhmm
```

### 3. 挂载数据卷并进入

```sh
diskutil list
# FileVault 开启时先解锁（会提示输入密码）：
diskutil apfs unlockVolume <数据卷VolumeID>
diskutil mount <数据卷VolumeID>
cd "/Volumes/Macintosh HD - Data"     # 卷名以 diskutil list 实际输出为准
```

诊断主要看数据卷：`private/var/log`、`Library/Logs/DiagnosticReports`、
用户 LaunchAgents、内核扩展等。系统卷快照是只读的。

### 4. 下载并运行

```sh
curl -LO https://github.com/tuxi/code-agent/releases/latest/download/codeagent
chmod +x codeagent
source codeagent-env 2>/dev/null || export DEEPSEEK_API_KEY=sk-...
HOME="$PWD/home" ./codeagent --trust
```

两处不可省略：

- `HOME="$PWD/home"`：TUI 启动时要在 `$HOME/.codeagent/` 下创建 session store，
  失败直接退出；恢复模式的 `/var/root` 可写性不可靠，重定向到已挂载的可写卷
- `--trust`：workspace 信任门放行（恢复模式数据卷不属于任何已信任目录）

校验（可选）：

```sh
shasum -a 256 -c codeagent.sha256
```

### 5. 使用

- 直接输入诊断问题。`diskutil`、`log show` 等白名单外命令会弹审批卡，确认执行
- `--auto` 启动可减少审批打扰（工作区内编辑/命令自动放行，网络操作仍确认）
- 需要留档时让 agent 把结论写到数据卷上的文件——恢复模式重启后会话即失

## 降级路径

| 场景 | 命令 |
|---|---|
| TUI 渲染异常 | `HOME="$PWD/home" ./codeagent --trust repl` |
| 单轮任务 | `HOME="$PWD/home" ./codeagent --trust run "诊断..."` |
| 一次性问答（无工具） | `HOME="$PWD/home" ./codeagent --trust ask "..."` |

## 故障排查

| 症状 | 原因与处理 |
|---|---|
| `Killed: 9` | 签名问题：`codesign -s - --force codeagent`（recovery 自带 codesign） |
| `x509: certificate signed by unknown authority` | TLS 根证书缺失：`export SSL_CERT_FILE="$PWD/etc-ssl-cert.pem"` 后重跑 |
| `certificate expired` / 下载失败 | 时钟偏差，回到第 2 步 |
| `create session store dir` 失败 | `HOME` 未指向可写目录 |
| `curl: (56) Failure writing output to destination, passed N returned M` | 下载落在了 recoveryOS 根卷（仅几十 MB 可写），写到 92% 左右短写失败：先挂数据卷/外置盘并 `cd` 进去再下载；`df -h .` 可验证当前目录所在卷大小 |
| `failed to mount because it appears to be an APFS Physical Store` | mount 的是物理存储分区而非卷：APFS 卷在 `(synthesized)` 合成条目下（内部磁盘的容器一般是编号最大的 synthesized disk），挂 `APFS Volume Data` 那一行对应的 identifier |
| TUI 显示错乱 | 用 repl 降级；恢复模式终端对 alt-screen 支持有限 |
| 外置 U 盘挂载失败 | 换 FAT32/APFS 格式（exFAT 驱动在 recoveryOS 中不保证） |
| agent 提示找不到命令 | 预期：recoveryOS 没有 git/go/rg，写代码类工具不可用 |

## 限制

- 这是诊断模式不是开发模式：无 git、无 go、无 LSP，代码级工具全部不可用
- 会话与下载的二进制重启即失；结论需显式落盘到数据卷
- SIP/系统卷快照保护的路径仍不可写，agent 只能做只读分析与数据卷内修复
- API key 会出现在恢复模式终端的 history 中，环境随重启销毁；不要把 key
  提交进仓库或上传到发布资产
