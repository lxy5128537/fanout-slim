package main

import (
	"syscall"

	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)
// 不再自带 SOCKS5 监听器，由 TunnelPool 统一管理入口。
type Tunnel struct {
	Slot    int       `json:"slot"`
	Node    Node      `json:"node"`
	Status  string    `json:"status"` // starting | up | failed | stopped
	ExitIP  string    `json:"exit_ip"`
	Err     string    `json:"err,omitempty"`
	Since   time.Time `json:"since"`
	workDir string

	ns          string
	ovpn        *exec.Cmd
	mismatchCnt int // 连续出口探测失配次数；>= 阈值才判定隧道不健康
	mu          sync.Mutex
}

func (t *Tunnel) nsName() string { return fmt.Sprintf("fo%d", t.Slot) }
func (t *Tunnel) subnet() string { return fmt.Sprintf("10.99.%d", t.Slot) }

// resetMismatch 探测命中(健康)时清零。
func (t *Tunnel) resetMismatch() {
	t.mu.Lock()
	t.mismatchCnt = 0
	t.mu.Unlock()
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runQuiet 执行清理类命令，忽略"本来就不存在"这类错误。
func runQuiet(name string, args ...string) {
	_ = exec.Command(name, args...).Run()
}

// setupNetns 建立 netns 与 veth 链路，并配好 NAT 与转发放行。
func (t *Tunnel) setupNetns() error {
	ns, sub := t.nsName(), t.subnet()
	veth, peer := fmt.Sprintf("fov%d", t.Slot), fmt.Sprintf("fop%d", t.Slot)

	t.teardownNetns()

	// 确保文件不存在，避免 ip netns add 的 O_EXCL 失败
	runQuiet("rm", "-f", fmt.Sprintf("/run/netns/%s", ns))

	// 确保 /run/netns 目录存在，防止 teardown 后目录丢失导致 ip netns add 失败
	if err := os.MkdirAll("/run/netns", 0755); err != nil {
		return fmt.Errorf("创建 /run/netns 失败: %w", err)
	}

	if err := run("ip", "netns", "add", ns); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up"); err != nil {
		return err
	}
	if err := run("ip", "link", "add", veth, "type", "veth", "peer", "name", peer); err != nil {
		return err
	}
	if err := run("ip", "link", "set", peer, "netns", ns); err != nil {
		return err
	}
	if err := run("ip", "addr", "add", sub+".1/30", "dev", veth); err != nil {
		return err
	}
	if err := run("ip", "link", "set", veth, "up"); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "addr", "add", sub+".2/30", "dev", peer); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "link", "set", peer, "up"); err != nil {
		return err
	}
	if err := run("ip", "netns", "exec", ns, "ip", "route", "add", "default", "via", sub+".1"); err != nil {
		return err
	}

	// netns 内的 DNS，仅用于 openvpn 解析远端主机名
	nsDir := filepath.Join("/etc/netns", ns)
	if err := os.MkdirAll(nsDir, 0755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", nsDir, err)
	}
	if err := os.WriteFile(filepath.Join(nsDir, "resolv.conf"), []byte("nameserver 114.114.114.114\nnameserver 8.8.8.8\nnameserver 1.1.1.1\n"), 0644); err != nil {
		return fmt.Errorf("写 resolv.conf 失败: %w", err)
	}

	cidr := sub + ".0/30"
	ensureRule("nat", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	ensureRuleInsert("filter", "FORWARD", "-s", cidr, "-j", "ACCEPT")
	ensureRuleInsert("filter", "FORWARD", "-d", cidr, "-j", "ACCEPT")
	return nil
}

// ensureRule 幂等追加一条 iptables 规则（末尾追加）。
func ensureRule(table, chain string, spec ...string) {
	check := append([]string{"-w", "5", "-t", table, "-C", chain}, spec...)
	if exec.Command("iptables", check...).Run() == nil {
		return
	}
	add := append([]string{"-w", "5", "-t", table, "-A", chain}, spec...)
	runQuiet("iptables", add...)
}

// ensureRuleInsert 幂等插入规则到链首（NAT 用 -A，FORWARD 用 -I）。
func ensureRuleInsert(table, chain string, spec ...string) {
	check := append([]string{"-w", "5", "-t", table, "-C", chain}, spec...)
	if exec.Command("iptables", check...).Run() == nil {
		return
	}
	ins := append([]string{"-w", "5", "-t", table, "-I", chain, "1"}, spec...)
	runQuiet("iptables", ins...)
}

// deleteRule 幂等删除一条 iptables 规则（先检查再删）。
// 与 ensureRule/ensureRuleInsert 完全匹配。
func deleteRule(table, chain string, spec ...string) {
	check := append([]string{"-w", "5", "-t", table, "-C", chain}, spec...)
	if exec.Command("iptables", check...).Run() != nil {
		return // 规则不存在，无需删除
	}
	del := append([]string{"-w", "5", "-t", table, "-D", chain}, spec...)
	runQuiet("iptables", del...)
}

func (t *Tunnel) teardownNetns() {
	ns, sub := t.nsName(), t.subnet()
	cidr := sub + ".0/30"
	// 先杀 openvpn（如果还在跑），等待退出，再删 netns
	if t.ovpn != nil && t.ovpn.Process != nil {
		_ = t.ovpn.Process.Kill()
		// 给 openvpn 一点时间退出，否则 netns 可能删不掉
		waitCh := make(chan struct{})
		go func() {
			t.ovpn.Wait()
			close(waitCh)
		}()
		select {
		case <-waitCh:
		case <-time.After(3 * time.Second):
			log.Printf("openvpn 在 netns %s 中未及时退出，发 SIGKILL", ns)
			_ = t.ovpn.Process.Kill() // 再次 kill（SIGKILL）
			// 再等 2 秒
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
				log.Printf("openvpn 在 netns %s 中仍未退出，强制继续", ns)
			}
		}
		t.ovpn = nil
	}
	// 强制删除 netns：先杀里面所有进程，再直接 umount + unlink
	nsPath := fmt.Sprintf("/run/netns/%s", ns)
	for i := 0; i < 5; i++ {
		// 杀 namespace 内所有进程
		out, _ := exec.Command("ip", "netns", "pids", ns).Output()
		pids := strings.Fields(string(out))
		if len(pids) > 0 {
			log.Printf("teardown %s: ip netns pids 找到 %v", ns, pids)
		}
		for _, pid := range pids {
			runQuiet("kill", "-9", pid)
		}
		// 直接 umount + unlink，绕过 ip netns del 的问题
		if err := syscall.Unmount(nsPath, syscall.MNT_DETACH); err != nil {
			log.Printf("teardown %s: umount 失败: %v", ns, err)
		}
		if err := os.Remove(nsPath); err != nil && !os.IsNotExist(err) {
			log.Printf("teardown %s: rm 失败: %v", ns, err)
		}
		// 检查是否已清理
		if _, err := os.Stat(nsPath); os.IsNotExist(err) {
			log.Printf("teardown %s: ok", ns)
			break
		}
		time.Sleep(time.Second)
	}
	// 清理 veth
	runQuiet("ip", "link", "del", fmt.Sprintf("fov%d", t.Slot))
	deleteRule("nat", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
	deleteRule("filter", "FORWARD", "-s", cidr, "-j", "ACCEPT")
	deleteRule("filter", "FORWARD", "-d", cidr, "-j", "ACCEPT")
}

// startOpenVPN 在 netns 内拉起 openvpn，并等待 tun0 拿到地址。
func (t *Tunnel) startOpenVPN() error {
	ns := t.nsName()
	cfgPath := filepath.Join(t.workDir, ns+".ovpn")
	if err := os.WriteFile(cfgPath, []byte(t.Node.Config), 0600); err != nil {
		return fmt.Errorf("写配置失败: %w", err)
	}
	authPath := filepath.Join(t.workDir, "auth.txt")
	if err := os.WriteFile(authPath, []byte("vpn\nvpn\n"), 0600); err != nil {
		return fmt.Errorf("写凭据失败: %w", err)
	}

	logPath := filepath.Join(t.workDir, ns+".log")
	cmd := exec.Command("ip", "netns", "exec", ns, "openvpn",
		"--config", cfgPath,
		"--auth-user-pass", authPath,
		"--auth-nocache",
		"--dev", "tun0",
		"--connect-retry-max", "3",
		"--connect-timeout", "10",
		"--data-ciphers", "AES-128-CBC:AES-256-GCM:AES-128-GCM:CHACHA20-POLY1305",
		"--verb", "3",
		"--log", logPath,
	)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 openvpn 失败: %w", err)
	}
	t.ovpn = cmd
	go cmd.Wait() // 回收子进程，避免僵尸

	// openvpn 建好 tun0 前无法出网，这里等它就绪（10 秒超时，节点不行就换下一个）
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if out, err := exec.Command("ip", "netns", "exec", ns, "ip", "-4", "addr", "show", "tun0").Output(); err == nil {
			if strings.Contains(string(out), "inet ") {
				return nil
			}
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return fmt.Errorf("openvpn 提前退出，详见 %s", logPath)
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("等待 tun0 就绪超时 (10s)，详见 %s", logPath)
}

// probeExitIP 通过隧道查询出口 IP，用于确认这条隧道确实换了 IP。
// 依次尝试多个出口 IP 检测源，覆盖国内/海外环境。
func (t *Tunnel) probeExitIP() (string, error) {
	// 等待几秒让隧道稳定，防止路由推送延迟
	time.Sleep(3 * time.Second)
	ip, err := tryIPCheckURLs(t.nsName(), probeIPURLs)
	if err != nil {
		return "", fmt.Errorf("查询出口 IP 失败: %w", err)
	}
	return ip, nil
}

// probeIPURLs 出口 IP 检测源列表，按优先级排列。
// 国内/海外都至少有一个可达。
var probeIPURLs = []string{
	"http://ip.sb",
	"http://api.ipify.org",
	"http://ifconfig.me",
	"http://ip.typotip.com",
	"http://myip.ipip.net",
	"http://httpbin.org/ip",
}

// tryIPCheckURLs 依次尝试多个出口 IP 检测 URL，返回第一个解析到的合法 IP。
func tryIPCheckURLs(ns string, urls []string) (string, error) {
	var lastErr error
	for _, u := range urls {
		out, err := exec.Command("ip", "netns", "exec", ns,
			"curl", "-s", "--max-time", "10", u).Output()
		if err != nil {
			lastErr = err
			continue
		}
		ip := strings.TrimSpace(string(out))
		// ipip.net 返回 "你的 IP: xxx.xxx.xxx.xxx 来自: xxx" 这种格式
		for _, tok := range strings.Fields(ip) {
			if net.ParseIP(tok) != nil {
				return tok, nil
			}
		}
		// 纯 IP 响应
		if net.ParseIP(ip) != nil {
			return ip, nil
		}
		lastErr = fmt.Errorf("%s 返回: %q", u, ip)
	}
	return "", fmt.Errorf("所有出口 IP 检测源均失败，最后一个: %w", lastErr)
}

// healthMismatchThreshold 连续出口探测失配阈值：只有连续多次不一致才判定
// 隧道不健康，单次探测抖动/被墙不会触发切换，避免出口 IP 频繁跳变(风控友好)。
const healthMismatchThreshold = 3

// healthIPURLs 健康检查用的出口 IP 检测源（短超时，多源兜底）。
var healthIPURLs = []string{
	"http://ip.sb",
	"http://api.ipify.org",
	"http://ifconfig.me",
	"http://ip.typotip.com",
	"http://myip.ipip.net",
}

// cleanupConfig 清理 .ovpn 配置文件。
func (t *Tunnel) cleanupConfig() {
	os.Remove(filepath.Join(t.workDir, t.nsName()+".ovpn"))
}

// stop 停止这条隧道并清理它占用的所有资源。
func (t *Tunnel) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ovpn != nil && t.ovpn.Process != nil {
		_ = t.ovpn.Process.Kill()
		t.ovpn = nil
	}
	t.teardownNetns()
	t.cleanupConfig()
	t.Status = "stopped"
}