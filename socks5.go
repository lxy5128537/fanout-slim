package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

const (
	socksVer5     = 0x05
	authNone      = 0x00
	cmdConnect    = 0x01
	atypIPv4      = 0x01
	atypDomain    = 0x03
	atypIPv6      = 0x04
	repSuccess    = 0x00
	repGenFail    = 0x01
	repHostUnre   = 0x04
	repCmdNotSupp = 0x07

	socksDefaultPort = 10000
)

// StartSocksServer 启动 SOCKS5 服务，所有出站连接通过 pool 的当前主隧道路由。
func StartSocksServer(pool *TunnelPool, port int) {
	if port <= 0 || port > 65535 {
		port = socksDefaultPort
	}
	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("监听 SOCKS5 %s 失败: %v", addr, err)
	}
	log.Printf("SOCKS5 入口已启动: %s（无认证）", addr)

	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go handleSocks(conn, pool)
	}
}

// handleSocks 处理一条 SOCKS5 连接。
func handleSocks(client net.Conn, pool *TunnelPool) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(30 * time.Second))

	// 无认证握手
	if err := socksNoAuthHandshake(client); err != nil {
		return
	}

	addr, err := socksReadRequest(client)
	if err != nil {
		if errors.Is(err, errCmdNotSupported) {
			socksReply(client, repCmdNotSupp)
		} else {
			socksReply(client, repGenFail)
		}
		return
	}

	// 获取当前主隧道的拨号器
	dialer := pool.GetActiveDialer()
	if dialer == nil {
		log.Printf("主隧道不可用，拒绝连接 %s", addr)
		socksReply(client, repHostUnre)
		return
	}

	remote, err := dialer("tcp", addr)
	if err != nil {
		socksReply(client, repHostUnre)
		return
	}
	defer remote.Close()

	if err := socksReply(client, repSuccess); err != nil {
		return
	}

	// 转发阶段不设整体超时，交给两端自然关闭
	_ = client.SetDeadline(time.Time{})
	_ = remote.SetDeadline(time.Time{})
	relay(client, remote)
}

// socksNoAuthHandshake 完成无认证方法协商。
func socksNoAuthHandshake(c net.Conn) error {
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return err
	}
	if head[0] != socksVer5 {
		return errors.New("不是 socks5")
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	// 回复无认证
	_, err := c.Write([]byte{socksVer5, authNone})
	return err
}

var errCmdNotSupported = errors.New("仅支持 CONNECT")
var errIPv6NotSupported = errors.New("隧道内不支持 IPv6")

func socksReadRequest(c net.Conn) (string, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		return "", err
	}
	if head[1] != cmdConnect {
		return "", errCmdNotSupported
	}

	var host string
	switch head[3] {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		host = net.IP(b).String()
	case atypIPv6:
		b := make([]byte, 16)
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		return "", errIPv6NotSupported
	case atypDomain:
		l := make([]byte, 1)
		if _, err := io.ReadFull(c, l); err != nil {
			return "", err
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return "", err
		}
		host = string(b)
	default:
		return "", fmt.Errorf("不支持的地址类型 %d", head[3])
	}

	pb := make([]byte, 2)
	if _, err := io.ReadFull(c, pb); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(pb)))), nil
}

func socksReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{socksVer5, code, 0x00, atypIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

func relay(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); done <- struct{}{} }()
	go func() { io.Copy(b, a); done <- struct{}{} }()
	<-done
}