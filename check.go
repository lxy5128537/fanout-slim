package main

import (
	"syscall"

	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// prepareHost 检查运行环境并开启网络转发。
// 在初始化隧道之前调用，确保所有依赖满足。
func prepareHost() error {
	// 0. 清理上次残留的 netns 和 iptables 规则
	cleanupStale()
	// 0. 确保 PATH 包含 sbin 目录（root 用户也可能没有）
	os.Setenv("PATH", os.Getenv("PATH")+":/sbin:/usr/sbin:/usr/local/sbin")

	// 1. 检查 /dev/net/tun
	if err := checkTunDevice(); err != nil {
		return err
	}

	// 3. 检查必需命令
	required := []struct {
		name   string
		cmd    string
		hint   string
		reason string
	}{
		{"ip", "ip", "apt install iproute2 / yum install iproute",
			"创建和管理网络命名空间（netns）、veth 虚拟网卡"},
		{"iptables", "iptables", "apt install iptables / yum install iptables",
			"配置 NAT 和 FORWARD 规则，让 netns 内的流量能出网"},
		{"openvpn", "openvpn", "apt install openvpn / yum install openvpn",
			"连接 VPN Gate 公共节点，建立隧道"},
		{"curl", "curl", "apt install curl / yum install curl",
			"检测出口 IP（api.ipify.org）和健康检查"},
		{"sysctl", "sysctl", "apt install procps / yum install procps-ng",
			"开启 net.ipv4.ip_forward 转发"},
	}

	for _, r := range required {
		if err := checkCommand(r.cmd); err != nil {
			return fmt.Errorf("缺少必需依赖: %s (%s)\n  → 安装: %s\n  → 用途: %s",
				r.name, r.cmd, r.hint, r.reason)
		}
	}

	// 4. 开启 ip_forward
	if err := exec.Command("sysctl", "-qw", "net.ipv4.ip_forward=1").Run(); err != nil {
		return fmt.Errorf("开启 ip_forward 失败: %w\n  → 检查容器是否允许修改内核参数（--privileged 或 --cap-add=NET_ADMIN,SYSCTL）", err)
	}

	// 5. 检查 iptables 是否可用（有些容器用 nftables 替代）
	if err := exec.Command("iptables", "-w", "3", "-L", "-n").Run(); err != nil {
		return fmt.Errorf("iptables 不可用: %w\n  → 检查是否被 nftables 替代，或需要 --privileged 权限", err)
	}

	// 6. 检查 netns 支持
	if err := exec.Command("ip", "netns", "list").Run(); err != nil {
		return fmt.Errorf("网络命名空间不可用: %w\n  → 内核缺少 CONFIG_NET_NS 支持", err)
	}

	log.Printf("环境检查通过: 所有依赖均已就绪")
	return nil
}

// cleanupStale 清理残留的 netns 和 iptables 规则。
// 如果上次运行被异常终止（SIGKILL、SSH 超时等），
// cleanupStale 清理残留的 netns 和 iptables 规则。
func cleanupStale() {
	for _, ns := range []string{"fo1", "fo2", "fo3", "fo4", "fo5"} {
		nsPath := fmt.Sprintf("/run/netns/%s", ns)
		// 检查文件是否存在，不存在则跳过
		if _, err := os.Stat(nsPath); os.IsNotExist(err) {
			continue
		}
		log.Printf("cleanupStale: 发现 %s, 开始清理", ns)
		// 杀 namespace 内所有进程
		out, _ := exec.Command("ip", "netns", "pids", ns).Output()
		pids := strings.Fields(string(out))
		if len(pids) > 0 {
			log.Printf("cleanupStale: ip netns pids %s 找到 %v", ns, pids)
		}
		for _, pid := range pids {
			runQuiet("kill", "-9", pid)
		}
		// 如果 ip netns pids 没找到，通过进程名找到 openvpn 进程
		if len(pids) == 0 {
			pidOut, _ := exec.Command("pgrep", "-f", fmt.Sprintf("openvpn.*%s", ns)).Output()
			otherPids := strings.Fields(string(pidOut))
			if len(otherPids) > 0 {
				log.Printf("cleanupStale: pgrep %s 找到 %v", ns, otherPids)
			}
			for _, pid := range otherPids {
				runQuiet("kill", "-9", pid)
			}
		}
		// 直接 umount + unlink
		if err := syscall.Unmount(nsPath, syscall.MNT_DETACH); err != nil {
			log.Printf("cleanupStale: umount %s 失败: %v", ns, err)
		} else {
			log.Printf("cleanupStale: umount %s 成功", ns)
		}
		if err := os.Remove(nsPath); err != nil && !os.IsNotExist(err) {
			log.Printf("cleanupStale: rm %s 失败: %v", ns, err)
		} else {
			log.Printf("cleanupStale: rm %s 成功", ns)
		}
	}
	for _, cidr := range []string{"10.99.1.0/30", "10.99.2.0/30", "10.99.3.0/30", "10.99.4.0/30"} {
		deleteRule("nat", "POSTROUTING", "-s", cidr, "-j", "MASQUERADE")
		deleteRule("filter", "FORWARD", "-s", cidr, "-j", "ACCEPT")
		deleteRule("filter", "FORWARD", "-d", cidr, "-j", "ACCEPT")
	}
	runQuiet("rm", "-rf", "/etc/netns")
	// 恢复 ip_forward 为 0（关闭转发）
	runQuiet("sysctl", "-qw", "net.ipv4.ip_forward=0")
	// 确保工作目录存在
	os.MkdirAll("/var/lib/fanout", 0700)
}

// checkTunDevice 检查 TUN/TAP 设备是否可用。
// openvpn 需要 /dev/net/tun 来创建 tun0 虚拟网卡。
// LXC 容器经常未给此权限，需要提前报错。
func checkTunDevice() error {
	// 检查设备节点是否存在
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		if os.IsNotExist(err) {
			// 尝试创建
			if err := exec.Command("mknod", "/dev/net/tun", "c", "10", "200").Run(); err != nil {
				return fmt.Errorf("TUN/TAP 设备不可用: /dev/net/tun 不存在且无法创建\n"+
					"  → 原因: 容器/LXC 未给 /dev/net/tun 权限\n"+
					"  → 解决: 在宿主机执行 'ls -l /dev/net/tun' 确认存在，然后以 --privileged 或\n"+
					"          --device=/dev/net/tun 参数重新启动容器\n"+
					"  → 验证: ls /dev/net/tun 应存在，且 mknod 不报 Operation not permitted")
			}
		} else {
			return fmt.Errorf("检查 /dev/net/tun 失败: %w", err)
		}
	}

	// 检查是否可读写
	f, err := os.OpenFile("/dev/net/tun", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("TUN/TAP 设备不可读写: %w\n  → 需要 --privileged 或 --device=/dev/net/tun 权限", err)
	}
	f.Close()

	return nil
}

// checkCommand 检查命令是否存在于 PATH 中（包括 /sbin、/usr/sbin）。
func checkCommand(name string) error {
	// 先查 PATH
	if _, err := exec.LookPath(name); err == nil {
		return nil
	}
	// 再查常见 sbin 路径（PATH 可能不包含这些）
	for _, dir := range []string{"/sbin", "/usr/sbin", "/usr/local/sbin"} {
		path := dir + "/" + name
		if fi, err := os.Stat(path); err == nil && fi.Mode().IsRegular() {
			return nil
		}
	}
	return fmt.Errorf("command not found in PATH or /sbin:/usr/sbin")
}