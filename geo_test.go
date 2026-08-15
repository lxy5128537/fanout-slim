package main

import (
	"os"
	"testing"
)

func TestDetectOverride(t *testing.T) {
	tests := []struct {
		name  string
		env   string
		want  GeoClass
	}{
		{"cn", "cn", GeoCN},
		{"china", "china", GeoCN},
		{"domestic", "domestic", GeoCN},
		{"overseas", "overseas", GeoOverseas},
		{"foreign", "foreign", GeoOverseas},
		{"abroad", "abroad", GeoOverseas},
		{"cn_upper", "CN", GeoCN},
	}

	// 清缓存，避免干扰
	os.Remove(cacheFile)
	defer os.Remove(cacheFile)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("FANOUT_GEO_OVERRIDE", tt.env)
			g := Detect()
			os.Unsetenv("FANOUT_GEO_OVERRIDE")
			if g.Class != tt.want {
				t.Fatalf("env=%q got class %d, want %d", tt.env, g.Class, tt.want)
			}
			if !g.Detected {
				t.Fatalf("env=%q: Detected should be true", tt.env)
			}
		})
	}
}

func TestDetectDefaultWhenEmptyEnv(t *testing.T) {
	os.Unsetenv("FANOUT_GEO_OVERRIDE")
	os.Remove(cacheFile)

	// 无代理、无缓存 → 探测走 API，失败后走可达性判断
	// 最终一定是 CN / Overseas / Unknown 之一
	g := Detect()
	valid := g.Class == GeoCN || g.Class == GeoOverseas || g.Class == GeoUnknown
	if !valid {
		t.Fatalf("unexpected class: %d", g.Class)
	}
}

func TestBuildTransport(t *testing.T) {
	tr := buildTransport("socks5://127.0.0.1:1080")
	if tr == nil {
		t.Fatal("transport nil")
	}
}

func TestLoadCache(t *testing.T) {
	os.Remove(cacheFile)

	// 写一个缓存
	saveCache(Geo{IP: "1.2.3.4", Country: "CN", Class: GeoCN, Detected: true})
	g, ok := loadCache()
	if !ok {
		t.Fatal("cache not loaded")
	}
	if g.IP != "1.2.3.4" || g.Country != "CN" || g.Class != GeoCN {
		t.Fatalf("cache mismatch: %+v", g)
	}

	os.Remove(cacheFile)
}

func TestGeoClassMethods(t *testing.T) {
	if !GeoCN.IsCN() { t.Fatal("CN.IsCN") }
	if !GeoOverseas.IsOverseas() { t.Fatal("Overseas.IsOverseas") }
	if GeoCN.IsOverseas() { t.Fatal("CN.IsOverseas") }
	if GeoUnknown.Label() != "未知" { t.Fatal("Unknown.Label") }
}