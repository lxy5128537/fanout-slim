package main

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const vpngateAPI = "https://www.vpngate.net/api/iphone/"

// vpngateMirrors 是 VPN Gate 官方镜像站列表（HTTP），
// 从 https://www.vpngate.net/cn/sites.aspx 自动更新。
// 使用 HTTP 而非 HTTPS，可避免 TLS 证书劫持问题。
// 轮换使用，哪个可用用哪个。
var vpngateMirrors = []string{
	"http://150.40.105.13:35090/api/iphone/",
	"http://150.40.105.7:15596/api/iphone/",
	"http://150.40.105.12:29202/api/iphone/",
	"http://150.40.105.11:4917/api/iphone/",
	"http://150.40.105.10:46711/api/iphone/",
	"http://150.40.105.8:61446/api/iphone/",
}

// githubMirror 是 GitHub 上 VPN Gate 节点列表的 JSON 镜像。
// 由 fdciabdul/Vpngate-Scraper-API 自动更新，每小时更新一次。
const githubMirror = "https://raw.githubusercontent.com/fdciabdul/Vpngate-Scraper-API/main/json/data.json"

// ghproxyMirror 是 ghproxy.net 加速版，国内环境默认首选。
// ghproxy 对 raw.githubusercontent.com 的镜像在国内普遍可达，
// 而 raw.githubusercontent.com 直连在国内通常被 GFW 挡死。
const ghproxyMirror = "https://ghproxy.net/https://raw.githubusercontent.com/fdciabdul/Vpngate-Scraper-API/main/json/data.json"

// defaultMirrors 默认镜像尝试顺序。
// 国内环境：ghproxy → raw.githubusercontent.com
// 海外环境：raw.githubusercontent.com → ghproxy
// 用户可通过 FANOUT_MIRROR 环境变量或 --mirror flag 强制指定。
func defaultMirrors() []string {
	if geoClass.IsCN() {
		return []string{ghproxyMirror, githubMirror}
	}
	return []string{githubMirror, ghproxyMirror}
}

// getMirrorURL 返回当前使用的镜像 URL。
// 优先使用 FANOUT_MIRROR 环境变量（或 --mirror flag），
// 否则用默认镜像（国内 ghproxy 优先）。
func getMirrorURL() string {
	if v := strings.TrimSpace(os.Getenv("FANOUT_MIRROR")); v != "" {
		return v
	}
	return defaultMirrors()[0]
}

var (
	geoClass    GeoClass // 当前运行环境（由 Detect() 设置，启动时初始化）
	vpngateIPv4 string   // VPN Gate 的 IPv4 直连地址，通过 FANOUT_VPNGATE_IP 环境变量设置
)

// Node 是一个 VPN Gate 节点。
type Node struct {
	HostName    string  `json:"hostname"`
	IP          string  `json:"ip"`
	Country     string  `json:"country"`
	CountryCode string  `json:"country_code"`
	Ping        int     `json:"ping"`
	SpeedMbps   float64 `json:"speed_mbps"`
	Sessions    int     `json:"sessions"`
	Config      string  `json:"-"` // 解码后的 .ovpn 内容
	Port        int     `json:"-"` // 从 Config 中提取的端口
}

// extractPort 从 OpenVPN 配置中提取远程端口。
// 格式: "remote <ip|hostname> <port>"
func extractPort(config string) int {
	for _, line := range strings.Split(config, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "remote ") {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				if port, err := strconv.Atoi(parts[2]); err == nil {
					return port
				}
			}
		}
	}
	return 0
}

// portPriority 返回端口的优先级值（越小越优先）。
func portPriority(port int) int {
	switch port {
	case 443:
		return 0
	case 995:
		return 1
	case 80:
		return 2
	default:
		return 3
	}
}

// fetchNodes 拉取并解析 VPN Gate 的节点列表。
// 按运行地区选择不同的拉取策略（见 fetchNodesCN / fetchNodesOverseas）。
func fetchNodes(timeout time.Duration) ([]Node, error) {
	// 允许通过环境变量强制指定探测到的 VPN Gate IPv4（备用直连）
	vpngateIPv4 = strings.TrimSpace(os.Getenv("FANOUT_VPNGATE_IP"))

	switch geoClass {
	case GeoCN:
		log.Printf("当前环境: 国内，使用国内优化拉取策略")
		return fetchNodesCN(timeout)
	case GeoOverseas:
		log.Printf("当前环境: 海外，使用海外拉取策略")
		return fetchNodesOverseas(timeout)
	default:
		// 未知时按海外处理（保守、最通用）
		log.Printf("运行环境未知，按海外策略拉取节点")
		return fetchNodesOverseas(timeout)
	}
}

// fetchNodesCN 国内环境：官方 API 和官方镜像站 IP 段 150.40.105.x 均被墙，
// 直接走 GitHub 镜像（HTTPS），超时略长以应对 GFW 干扰。
func fetchNodesCN(timeout time.Duration) ([]Node, error) {
	log.Println("国内策略: 跳过官方 API/镜像站（被墙），优先 GitHub 镜像")

	// 首选 GitHub 镜像（国内通常可访问，走 HTTPS）
	nodes, err := tryFetchMirror(45 * time.Second)
	if err == nil {
		return nodes, nil
	}
	log.Printf("GitHub 镜像拉取失败: %v", err)

	// 兜底：尝试 HTTP 镜像站（虽然大概率被墙，但留一条路）
	log.Println("尝试 HTTP 镜像站（国内通常失败）...")
	for _, url := range shuffleStrings(vpngateMirrors) {
		if nodes, err := tryFetchNodes(url, 8*time.Second); err == nil {
			log.Printf("HTTP 镜像 %s 拉取成功（非预期，但仍可用）", url)
			return nodes, nil
		}
	}

	return nil, fmt.Errorf("所有节点列表源均失败（国内策略），请检查网络或代理设置")
}

// fetchNodesOverseas 海外环境：官方 API 优先，镜像轮换兜底。
func fetchNodesOverseas(timeout time.Duration) ([]Node, error) {
	var lastErr error

	// 1. 官方 API（海外直连）
	if nodes, err := tryFetchNodes(vpngateAPI, 15*time.Second); err == nil {
		return nodes, nil
	} else {
		lastErr = err
		log.Printf("官方 API 失败: %v", err)
	}

	// 2. 官方镜像站轮换（HTTPS 优先；镜像 URL 是 HTTP，所以这里直接用 tryFetchNodes）
	urls := shuffleStrings(vpngateMirrors)
	for _, url := range urls {
		if nodes, err := tryFetchNodes(url, 10*time.Second); err == nil {
			log.Printf("镜像 %s 拉取成功", url)
			return nodes, nil
		}
	}
	log.Println("所有镜像站均不可用，尝试 GitHub 镜像")

	// 3. GitHub 镜像兜底
	if nodes, err := tryFetchMirror(30 * time.Second); err == nil {
		return nodes, nil
	} else {
		lastErr = err
	}

	return nil, fmt.Errorf("所有节点列表源均失败，最后错误: %w", lastErr)
}

// tryFetchNodes 从指定 URL 以 CSV 格式拉取 VPN Gate 节点列表。
func tryFetchNodes(url string, timeout time.Duration) ([]Node, error) {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   timeout,
			KeepAlive: 0,
			DualStack: false, // 禁止双栈，只走 IPv4
		}).DialContext,
	}
	if vpngateIPv4 != "" {
		transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp4", net.JoinHostPort(vpngateIPv4, "443"))
		}
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return parseNodeCSV(string(raw))
}

// tryFetchMirror 从 GitHub 镜像 JSON 拉取 VPN Gate 节点列表。
// 依次尝试 defaultMirrors() 中的多个镜像源（国内 ghproxy 优先），直到成功。
func tryFetchMirror(timeout time.Duration) ([]Node, error) {
	// 如果用户指定了 FANOUT_MIRROR，只试它
	userMirror := strings.TrimSpace(os.Getenv("FANOUT_MIRROR"))
	mirrors := defaultMirrors()
	if userMirror != "" {
		mirrors = []string{userMirror}
	}
	return tryFetchMirrorList(mirrors, timeout)
}

// tryFetchMirrorList 依次尝试多个镜像源拉取节点列表，返回第一个成功的。
func tryFetchMirrorList(mirrors []string, timeout time.Duration) ([]Node, error) {
	var lastErr error
	client := &http.Client{Timeout: timeout}
	for _, url := range mirrors {
		log.Printf("尝试镜像: %s", url)
		resp, err := client.Get(url)
		if err != nil {
			lastErr = fmt.Errorf("拉取 %s 失败: %w", url, err)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("%s HTTP %d", url, resp.StatusCode)
			continue
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 20*1024*1024))
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("读取 %s 失败: %w", url, err)
			continue
		}
		nodes, err := parseMirrorJSON(raw)
		if err != nil {
			lastErr = fmt.Errorf("解析 %s 失败: %w", url, err)
			continue
		}
		log.Printf("从镜像 %s 拉取成功", url)
		return nodes, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("所有镜像源均不可用")
}

// mirrorServer 对应 GitHub 镜像 JSON 中的单个服务器对象。
type mirrorServer struct {
	HostName     string `json:"hostname"`
	IP           string `json:"ip"`
	Ping         string `json:"ping"`
	Speed        string `json:"speed"`
	CountryLong  string `json:"countrylong"`
	CountryShort string `json:"countryshort"`
	ConfigB64    string `json:"openvpn_configdata_base64"`
}

// parseMirrorJSON 解析 GitHub 镜像 JSON 格式。
// 格式: [{"servers": [...], "countries": {...}}, 节点总数]
func parseMirrorJSON(raw []byte) ([]Node, error) {
	var wrapper []json.RawMessage
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, fmt.Errorf("JSON 解析失败: %w", err)
	}
	if len(wrapper) < 1 {
		return nil, fmt.Errorf("镜像 JSON 格式异常: 数据为空")
	}
	var data struct {
		Servers []mirrorServer `json:"servers"`
	}
	if err := json.Unmarshal(wrapper[0], &data); err != nil {
		return nil, fmt.Errorf("解析服务器列表失败: %w", err)
	}
	if len(data.Servers) == 0 {
		return nil, fmt.Errorf("镜像中无可用节点")
	}

	var nodes []Node
	for _, s := range data.Servers {
		if s.ConfigB64 == "" || s.HostName == "" {
			continue
		}
		cfg, err := base64.StdEncoding.DecodeString(s.ConfigB64)
		if err != nil {
			continue
		}
		ping, _ := strconv.Atoi(s.Ping)
		speed, _ := strconv.ParseFloat(s.Speed, 64)
		nodes = append(nodes, Node{
			HostName:    s.HostName,
			IP:          s.IP,
			Country:     s.CountryLong,
			CountryCode: s.CountryShort,
			Ping:        ping,
			SpeedMbps:   speed / 1e6,
			Config:      string(cfg),
			Port:        extractPort(string(cfg)),
		})
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("镜像中无有效节点")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].SpeedMbps > nodes[j].SpeedMbps })
	return nodes, nil
}

// parseNodeCSV 解析 VPN Gate 的 CSV。首行是 "*vpn_servers"，
// 第二行是以 '#' 开头的表头，末行是 "*"。
func parseNodeCSV(body string) ([]Node, error) {
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "*") {
			continue
		}
		kept = append(kept, strings.TrimPrefix(line, "#"))
	}
	if len(kept) < 2 {
		return nil, fmt.Errorf("节点列表格式异常: 有效行不足")
	}

	r := csv.NewReader(strings.NewReader(strings.Join(kept, "\n")))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("解析节点 CSV 失败: %w", err)
	}

	header := records[0]
	idx := map[string]int{}
	for i, name := range header {
		idx[strings.TrimSpace(name)] = i
	}
	need := []string{"HostName", "IP", "CountryLong", "CountryShort", "Ping", "Speed", "OpenVPN_ConfigData_Base64"}
	for _, k := range need {
		if _, ok := idx[k]; !ok {
			return nil, fmt.Errorf("节点列表缺少字段 %s", k)
		}
	}

	var nodes []Node
	for _, rec := range records[1:] {
		get := func(k string) string {
			i := idx[k]
			if i >= len(rec) {
				return ""
			}
			return rec[i]
		}
		cfgB64 := get("OpenVPN_ConfigData_Base64")
		if cfgB64 == "" || get("HostName") == "" {
			continue
		}
		cfg, err := base64.StdEncoding.DecodeString(cfgB64)
		if err != nil {
			continue
		}
		ping, _ := strconv.Atoi(get("Ping"))
		speed, _ := strconv.ParseFloat(get("Speed"), 64)
		sessions, _ := strconv.Atoi(get("NumVpnSessions"))
		nodes = append(nodes, Node{
			HostName:    get("HostName"),
			IP:          get("IP"),
			Country:     get("CountryLong"),
			CountryCode: get("CountryShort"),
			Ping:        ping,
			SpeedMbps:   speed / 1e6,
			Sessions:    sessions,
			Config:      string(cfg),
			Port:        extractPort(string(cfg)),
		})
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("节点列表为空")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].SpeedMbps > nodes[j].SpeedMbps })
	return nodes, nil
}

// shuffleStrings 随机打乱字符串切片，用于镜像站轮换。
func shuffleStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	rand.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out
}