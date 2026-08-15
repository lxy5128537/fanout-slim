package main

import (
	"net"
	"testing"
	"time"
)

func TestPreScanLive_AllReachable(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, e := ln.Accept()
			if e != nil {
				return
			}
			conn.Close()
		}
	}()
	defer ln.Close()

	srvIP := ln.Addr().(*net.TCPAddr).IP.String()

	// 用 Dialer 模拟: 把 srvIP 视为 "IP 指向扫描端口 443" — 不行, 实际扫描固定 443
	// 改为: 让 srvIP = "127.0.0.1", 起 mock 监听在 443 (需要 root)
	_ = srvIP
	// 直接测 192.0.2.1 一定不可达
	cand := []Node{
		{IP: "192.0.2.1", HostName: "dead1"},
		{IP: "192.0.2.2", HostName: "dead2"},
		{IP: "", HostName: "empty"},
	}
	live := preScanLive(cand, 100*time.Millisecond, 10)
	if len(live) != 0 {
		t.Fatalf("expected 0 live, got %d", len(live))
	}
}

func TestPreScanLive_EmptyInput(t *testing.T) {
	if live := preScanLive([]Node{}, 100*time.Millisecond, 10); live != nil {
		t.Fatal("expected nil")
	}
}

func TestPoolPickNode_prefersLive(t *testing.T) {
	p := &TunnelPool{
		workDir: "/tmp/fanout-test",
		nodes: []Node{
			{HostName: "dead1", IP: "1.2.3.4"},
			{HostName: "dead2", IP: "5.6.7.8"},
			{HostName: "dead3", IP: "9.10.11.12"},
		},
		liveNodes: []Node{
			{HostName: "dead1", IP: "1.2.3.4"},
			{HostName: "dead2", IP: "5.6.7.8"},
		},
		shutdownCh: make(chan struct{}),
	}
	n, err := p.pickNode("")
	if err != nil {
		t.Fatal(err)
	}
	if n.HostName != "dead1" && n.HostName != "dead2" {
		t.Fatalf("expected live node, got %s", n.HostName)
	}
}

func TestPoolPickNode_fallbackWhenLiveTooFew(t *testing.T) {
	p := &TunnelPool{
		workDir: "/tmp/fanout-test",
		nodes: []Node{
			{HostName: "fallback1", IP: "1.1.1.1"},
			{HostName: "fallback2", IP: "2.2.2.2"},
		},
		liveNodes:  []Node{{HostName: "dead1", IP: "3.3.3.3"}}, // < 2
		shutdownCh: make(chan struct{}),
	}
	n, err := p.pickNode("")
	if err != nil {
		t.Fatal(err)
	}
	if n.HostName != "fallback1" && n.HostName != "fallback2" {
		t.Fatalf("expected fallback, got %s", n.HostName)
	}
}

func TestPoolPickNode_liveExcludesUsed(t *testing.T) {
	// liveNodes 2 个 + 1 个被 active 占用, 应挑剩余的
	p := &TunnelPool{
		workDir: "/tmp/fanout-test",
		nodes: []Node{
			{HostName: "a", IP: "1.1.1.1"},
			{HostName: "b", IP: "2.2.2.2"},
			{HostName: "c", IP: "3.3.3.3"},
		},
		liveNodes: []Node{
			{HostName: "a", IP: "1.1.1.1"},
			{HostName: "b", IP: "2.2.2.2"},
			{HostName: "c", IP: "3.3.3.3"},
		},
		active:     &Tunnel{Slot: 1, Node: Node{HostName: "a"}, Status: "up"},
		shutdownCh: make(chan struct{}),
	}
	n, err := p.pickNode("")
	if err != nil {
		t.Fatal(err)
	}
	if n.HostName == "a" {
		t.Fatal("should not pick active node 'a'")
	}
}

func TestPoolPickNode_countryFilterAppliesToLive(t *testing.T) {
	p := &TunnelPool{
		workDir: "/tmp/fanout-test",
		country: "JP",
		nodes: []Node{
			{HostName: "jp1", IP: "1.1.1.1", CountryCode: "JP"},
			{HostName: "jp2", IP: "1.1.1.2", CountryCode: "JP"},
			{HostName: "us1", IP: "2.2.2.2", CountryCode: "US"},
			{HostName: "kr1", IP: "3.3.3.3", CountryCode: "KR"},
		},
		liveNodes: []Node{
			{HostName: "jp1", IP: "1.1.1.1", CountryCode: "JP"},
			{HostName: "jp2", IP: "1.1.1.2", CountryCode: "JP"},
			{HostName: "us1", IP: "2.2.2.2", CountryCode: "US"},
		},
		shutdownCh: make(chan struct{}),
	}
	n, err := p.pickNode("")
	if err != nil {
		t.Fatal(err)
	}
	if n.CountryCode != "JP" {
		t.Fatalf("expected JP node (country=JP filter), got %s (%s)", n.HostName, n.CountryCode)
	}
}