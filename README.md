# fout

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

把 VPN Gate 公共节点变成本地 SOCKS5 代理，自动故障切换，双隧道高可用。

fout 是 [fanout](https://github.com/byJoey/fanout) 的轻量精简版：
- **去掉 Web 管理界面**、Xray/3x-ui 集成、多出口管理
- **保留核心**：网络命名空间隔离、双隧道自动故障切换、SOCKS5 入口
- 加上**完善的清理机制**：退出时自动清理 netns 和 iptables 残留

## 原理

```
客户端 ──> SOCKS5 :端口 ──> setns 切进 netns ──> openvpn ──> VPN Gate 节点
```

每个隧道跑在独立的 network namespace 里：
- netns 内启动官方 openvpn 客户端，连接 VPN Gate 公共节点
- VPN 路由劫持只影响自己的 netns，不切断宿主机网络
- SOCKS5 连接通过 `setns` 系统调用切入对应 netns 建立出站连接
- 域名解析在隧道 netns 内用 `8.8.8.8` 完成，避免 DNS 泄漏

## 架构

```
┌─────────────────────────────────────────────────────┐
│  TunnelPool                                         │
│  ┌──────────────────────────────────────────────┐   │
│  │  主隧道 (active)   │   备用隧道 (standby)    │   │
│  │  ┌──────────────┐  │  ┌──────────────┐      │   │
│  │  │ netns: fo1   │  │  │ netns: fo2   │      │   │
│  │  │ openvpn → JP │  │  │ openvpn → JP │      │   │
│  │  │ ExitIP: x.x  │  │  │ ExitIP: y.y  │      │   │
│  │  └──────────────┘  │  └──────────────┘      │   │
│  └──────────────────────────────────────────────┘   │
│                                                      │
│  ┌──────────────────────────────────────────────┐   │
│  │  SOCKS5 :端口 (默认 10000, 可 -p 指定)        │   │
│  │  → 通过 GetActiveDialer() 获取主隧道拨号器    │   │
│  │  → setns 切到 active netns 建立出站连接        │   │
│  └──────────────────────────────────────────────┘   │
│                                                      │
│  ┌──────────────────────────────────────────────┐   │
│  │  WatchHealth (每 5 秒)                        │   │
│  │  主隧道故障 → 切换到备用隧道 → 刷新原主隧道    │   │
│  │  备用隧道故障 → 刷新备用隧道                  │   │
│  └──────────────────────────────────────────────┘   │
│                                                      │
│  ┌──────────────────────────────────────────────┐   │
│  │  (非 daemon 模式) 父进程监控 (每 1 秒)        │   │
│  │  PR_SET_PDEATHSIG + os.FindProcess 探活       │   │
│  │  父进程退出 → cleanupStale() → os.Exit(0)    │   │
│  └──────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────┘
```

## 文件结构

```
├── main.go        # 入口：参数解析、信号处理、daemon 模式
├── pool.go        # TunnelPool：主/备用隧道、节点选取、故障切换、shutdownCh
├── tunnel.go      # Tunnel：netns 创建销毁、openvpn 启停、出口 IP 探测
├── netns.go       # 网络命名空间切换：setns 拨号器、DNS 解析
├── socks5.go      # SOCKS5 协议：无认证 CONNECT 仅 TCP，端口可配
├── health.go      # 健康检查与故障恢复（recovering 状态防重复 refresh）
├── vpngate.go     # VPN Gate 节点拉取与解析（CSV + JSON 镜像）
├── check.go       # 环境检查、清理残留、依赖检测
└── README.md      # 本文件
```

## 安装

### 依赖

- Linux（需要 `netns` 支持，内核需开启 `CONFIG_NET_NS`）
- root 权限（创建 netns 和修改 iptables）
- 必需命令：`ip`、`iptables`、`openvpn`、`curl`、`sysctl`
- `/dev/net/tun` 设备（LXC 容器可能缺失）

### 编译

```bash
git clone https://github.com/byJoey/fanout.git
cd fanout
GOARCH=amd64 go build -ldflags "-X main.version=Ver1.0.0" -o fout .
```

### 运行

```bash
# 前台运行（默认启用父进程监控）
sudo ./fout

# 后台运行
nohup sudo ./fout > /var/log/fanout.log 2>&1 &
```

## 参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-p` | `10000` | SOCKS5 监听端口 |
| `-c` | `""` | VPN 节点国家代码（如 `JP` / `KR` / `US`），空字符串则不限国家 |
| `-d` | `false` | Daemon 模式：不监控父进程，避免开机脚本或 SSH 断开时误退出 |

### 用法示例

```bash
# 只跑日本节点，SOCKS5 监听 8000，daemon 模式
sudo ./fout -c JP -p 8000 -d

# 不加 -d 时，父进程退出（如 sudo 结束）会触发清理
sudo ./fout

# 不加 -c 时使用全部国家节点
sudo ./fout -d
```

### 开机自启（OpenRC local.d）

```bash
# /etc/local.d/fanout.start
#!/bin/sh
sleep 15
setsid nohup /usr/local/bin/fout -d -c JP >> /var/log/fanout.log 2>&1 < /dev/null &
```

> **重要**：开机自启场景**必须加 `-d`**。否则父进程是 init 派生的 shell，SSH 断开时 shell 退出会误触发 `PR_SET_PDEATHSIG`，fout 被连带杀掉。

## 使用

启动后 SOCKS5 代理监听在 `0.0.0.0:<端口>`，无认证：

```bash
# 通过隧道访问（默认端口 10000）
curl -x socks5://127.0.0.1:10000 http://ip.sb

# 浏览器配置 SOCKS5 代理为 127.0.0.1:<端口>
```

## 清理机制

fout 有**三层清理保障**，确保不会留下 netns 或 iptables 残留：

### 1. 启动时清理（`cleanupStale()`）
程序启动时立即清理上次残留的 netns 和 iptables 规则，防止崩溃残留。

### 2. 非 daemon 模式的父进程监控
仅 `-d` 未启用时有效：
- `PR_SET_PDEATHSIG`：父进程退出时收到 SIGTERM，触发清理
- 独立 goroutine **每秒**用 `os.FindProcess(ppid).Signal(0)` 探活
- 父进程退出（包括 `kill -9`）→ `cleanupStale()` → `os.Exit(0)`

> **daemon 模式（`-d`）下上述两条都禁用**，进程不再受父进程生命周期影响。

### 3. 信号处理（SIGINT/SIGTERM/SIGHUP）
收到信号时：
- 打印信号名称日志
- 调用 `Shutdown()` 清理双隧道
- `close(shutdownCh)` 通知所有后台 goroutine 退出
- `cleanupStale()` 兜底清理残留
- `os.Exit(0)`

> **SIGPIPE 不监听**——openvpn/curl 管道断开会误触发 Shutdown，Go 运行时默认忽略 SIGPIPE。

### 4. 每节点故障清理（`teardownNetns()`）
每次隧道节点切换或故障时，彻底清理：
- 杀掉 openvpn 子进程
- 卸载 netns 文件系统挂载（`syscall.Unmount`）
- 删除 netns 绑定文件（`os.Remove`）
- 通过 `pgrep` 回退查找残留 openvpn 进程
- 删除 iptables MASQUERADE 和 FORWARD 规则

## 隧道故障切换

```
主隧道故障
  ├─ 备用隧道可用 → 立即切换 → 刷新原主隧道为新备用
  └─ 备用隧道不可用 → 刷新主隧道

备用隧道故障
  └─ 刷新备用隧道

备用隧道不存在
  └─ 每 30 秒尝试创建一条（异步，不影响主隧道）
```

节点选取策略：
1. 首选同地区节点（和故障节点同一国家）
2. 同地区尝试 **3 个**节点后，不限地区轮换（`candidatesFor`）
3. 每个节点按连接数降序排列，优先 443 端口
4. 通过 `-c` 参数指定国家时，只从该国家节点中选取

## 健康检查

每 5 秒检查一次隧道健康：
- **`up` 状态**：通过 curl 比对出口 IP（`http://ip.sb`），确保隧道确实在 VPN 上
- **`starting` / `recovering` 状态**：给予 3 分钟宽限，不视为故障
- 连续两次不符 → 自动换节点重连

> **防重复刷新**：`refreshFailedTunnel` 调 `stop()` 后重新设置 `Status="recovering"` + 重置 `Since`，防止 `checkTunnels` 在重建期间再次判定故障并重复触发 refresh。

## 性能参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-p` | `10000` | SOCKS5 监听端口 |
| `-c` | `""` | VPN 节点国家代码 |
| `-d` | `false` | Daemon 模式 |
| 工作目录 | `/var/lib/fanout/` | 日志、配置存储 |
| 健康检查间隔 | 5 秒 | 隧道健康检查周期 |
| 备用隧道重试间隔 | 30 秒 | 创建备用隧道失败后的重试间隔 |
| 单节点连接超时 | 10 秒 | `openvpn --connect-timeout` |
| 同节点重试次数 | 3 | `openvpn --connect-retry-max` |
| tun0 就绪等待 | 15 秒 | openvpn 建立后等 tun0 拿到 IP |
| 候选节点数量 | 3 | `candidatesFor` 每次最多尝试的节点数 |
| 节点列表刷新超时 | 60 秒 | VPN Gate API 超时 |

## 已知限制

- **只转发 TCP**。SOCKS5 收到域名时通过隧道内 `8.8.8.8` 解析，隧道内不跑 UDP/DNS
- **VPN Gate 节点可靠性**。VPN Gate 是筑波大学的学术实验项目，有相当比例节点已下线或满员。启动时连不上会自动顺着候选节点往下试
- **国内访问受限**。部分中国网络环境无法直连 VPN Gate 节点（219.100.37.x 被 GFW 阻断），需要在海外中转服务器上运行

## 许可

[MIT](LICENSE)。

节点来自 [VPN Gate](https://www.vpngate.net/)（筑波大学的学术实验项目），本工具只是调用其公开的节点列表并用官方 openvpn 客户端连接，不修改也不代理其服务。使用时请遵守 VPN Gate 的条款和你所在地的法律。