package main

import (
	"fmt"
	"log"
	"math/rand"
	"net"
	"sort"
	"sync"
	"time"
)

// TunnelPool 管理两条隧道：主隧道（活跃）和备用隧道（待命）。
// 入口 SOCKS5 始终走主隧道，主隧道故障时立即切换到备用隧道。
//
// liveNodes 是国内环境下的"节点存活预扫描"缓存：
// 国内环境下 90%+ 节点被 GFW 挡住，直接挑随机节点基本必死。
// preScanLive 会对候选池做并发 TCP 443 探活，挑出真正可达的节点。
type TunnelPool struct {
	mu               sync.RWMutex
	active           *Tunnel  // 当前活跃的主隧道
	standby          *Tunnel  // 待命的备用隧道
	workDir          string
	country          string   // 指定国家代码，为空则不限
	nodes            []Node
	liveNodes        []Node   // 国内环境下预扫描出的存活节点
	lastStandbyRetry time.Time
	shutdownCh       chan struct{} // 仅 Shutdown() 关闭，后台 goroutine 据此退出
}

// NewTunnelPool 创建隧道池。
func NewTunnelPool(workDir string, country string) *TunnelPool {
	return &TunnelPool{workDir: workDir, country: country, shutdownCh: make(chan struct{})}
}

// Init 初始化隧道池：拉取节点列表，启动两条隧道。
// 先启动的成为主隧道，后启动的成为备用隧道。
// 国内环境下会先做一次"节点存活预扫描"，挑出真正可达的节点，避免被 GFW 挡死。
func (p *TunnelPool) Init() error {
	// 拉取节点列表
	log.Printf("正在拉取 VPN Gate 节点列表...")
	nodes, err := fetchNodes(60 * time.Second)
	if err != nil {
		return fmt.Errorf("拉取节点列表失败: %w", err)
	}
	p.nodes = nodes
	log.Printf("已获取 %d 个节点", len(nodes))

	// 国内环境: 预扫描活节点池
	if geoClass.IsCN() {
		p.preScanAndLog()
	}

	// 主隧道同步拉起
	t1, err := p.startTunnel(1)
	if err != nil {
		return fmt.Errorf("启动主隧道失败: %w", err)
	}
	p.active = t1
	log.Printf("主隧道出口 IP: %s", t1.ExitIP)

	// 备用隧道异步拉起：失败不影响主隧道和 SOCKS5
	go func() {
		t2, err := p.startTunnel(2)
		if err != nil {
			log.Printf("备用隧道启动失败: %v", err)
			return
		}
		p.mu.Lock()
		p.standby = t2
		p.mu.Unlock()
		log.Printf("备用隧道出口 IP: %s", t2.ExitIP)
	}()

	return nil
}

// preScanAndLog 在国内环境下对候选节点做并发 TCP 443 探活。
// 命中活节点数量 >= 2 时存入 liveNodes，供 pickNode 优先挑选。
func (p *TunnelPool) preScanAndLog() {
	if !geoClass.IsCN() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	// 过滤 + 去重, 最多 40 个候选
	cand := make([]Node, 0, 40)
	seen := map[string]bool{}
	for _, n := range p.nodes {
		if p.country != "" && n.CountryCode != p.country {
			continue
		}
		if seen[n.IP] {
			continue
		}
		seen[n.IP] = true
		cand = append(cand, n)
		if len(cand) >= 40 {
			break
		}
	}

	log.Printf("国内环境: 开始对 %d 个候选节点做 TCP 443 存活预扫描（3s/节点, 50 并发）", len(cand))
	t0 := time.Now()
	live := preScanLive(cand, 3*time.Second, 50)
	dt := time.Since(t0)
	log.Printf("存活预扫描完成: 共 %d 个节点可达（耗时 %v）", len(live), dt.Round(time.Millisecond))
	p.liveNodes = live

	// 命中 >= 2 才启用; 否则保留海外式随机挑选
	if len(live) >= 2 {
		log.Printf("启用活节点优先策略: %d 个可用节点", len(live))
	}
}

// startTunnel 启动一条新隧道，slot 用于区分 netns 名称。
func (p *TunnelPool) startTunnel(slot int) (*Tunnel, error) {
	node, err := p.pickNode("")
	if err != nil {
		return nil, err
	}

	t := &Tunnel{
		Slot:   slot,
		Node:   node,
		Status: "starting",
		Since:  time.Now(),
		workDir: p.workDir,
	}
	log.Printf("隧道 %d: 正在连接 %s (%s)", slot, node.HostName, node.CountryCode)

	// bringUp 是同步的，会在内部重试最多 6 个候选节点
	if err := p.bringUp(t); err != nil {
		return nil, fmt.Errorf("隧道 %d: %w", slot, err)
	}

	log.Printf("隧道 %d: 已就绪，出口 IP: %s", slot, t.ExitIP)
	return t, nil
}

// bringUp 拉起一条隧道，自动在同地区候选节点中重试。
// 如果同地区全部失败，尝试其他地区的节点。
func (p *TunnelPool) bringUp(t *Tunnel) error {
	firstRegion := t.Node.CountryCode
	// 先试同地区，再试其他地区
	attempts := []string{firstRegion, ""} // "" 表示不限地区
	var lastErr error

	for _, region := range attempts {
		candidates := p.candidatesFor(t.Node, region)
		for i, node := range candidates {
			// 检查是否已整体关闭
			select {
			case <-p.shutdownCh:
				return fmt.Errorf("已关闭")
			default:
			}

			if i > 0 && p.nodeInUse(node.HostName, t.Slot) {
				continue
			}
			t.Node = node
			if i > 0 {
				t.Status = "starting"
				t.Err = fmt.Sprintf("已换到第 %d 个候选节点", i+1)
			}

			err := p.tryNode(t)
			if err == nil {
				t.Status = "up"
				t.Err = ""
				return nil
			}
			lastErr = err
			log.Printf("隧道 %d 节点 %s (%s) 失败: %v", t.Slot, node.HostName, node.CountryCode, err)
			t.teardownNetns()
			t.cleanupConfig()
		}
	}

	t.Status = "failed"
	if lastErr != nil {
		t.Err = fmt.Sprintf("尝试所有候选节点均失败，最后一个: %v", lastErr)
	}
	return fmt.Errorf(t.Err)
}

// tryNode 尝试用当前节点把隧道拉起来。
func (p *TunnelPool) tryNode(t *Tunnel) error {
	if err := t.setupNetns(); err != nil {
		return err
	}
	if err := t.startOpenVPN(); err != nil {
		return err
	}
	ip, err := t.probeExitIP()
	if err != nil {
		return err
	}
	t.ExitIP = ip
	return nil
}

// candidatesFor 以指定节点打头，后面跟上候选节点。
// region 为空时不限地区，否则只取同地区。
// 排序规则：优先 443 端口，再按连接数降序。
func (p *TunnelPool) candidatesFor(first Node, region string) []Node {
	const maxTries = 3
	p.mu.RLock()
	defer p.mu.RUnlock()

	used := map[string]bool{first.HostName: true}
	if p.active != nil {
		used[p.active.Node.HostName] = true
	}
	if p.standby != nil {
		used[p.standby.Node.HostName] = true
	}

	// 收集同地区候选节点
	var pool []Node
	for _, n := range p.nodes {
		if used[n.HostName] {
			continue
		}
		if region != "" && n.CountryCode != region {
			continue
		}
		if p.country != "" && n.CountryCode != p.country {
			continue
		}
		pool = append(pool, n)
	}

	// 排序：优先 443 端口，再按会话数降序
	sort.Slice(pool, func(i, j int) bool {
		pi := portPriority(pool[i].Port)
		pj := portPriority(pool[j].Port)
		if pi != pj {
			return pi < pj
		}
		return pool[i].Sessions > pool[j].Sessions
	})

	out := []Node{first}
	for i := 0; i < len(pool) && len(out) < maxTries; i++ {
		out = append(out, pool[i])
	}
	return out
}

// nodeInUse 判断某节点是否已被别的隧道占用。
func (p *TunnelPool) nodeInUse(host string, exceptSlot int) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active != nil && p.active.Slot != exceptSlot && p.active.Node.HostName == host {
		return true
	}
	if p.standby != nil && p.standby.Slot != exceptSlot && p.standby.Node.HostName == host {
		return true
	}
	return false
}

// pickNode 从节点列表中挑一个可用的节点。
// 如果 liveNodes 有缓存（国内环境预扫描结果）且数量 >= 2，优先从 liveNodes 挑；
// 否则走常规的随机挑选。
// exclude 不为空时排除该主机名。
func (p *TunnelPool) pickNode(exclude string) (Node, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	used := map[string]bool{}
	if p.active != nil {
		used[p.active.Node.HostName] = true
	}
	if p.standby != nil {
		used[p.standby.Node.HostName] = true
	}
	if exclude != "" {
		used[exclude] = true
	}

	// 优先从 liveNodes 挑（如果存活数 >= 2 才启用）
	if len(p.liveNodes) >= 2 {
		var live []Node
		for _, n := range p.liveNodes {
			if used[n.HostName] {
				continue
			}
			if p.country != "" && n.CountryCode != p.country {
				continue
			}
			live = append(live, n)
		}
		if len(live) >= 2 {
			return live[rand.Intn(len(live))], nil
		}
	}

	// 走常规节点列表
	var candidates []Node
	for _, n := range p.nodes {
		if used[n.HostName] {
			continue
		}
		if p.country != "" && n.CountryCode != p.country {
			continue
		}
		candidates = append(candidates, n)
	}
	if len(candidates) == 0 {
		return Node{}, fmt.Errorf("没有可用的节点（国家 %s），试试重新拉取列表", p.country)
	}
	return candidates[rand.Intn(len(candidates))], nil
}

// refreshNodes 重新拉取节点列表。
// 如果运行在国内环境，会同步刷新活节点预扫描缓存，
// 因为 GFW 拦截状态随时可能变化（旧的活节点可能已死，新的可能已通）。
func (p *TunnelPool) refreshNodes() error {
	nodes, err := fetchNodes(60 * time.Second)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.nodes = nodes
	p.mu.Unlock()

	// 国内环境下重扫活节点池
	if geoClass.IsCN() {
		log.Println("国内环境: 节点列表已刷新，重新做存活预扫描")
		p.preScanAndLog()
	}
	return nil
}

// GetActiveDialer 返回当前主隧道的出站拨号器。
// 若主隧道不可用，返回 nil。
func (p *TunnelPool) GetActiveDialer() func(string, string) (net.Conn, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active == nil || p.active.Status != "up" {
		return nil
	}
	return dialerInNetns(p.active.nsName())
}

// SwitchToStandby 把备用隧道提升为主隧道，原主隧道降为备用。
// 如果备用隧道不可用，则不切换。
func (p *TunnelPool) SwitchToStandby() bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.standby == nil || p.standby.Status != "up" {
		log.Printf("切换失败: 备用隧道不可用")
		return false
	}

	log.Printf("正在切换到备用隧道 %s (%s)", p.standby.Node.HostName, p.standby.ExitIP)
	p.active, p.standby = p.standby, p.active
	return true
}

// ReplaceTunnel 用新隧道替换旧隧道。旧隧道必须是 active 或 standby。
func (p *TunnelPool) ReplaceTunnel(old, newTunnel *Tunnel) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.active == old {
		p.active = newTunnel
		log.Printf("主隧道已替换为 %s (%s)", newTunnel.Node.HostName, newTunnel.ExitIP)
	} else if p.standby == old {
		p.standby = newTunnel
		log.Printf("备用隧道已替换为 %s (%s)", newTunnel.Node.HostName, newTunnel.ExitIP)
	} else {
		// 旧隧道不在池中，新隧道直接丢弃
		newTunnel.stop()
	}
}

// Status 返回当前隧道池状态摘要。
func (p *TunnelPool) Status() (activeIP, standbyIP string, activeOK, standbyOK bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.active != nil {
		activeIP = p.active.ExitIP
		activeOK = p.active.Status == "up"
	}
	if p.standby != nil {
		standbyIP = p.standby.ExitIP
		standbyOK = p.standby.Status == "up"
	}
	return
}

// Shutdown 清理所有隧道并释放资源。
func (p *TunnelPool) Shutdown() {
	p.mu.Lock()
	if p.active != nil {
		p.active.stop()
	}
	if p.standby != nil {
		p.standby.stop()
	}
	p.active = nil
	p.standby = nil
	p.mu.Unlock()
	close(p.shutdownCh) // 通知所有后台 goroutine 退出
	// 清理可能残留的 netns 和 iptables（即使隧道未完全建立）
	cleanupStale()
}

// preScanLive 对候选节点并发做 TCP 443 端口存活探测。
// 国内环境下 90%+ 节点被 GFW 挡死，直接挑随机节点基本必死。
// 先快速筛出真正可达的节点，再让主/备隧道从活节点池中挑，显著降低启动失败率。
//
// 参数：
//   - perTryTimeout 每个 TCP 尝试超时（建议 3s）
//   - concurrency   并发度（<= 0 时使用默认 50）
func preScanLive(cand []Node, perTryTimeout time.Duration, concurrency int) []Node {
	if len(cand) == 0 {
		return nil
	}
	if concurrency <= 0 {
		concurrency = 50
	}

	type result struct{ idx int; ok bool }
	resCh := make(chan result, len(cand))
	sem := make(chan struct{}, concurrency)

	for i, n := range cand {
		sem <- struct{}{}
		go func(idx int, node Node) {
			defer func() { <-sem }()
			ip := node.IP
			if ip == "" {
				resCh <- result{idx: idx, ok: false}
				return
			}
			conn, err := net.DialTimeout("tcp4", net.JoinHostPort(ip, "443"), perTryTimeout)
			resCh <- result{idx: idx, ok: err == nil}
			if err == nil {
				conn.Close()
			}
		}(i, n)
	}

	live := make([]Node, 0)
	for range cand {
		r := <-resCh
		if r.ok {
			live = append(live, cand[r.idx])
		}
	}
	return live
}