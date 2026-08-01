# fanout-slim

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

把 VPN Gate 公共节点变成本地 SOCKS5 代理，自动故障切换，双隧道高可用。

fanout-slim 是 [fanout](https://github.com/byJoey/fanout) 的轻量精简版：
- **去掉 Web 管理界面**、Xray/3x-ui 集成、多出口管理
- **保留核心**：网络命名空间隔离、双隧道自动故障切换、SOCKS5 入口
- 加上**完善的清理机制**：父进程退出自动清理 netns 和 iptables 残留

## 原理

```
客户端 ──> SOCKS5 :10000 ──> setns 切进 netns ──> openvpn ──> VPN Gate 节点
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
│  │  │ openvpn → JP │  │  │ openvpn → US │      │   │
│  │  │ ExitIP: x.x  │  │  │ ExitIP: y.y  │      │   │
│  │  └──────────────┘  │  └──────────────┘      │   │
│  └──────────────────────────────────────────────┘   │
│                                                      │
│  ┌──────────────────────────────────────────────┐   │
│  │  SOCKS5 :10000                                │   │
│  │  → 通过 GetActiveDialer() 获取主隧道拨号器    │   │
│  │  → setns 切到 fo1 建立出站连接                │   │
│  └──────────────────────────────────────────────┘   │
│                                                      │
│  ┌──────────────────────────────────────────────┐   │
│  │  WatchHealth (每 5 秒)                        │   │
│  │  主隧道故障 → 切换到备用隧道 → 刷新主隧道    │   │
│  │  备用隧道故障 → 刷新备用隧道                  │   │
│  └──────────────────────────────────────────────┘   │
│                                                      │
│  ┌──────────────────────────────────────────────┐   │
│  │  父进程监控 (每 1 秒)                          │   │
│  │  os.FindProcess(ppid).Signal(0) 探活          │   │
│  │  父进程死亡 → cleanupStale() → os.Exit(0)    │   │
│  └──────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────┘
```

## 文件结构

```
├── main.go        # 入口：启动、父进程监控、信号处理
├── pool.go        # TunnelPool 管理：主/备用隧道、节点选取、故障切换
├── tunnel.go      # Tunnel 实现：netns 创建销毁、openvpn 启停、出口 IP 探测
├── netns.go       # 网络命名空间切换：setns 拨号器、DNS 解析
├── socks5.go      # SOCKS5 协议实现：无认证 CONNECT 仅 TCP
├── health.go      # 健康检查与故障恢复
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
GOARCH=amd64 go build -ldflags "-X main.version=slim" -o fanout-slim-amd64 .
```

### 运行

```bash
# 直接运行（前台）
sudo ./fanout-slim-amd64

# 或后台运行
nohup sudo ./fanout-slim-amd64 > /var/log/fanout.log 2>&1 &
```

## 使用

启动后 SOCKS5 代理监听在 `0.0.0.0:10000`，无认证：

```bash
# 通过隧道访问
curl -x socks5://127.0.0.1:10000 http://ip.sb

# 浏览器配置
# 设置 SOCKS5 代理为 127.0.0.1:10000
```

## 清理机制

fanout-slim 有**三层清理保障**，确保不会留下 netns 或 iptables 残留：

### 1. 启动时清理（`cleanupStale()`）
程序启动时立即清理上次残留的 netns 和 iptables 规则，防止崩溃残留。

### 2. 运行时监控（`os.FindProcess` 探活）
独立 goroutine **每秒**检查父进程是否存活：
- 父进程退出（包括 `sudo kill -9` 强杀）→ 自动调用 `cleanupStale()` → `os.Exit(0)`
- 比传统 `PR_SET_PDEATHSIG` 更可靠，可处理 `SIGKILL` 等不可捕获信号

### 3. 每节点故障清理（`teardownNetns()`）
每次隧道节点切换或故障时，彻底清理：
- 杀掉 openvpn 子进程
- 卸载 netns 文件系统挂载（`syscall.Unmount`）
- 删除 netns 绑定文件（`os.Remove`）
- 通过 `pgrep` 回退查找残留 openvpn 进程
- 删除 iptables MASQUERADE 和 FORWARD 规则

## 隧道故障切换

```
主隧道故障
  ├─ 备用隧道可用 → 立即切换 → 刷新原主隧道
  └─ 备用隧道不可用 → 刷新主隧道

备用隧道故障
  └─ 刷新备用隧道

备用隧道不存在
  └─ 每 30 秒尝试创建一条
```

节点选取策略：
1. 首选同地区节点（和故障节点同一国家）
2. 同地区尝试 6 个节点后，不限地区轮换
3. 每个节点按速度降序排列，优先 443 端口

## 健康检查

每 5 秒检查一次隧道健康：
- **`up` 状态**：通过 curl 比对出口 IP（`http://ip.sb`），确保隧道确实在 VPN 上
- **`starting`/`recovering` 状态**：给予 3 分钟宽限，不视为故障
- 连续两次不符 → 自动换节点重连

## 配置

| 参数 | 说明 | 默认值 |
|------|------|--------|
| SOCKS5 端口 | 固定监听端口 | `10000` |
| 工作目录 | 日志、配置存储 | `/var/lib/fanout/` |
| 健康检查间隔 | 隧道健康检查周期 | 5 秒 |
| 备用隧道重试间隔 | 创建备用隧道失败后的重试间隔 | 30 秒 |
| 路由探测超时 | 出口 IP 检测超时 | 30 秒 |
| 隧道建立超时 | openvpn 连接 + tun0 等待 | 30 秒 |
| 节点列表刷新 | 手动刷新节点列表 | 60 秒 |

## 已知限制

- **只转发 TCP**。SOCKS5 收到域名时通过隧道内 `8.8.8.8` 解析，隧道内不跑 UDP/DNS
- **VPN Gate 节点可靠性**。VPN Gate 是筑波大学的学术实验项目，有相当比例节点已下线或满员。启动时连不上会自动顺着候选节点往下试
- **国内访问受限**。部分中国网络环境无法直连 VPN Gate 节点（219.100.37.x 被 GFW 阻断），需要在海外中转服务器上运行

## 许可

[MIT](LICENSE)。

节点来自 [VPN Gate](https://www.vpngate.net/)（筑波大学的学术实验项目），本工具只是调用其公开的节点列表并用官方 openvpn 客户端连接，不修改也不代理其服务。使用时请遵守 VPN Gate 的条款和你所在地的法律。