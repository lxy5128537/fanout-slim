package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 出口地理位置信息。
type Geo struct {
	IP        string
	Country   string // ISO 两位国家代码（CN / US / JP ...）
	Class     GeoClass
	Detected  bool // 是否探测成功
}

// GeoClass 运行地区分类。
type GeoClass int

const (
	GeoCN        GeoClass = iota // 国内（中国大陆）
	GeoOverseas                  // 海外
	GeoUnknown                   // 探测失败，默认按海外处理
)

func (g GeoClass) IsCN() bool        { return g == GeoCN }
func (g GeoClass) IsOverseas() bool  { return g == GeoOverseas }
func (g GeoClass) Label() string     { return [...]string{"中国", "海外", "未知"}[g] }

// 探测优先级：每个源的 URL、字段名。
// 字段名对应响应 JSON 中的 country 字段（不区分大小写）。
type ipAPI struct {
	url     string
	field   string
	timeout time.Duration
}

var ipAPIs = []ipAPI{
	// 国内通常能直连（返回 JSON 含 country），也是"能联网"的最强信号
	{"https://ip-api.com/json/?fields=country", "country", 6 * time.Second},
	// 国内可达，只返回纯 IP
	{"http://myip.ipip.net", "", 6 * time.Second},
	{"http://ip.ipip.net", "", 6 * time.Second},
	{"http://ifconfig.me", "", 6 * time.Second},
	{"http://httpbin.org/ip", "origin", 8 * time.Second},
	// 海外可直连；国内需代理
	{"https://api.ip.sb/geoip", "country", 8 * time.Second},
	{"https://ipapi.co/json/", "country_code", 8 * time.Second},
	{"https://api.ipify.org", "", 8 * time.Second},
}

// cacheFile 本地缓存文件路径，避免每次启动都探测。
const cacheFile = "/root/.fanout-geo-cache"
const cacheTTL = 12 * time.Hour

// Detect 探测本机出口 IP 所在地区。
//
// 行为：
//  - 优先读本地缓存（12h 内有效）；
//  - 使用 FANOUT_PROXY 环境变量指定的代理（SOCKS5 或 HTTP/HTTPS）；
//  - 依次尝试多个 IP API，第一个返回 CN 即判定国内；
//  - 全部失败时通过"可达性"判断（能直连 ipify → 海外），否则默认海外；
//  - FANOUT_GEO_OVERRIDE=cn|overseas 环境变量强制指定，跳过探测。
func Detect() Geo {
	// 1. 环境变量强制覆盖
	v := strings.ToLower(strings.TrimSpace(os.Getenv("FANOUT_GEO_OVERRIDE")))
	if v != "" {
		switch v {
		case "cn", "china", "domestic":
			log.Println("运行环境: 中国 (FANOUT_GEO_OVERRIDE=cn)")
			return Geo{Class: GeoCN, Detected: true}
		case "overseas", "foreign", "abroad":
			log.Println("运行环境: 海外 (FANOUT_GEO_OVERRIDE=overseas)")
			return Geo{Class: GeoOverseas, Detected: true}
		}
	}

	// 2. 尝试读缓存
	if g, ok := loadCache(); ok {
		log.Printf("运行环境: %s (本地缓存, IP %s)", g.Class.Label(), g.IP)
		return g
	}

	// 3. 在线探测
	proxyStr := strings.TrimSpace(os.Getenv("FANOUT_PROXY"))
	transport := buildTransport(proxyStr)
	defer transport.CloseIdleConnections()

	for _, api := range ipAPIs {
		g, ok := tryOneAPI(api, transport)
		if !ok {
			continue
		}
		if g.Country == "CN" || strings.EqualFold(g.Country, "CN") {
			log.Printf("运行环境: 中国 (出口 IP %s, 来源 %s)", g.IP, api.url)
			g.Class = GeoCN
			g.Detected = true
			saveCache(g)
			return g
		}
		// 拿到了海外国家代码 → 海外
		if g.Country != "" && g.Country != "CN" {
			log.Printf("运行环境: 海外 (%s, 出口 IP %s, 来源 %s)", g.Country, g.IP, api.url)
			g.Class = GeoOverseas
			g.Detected = true
			saveCache(g)
			return g
		}
		// 只有 IP 没有国家代码（ipify 场景）：标记但继续
		if g.IP != "" {
			log.Printf("获得出口 IP %s，但未获国家代码，继续尝试", g.IP)
		}
	}

	// 4. 全部 API 失败 → 通过"可达性"判断
	// 只对"海外独有"的服务做可达性判断：国内被墙、海外可达
	// → 可达 = 海外；不可达 = 国内（无代理）
	log.Println("所有 IP API 探测失败，尝试可达性判断...")
	if canReachDirect(overseasOnlyURLs) {
		log.Println("运行环境: 海外 (外部 IP API 直连可达, 无法获知具体国家)")
		g := Geo{Class: GeoOverseas, Detected: false}
		saveCache(g)
		return g
	}

	// 5. 连海外独有服务也直连不了 → 国内（无代理）
	log.Println("运行环境: 中国 (外部 IP API 均不可达, 默认按国内处理)")
	g := Geo{Class: GeoCN, Detected: false}
	saveCache(g)
	return g
}

// tryOneAPI 尝试调用单个 IP API 并解析 country 字段。
func tryOneAPI(api ipAPI, transport *http.Transport) (Geo, bool) {
	client := &http.Client{Timeout: api.timeout, Transport: transport}
	resp, err := client.Get(api.url)
	if err != nil {
		return Geo{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Geo{}, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if err != nil {
		return Geo{}, false
	}

	if api.field == "" {
		// 只返回 IP 的 API（如 ipify）
		ip := strings.TrimSpace(string(body))
		if net.ParseIP(ip) != nil {
			return Geo{IP: ip}, true
		}
		return Geo{}, false
	}

	// 解析 JSON，找 country 字段（不区分大小写）
	var raw map[string]interface{}
	if json.Unmarshal(body, &raw) != nil {
		return Geo{}, false
	}
	ip, _ := raw["ip"].(string)
	if ip == "" {
		ip, _ = raw["query"].(string)
	}
	var country string
	for k, v := range raw {
		if strings.EqualFold(k, api.field) {
			country = fmt.Sprintf("%v", v)
			break
		}
	}
	return Geo{IP: ip, Country: country}, true
}

// overseasOnlyURLs 是"可达性判断"尝试的 URL。
// 这些服务在海外可达、在国内被墙，用作海外信号的探照灯。
// 全部不可达 → 判断为国内（无代理）。
var overseasOnlyURLs = []string{
	"https://api.ipify.org",
	"https://api.ip.sb/geoip",
	"https://ipapi.co/json/",
}

// canReachDirect 用**无代理**的传输直连目标 URL，判断可达性。
// 用于兜底：依次尝试多个国内/海外都可达的服务，任一可达即返回 true。
func canReachDirect(urls []string) bool {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			DualStack: false,
		}).DialContext,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 6 * time.Second, Transport: transport}
	for _, u := range urls {
		resp, err := client.Get(u)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

// buildTransport 根据代理字符串构造 http.Transport。
// proxyStr 支持: socks5://host:port, http://host:port, https://host:port, [user:pass@]host:port
func buildTransport(proxyStr string) *http.Transport {
	if proxyStr == "" {
		return &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   8 * time.Second,
				DualStack: false,
			}).DialContext,
		}
	}

	proxyURL, err := url.Parse(proxyStr)
	if err != nil {
		log.Printf("FANOUT_PROXY 格式错误 (%v)，忽略", err)
		proxyURL = nil
	}

	return &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: (&net.Dialer{
			Timeout:   8 * time.Second,
			DualStack: false,
		}).DialContext,
	}
}

// loadCache 读取本地缓存，返回 (Geo, 是否有效)。
func loadCache() (Geo, bool) {
	data, err := os.ReadFile(cacheFile)
	if err != nil {
		return Geo{}, false
	}
	type cache struct {
		TS      int64  `json:"ts"`
		IP      string `json:"ip"`
		Country string `json:"country"`
		Class   int    `json:"class"`
	}
	var c cache
	if json.Unmarshal(data, &c) != nil {
		return Geo{}, false
	}
	if time.Since(time.Unix(c.TS, 0)) > cacheTTL {
		return Geo{}, false
	}
	g := Geo{
		IP:      c.IP,
		Country: c.Country,
		Class:   GeoClass(c.Class),
	}
	return g, true
}

// saveCache 写本地缓存。
func saveCache(g Geo) {
	data, err := json.Marshal(struct {
		TS      int64  `json:"ts"`
		IP      string `json:"ip"`
		Country string `json:"country"`
		Class   int    `json:"class"`
	}{
		TS:      time.Now().Unix(),
		IP:      g.IP,
		Country: g.Country,
		Class:   int(g.Class),
	})
	if err != nil {
		return
	}
	os.WriteFile(cacheFile, data, 0600) // 忽略错误，缓存非关键
}