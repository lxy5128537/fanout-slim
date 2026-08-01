package main

import (
	"log"
	"time"
)

const (
	healthInterval     = 5 * time.Second
	standbyRetryAfter  = 30 * time.Second // 备用隧道创建失败后，等多久再重试
)

// WatchHealth 周期检查两条隧道：
// - 主隧道故障 → 立即切换到备用隧道，然后刷新原主隧道
// - 备用隧道故障 → 直接刷新备用隧道
// - 始终保持两条隧道可用
func (p *TunnelPool) WatchHealth() {
	// 刚启动时给隧道一些时间建立连接
	time.Sleep(10 * time.Second)

	for range time.Tick(healthInterval) {
		activeOK, standbyOK := p.checkTunnels()

		if !activeOK {
			log.Printf("主隧道故障，正在切换到备用隧道")
			if p.SwitchToStandby() {
				// 切换成功，原主隧道（现在是备用）需要刷新
				oldActive := p.standby // SwitchToStandby 交换了指针
				go p.refreshFailedTunnel(oldActive)
			} else {
				// 备用隧道也不可用，刷新主隧道
				active := p.active
				if active != nil {
					go p.refreshFailedTunnel(active)
				}
			}
			continue
		}

		// 主隧道正常，检查备用隧道
		if !standbyOK {
			standby := p.standby
			if standby != nil {
				log.Printf("备用隧道 %s 故障，正在刷新", standby.Node.HostName)
				go p.refreshFailedTunnel(standby)
			} else if time.Since(p.lastStandbyRetry) >= standbyRetryAfter {
				// 备用隧道不存在，且距上次重试已超过间隔，重试
				p.lastStandbyRetry = time.Now()
				go p.createStandbyTunnel()
			}
		}
	}
}

// checkTunnels 检查两条隧道的健康状况。
// - "up" 状态的隧道：通过 curl api.ipify.org 比对出口 IP
// - "starting" 状态的隧道：正在重建，给 3 分钟宽限，不视为故障
// - 其他状态（failed/stopped）：判定为故障
func (p *TunnelPool) checkTunnels() (activeOK, standbyOK bool) {
	p.mu.RLock()
	active := p.active
	standby := p.standby
	p.mu.RUnlock()

	if active != nil {
		switch active.Status {
		case "up":
			activeOK = active.tunnelHealthy()
		case "starting", "recovering":
			// 正在重建或恢复中，给宽限 3 分钟
			activeOK = time.Since(active.Since) < 3*time.Minute
		}
	}
	if standby != nil {
		switch standby.Status {
		case "up":
			standbyOK = standby.tunnelHealthy()
		case "starting", "recovering":
			standbyOK = time.Since(standby.Since) < 3*time.Minute
		}
	}
	return
}

// refreshFailedTunnel 刷新一条故障隧道：停掉旧的，建一条新的替换。
// 停掉前先标记为"recovering"，防止健康检查在重建期间重复触发。
func (p *TunnelPool) refreshFailedTunnel(failed *Tunnel) {
	oldHost := failed.Node.HostName
	log.Printf("正在刷新隧道 %s", oldHost)

	// 先标记为 recovering，让 checkTunnels 知道这条隧道正在被处理
	// 防止健康检查在重建期间重复触发
	failed.mu.Lock()
	failed.Status = "recovering"
	failed.mu.Unlock()

	// 停掉旧隧道（释放 netns 和端口）
	failed.stop()

	// 重新拉取节点列表，确保有最新数据
	if err := p.refreshNodes(); err != nil {
		log.Printf("刷新节点列表失败: %v", err)
	}

	// 新建一条隧道
	node, err := p.pickNode(oldHost)
	if err != nil {
		log.Printf("刷新隧道失败: %v", err)
		// 仍然尝试创建，不留空位
		node, err = p.pickNode("")
		if err != nil {
			log.Printf("无可用节点，隧道将保持空置: %v", err)
			return
		}
	}

	// 复用旧隧道的槽位
	newTunnel := &Tunnel{
		Slot:    failed.Slot,
		Node:    node,
		Status:  "starting",
		Since:   time.Now(),
		workDir: p.workDir,
	}

	log.Printf("新隧道 %d: 正在连接 %s (%s)", newTunnel.Slot, node.HostName, node.CountryCode)
	if err := p.bringUp(newTunnel); err != nil {
		log.Printf("新隧道 %d 启动失败: %v", newTunnel.Slot, err)
		// 失败后清理
		newTunnel.stop()
		return
	}

	log.Printf("新隧道 %d 已就绪，出口 IP: %s", newTunnel.Slot, newTunnel.ExitIP)
	p.ReplaceTunnel(failed, newTunnel)
}

// createStandbyTunnel 创建一条备用隧道（当 standby 为 nil 时调用）。
func (p *TunnelPool) createStandbyTunnel() {
	// 检查是否已有备用隧道
	p.mu.RLock()
	alreadyHasStandby := p.standby != nil
	activeSlot := 0
	if p.active != nil {
		activeSlot = p.active.Slot
	}
	p.mu.RUnlock()

	if alreadyHasStandby {
		return
	}

	// 找可用节点
	node, err := p.pickNode("")
	if err != nil {
		if err := p.refreshNodes(); err != nil {
			log.Printf("创建备用隧道失败: %v", err)
			return
		}
		node, err = p.pickNode("")
		if err != nil {
			log.Printf("创建备用隧道失败: %v", err)
			return
		}
	}

	// 备用隧道用 2 号槽位（如果主隧道是 1），否则用 1
	slot := 2
	if activeSlot == 2 {
		slot = 1
	}

	newTunnel := &Tunnel{
		Slot:    slot,
		Node:    node,
		Status:  "starting",
		Since:   time.Now(),
		workDir: p.workDir,
	}

	log.Printf("创建备用隧道 %d: 正在连接 %s (%s)", slot, node.HostName, node.CountryCode)
	if err := p.bringUp(newTunnel); err != nil {
		log.Printf("备用隧道创建失败: %v", err)
		newTunnel.stop()
		return
	}

	// 替换到池中。检查是否已有 standby（防止并发）
	p.mu.Lock()
	if p.standby == nil {
		p.standby = newTunnel
		log.Printf("备用隧道已创建: %s (%s)", newTunnel.Node.HostName, newTunnel.ExitIP)
	} else {
		// 已被别的 goroutine 创建了，丢弃
		newTunnel.stop()
	}
	p.mu.Unlock()
}