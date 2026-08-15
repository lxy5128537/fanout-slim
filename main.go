package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// version 由构建时通过 -ldflags 注入。
var version = "Ver1.0.0"

var socksPort = flag.Int("p", 10000, "SOCKS5 监听端口（默认 10000）")
var country = flag.String("c", "", "VPN 节点国家代码（如 JP/KR/US，为空则不限）")
var daemon = flag.Bool("d", false, "Daemon 模式：不监控父进程，避免开机脚本/SSH 断开时退出")
var geoOverride = flag.String("geo-override", "", "运行环境覆盖: cn / overseas（默认自动探测）")
var mirrorURL = flag.String("mirror", "", "VPN Gate 节点列表 JSON 镜像 URL（默认内置，也可用 FANOUT_MIRROR 环境变量指定）")

func main() {
	flag.Parse()

	// 将 --geo-override 同步到环境变量，让 geo.Detect() 能通过环境变量读取到
	if *geoOverride != "" {
		os.Setenv("FANOUT_GEO_OVERRIDE", *geoOverride)
	}

	// 将 --mirror 同步到环境变量，让 tryFetchMirror() 能通过 FANOUT_MIRROR 读取到
	if *mirrorURL != "" {
		os.Setenv("FANOUT_MIRROR", *mirrorURL)
	}

	// 探测运行环境（国内 / 海外），失败时不阻塞，按海外兜底
	geo := Detect()
	geoClass = geo.Class

	fmt.Printf("fanout-slim v%s — 双隧道自动故障切换 SOCKS5 入口\n", version)

	if os.Geteuid() != 0 {
		log.Fatal("需要 root 权限（要创建 netns 和改 iptables）")
	}

	// 父进程死亡信号 + 父进程监控：仅非 daemon 模式启用
	// 开机脚本场景（local.d）下父进程是 init 的 shell，SSH 断开时父进程退出会误触发 SIGTERM
	if !*daemon {
		_, _, errno := syscall.Syscall(syscall.SYS_PRCTL, syscall.PR_SET_PDEATHSIG, uintptr(syscall.SIGTERM), 0)
		if errno != 0 {
			log.Printf("警告: 设置死亡信号失败: %v", errno)
		}
		ppid := os.Getppid()
		log.Printf("父进程 PID: %d", ppid)
		log.Printf("父进程监控: 已启用")

		go func() {
			for {
				parent, err := os.FindProcess(ppid)
				if err != nil || parent.Signal(syscall.Signal(0)) != nil {
					log.Println("父进程已退出，正在清理...")
					return
				}
				time.Sleep(1 * time.Second)
			}
		}()
	}

	if *country != "" {
		log.Printf("国家过滤: %s", *country)
	}
	if *daemon {
		log.Printf("Daemon 模式: 父进程监控已禁用")
	}

	workDir := "/var/lib/fanout"
	if err := os.MkdirAll(workDir, 0700); err != nil {
		log.Fatalf("创建工作目录失败: %v", err)
	}
	if err := prepareHost(); err != nil {
		log.Fatal(err)
	}
	pool := NewTunnelPool(workDir, *country)

	// 信号处理：优雅退出（不监听 SIGPIPE——openvpn/curl 管道断开会误触发 Shutdown）
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		go func() {
			sig := <-stop
			log.Printf("收到信号 %s，正在退出", sig)
			pool.Shutdown()
			os.Exit(0)
		}()

	if err := pool.Init(); err != nil {
		log.Fatalf("初始化隧道池失败: %v", err)
	}

	// 启动健康检查（自动切换 + 自动刷新）
	go pool.WatchHealth()

	// 启动 SOCKS5 服务（无认证，端口可通过 -p 指定）
	go StartSocksServer(pool, *socksPort)

	// 打印状态
	activeIP, standbyIP, _, _ := pool.Status()
	log.Printf("主隧道出口 IP: %s", activeIP)
	if standbyIP != "" {
		log.Printf("备用隧道出口 IP: %s", standbyIP)
	} else {
		log.Printf("备用隧道: 无")
	}
	log.Printf("SOCKS5 入口: 127.0.0.1:%d", *socksPort)

	// 等待信号
	<-stop
	pool.Shutdown()
}