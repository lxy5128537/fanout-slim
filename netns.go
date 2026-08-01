package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// dialerInNetns 返回一个在指定 netns 内建立出站连接的 dial 函数。
// 每次拨号都要切一次 netns，因为 socket 的归属在创建时确定。
// DNS 解析使用隧道内的 8.8.8.8，避免 Go 缓存宿主机的 DNS 配置。
func dialerInNetns(nsName string) func(network, addr string) (net.Conn, error) {
	return func(network, addr string) (net.Conn, error) {
		network = forceIPv4Network(network)

		type result struct {
			conn net.Conn
			err  error
		}
		ch := make(chan result, 1)

		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			origin, err := os.Open("/proc/self/ns/net")
			if err != nil {
				ch <- result{nil, err}
				return
			}
			defer origin.Close()

			target, err := os.Open("/var/run/netns/" + nsName)
			if err != nil {
				ch <- result{nil, err}
				return
			}
			defer target.Close()

			// 切换到隧道 netns
			if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
				ch <- result{nil, err}
				return
			}

			// 解析地址：分离 host 和 port
			host, portStr, err := net.SplitHostPort(addr)
			if err != nil {
				// 切换回原 netns 再返回
				unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET)
				ch <- result{nil, fmt.Errorf("拆分地址 %s 失败: %w", addr, err)}
				return
			}

			port, err := strconv.Atoi(portStr)
			if err != nil {
				unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET)
				ch <- result{nil, fmt.Errorf("端口 %s 无效: %w", portStr, err)}
				return
			}

			// 如果 host 是 IP 则直接拨号
			if ip := net.ParseIP(host); ip != nil {
				tcpAddr := &net.TCPAddr{IP: ip, Port: port}
				conn, dialErr := net.DialTCP(network, nil, tcpAddr)
				unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET)
				ch <- result{conn, dialErr}
				return
			}

			// 域名解析：在隧道 netns 内用 8.8.8.8 解析
			resolver := &net.Resolver{
				Dial: func(ctx context.Context, nw, dnsAddr string) (net.Conn, error) {
					d := net.Dialer{Timeout: 8 * time.Second}
					return d.DialContext(ctx, "tcp4", "8.8.8.8:53")
				},
			}
			ips, lookupErr := resolver.LookupIPAddr(context.Background(), host)
			if lookupErr != nil {
				unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET)
				ch <- result{nil, fmt.Errorf("DNS 解析 %s 失败: %w", host, lookupErr)}
				return
			}

			// 取第一个 IPv4 地址
			var dialIP net.IP
			for _, ip := range ips {
				if ip4 := ip.IP.To4(); ip4 != nil {
					dialIP = ip4
					break
				}
			}
			if dialIP == nil && len(ips) > 0 {
				dialIP = ips[0].IP
			}
			if dialIP == nil {
				unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET)
				ch <- result{nil, fmt.Errorf("DNS 解析 %s 没有返回地址", host)}
				return
			}

			// 在隧道 netns 内拨号
			tcpAddr := &net.TCPAddr{IP: dialIP, Port: port}
			conn, dialErr := net.DialTCP(network, nil, tcpAddr)

			// 切换回原 netns
			if err := unix.Setns(int(origin.Fd()), unix.CLONE_NEWNET); err != nil {
				if conn != nil {
					conn.Close()
				}
				ch <- result{nil, err}
				return
			}

			ch <- result{conn, dialErr}
		}()

		r := <-ch
		return r.conn, r.err
	}
}

// forceIPv4Network 把 tcp/udp 收敛成 tcp4/udp4，已经指定版本的原样返回。
func forceIPv4Network(network string) string {
	switch network {
	case "tcp":
		return "tcp4"
	case "udp":
		return "udp4"
	}
	return network
}