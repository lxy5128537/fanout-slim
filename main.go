package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version 由构建时通过 -ldflags 注入。
var version = "dev"

func main() {
	fmt.Printf("fanout-slim v%s — 双隧道自动故障切换 SOCKS5 入口\n", version)

	if os.Geteuid() != 0 {
		log.Fatal("需要 root 权限（要创建 netns 和改 iptables）")
	}

	// 设置父进程死亡信号：当 sudo 退出时，自动收到 SIGTERM
	// 这样即使 SSH 超时断开，也能触发清理
	_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, syscall.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0)
	if errno != 0 {
		log.Printf("警告: 设置死亡信号失败: %v", errno)
	}

	ppid := os.Getppid()
	log.Printf("父进程 PID: %d", ppid)

	workDir := "/var/lib/fanout"
	if err := os.MkdirAll(workDir, 0700); err != nil {
		log.Fatalf("创建工作目录失败: %v", err)
	}
	if err := prepareHost(); err != nil {
		log.Fatal(err)
	}
	pool := NewTunnelPool(workDir)

	// 父进程监控：当 sudo 退出时自动清理
	// 必须在 Init() 之前启动，因为 Init() 可能长时间阻塞
	go func() {
		for {
			// 检查父进程是否还活着（信号 0 是探活）
			parent, err := os.FindProcess(ppid)
			if err != nil || parent.Signal(syscall.Signal(0)) != nil {
				log.Println("父进程已退出，正在清理...")
				pool.Shutdown()
				os.Exit(0)
			}
			time.Sleep(1 * time.Second)
		}
	}()

	// 信号处理：优雅退出
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGPIPE)
	go func() {
		<-stop
		pool.Shutdown()
		os.Exit(0)
	}()

	if err := pool.Init(); err != nil {
		log.Fatalf("初始化隧道池失败: %v", err)
	}

	// 启动健康检查（自动切换 + 自动刷新）
	go pool.WatchHealth()

	// 启动 SOCKS5 服务（固定端口 10000，无认证）
	go StartSocksServer(pool)

	// 打印状态
	activeIP, standbyIP, _, _ := pool.Status()
	log.Printf("主隧道出口 IP: %s", activeIP)
	if standbyIP != "" {
		log.Printf("备用隧道出口 IP: %s", standbyIP)
	} else {
		log.Printf("备用隧道: 无")
	}
	log.Printf("SOCKS5 入口: 127.0.0.1:10000")

	// 等待信号
	<-stop
	pool.Shutdown()
}