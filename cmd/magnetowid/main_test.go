package main

import (
	"net/http"
	"strings"
	"testing"
)

func TestHealthURL(t *testing.T) {
	for listen, want := range map[string]string{
		":8484":               "http://127.0.0.1:8484/health",
		"0.0.0.0:8484":        "http://127.0.0.1:8484/health",
		"[::]:8484":           "http://[::1]:8484/health",
		"127.0.0.1:9000":      "http://127.0.0.1:9000/health",
		"[fd00::5]:8484":      "http://[fd00::5]:8484/health",
		"[fe80::1%eth0]:8484": "http://[fe80::1%25eth0]:8484/health",
		"magnetowid:8484":     "http://magnetowid:8484/health",
	} {
		if got, err := healthURL(listen); err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
	if _, err := healthURL("8484"); err == nil {
		t.Error("healthURL accepted a listen address without a port")
	}
}

func TestSiteClient(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "https://vod.example/playlist.m3u8", nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := func(proxy string) *http.Transport {
		t.Helper()
		c, err := siteClient(proxy)
		if err != nil {
			t.Fatalf("siteClient(%q): %v", proxy, err)
		}
		return c.Transport.(*http.Transport)
	}

	// Unset, the environment decides host by host, for ffmpeg too: a transport
	// here would make the downloader settle ffmpeg's proxy from one URL.
	if c, err := siteClient(""); err != nil || c.Transport != nil || c.Timeout == 0 {
		t.Errorf("siteClient(\"\") = %+v, %v; want a client without a transport", c, err)
	}
	if transport("direct").Proxy != nil {
		t.Error(`"direct" still uses a proxy`)
	}
	for proxy, name := range map[string]string{
		"http://proxy.example:3128":             "http://proxy.example:3128",
		"http://user:secret@proxy.example:3128": "http://user:xxxxx@proxy.example:3128",
	} {
		u, err := transport(proxy).Proxy(req)
		if err != nil || u == nil || u.String() != proxy {
			t.Errorf("siteClient(%q) proxies through %v, %v", proxy, u, err)
		}
		if got := proxyName(proxy); got != name {
			t.Errorf("proxyName(%q) = %q, want %q", proxy, got, name)
		}
	}
	if a, b := transport("direct"), transport("direct"); a == b {
		t.Error("sites share a transport")
	}

	// ffmpeg tunnels through HTTP proxies only.
	for _, proxy := range []string{
		"socks5://user:secret@proxy.example:1080",
		"https://proxy.example:3128",
		"proxy.example:3128",
		"http://",
		"http://[proxy",
		"DIRECT",
	} {
		_, err := siteClient(proxy)
		if err == nil {
			t.Errorf("siteClient(%q) accepted the proxy", proxy)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("siteClient(%q) reports the password: %v", proxy, err)
		}
	}
	for proxy, want := range map[string]string{"": "environment", "direct": "direct"} {
		if got := proxyName(proxy); got != want {
			t.Errorf("proxyName(%q) = %q, want %q", proxy, got, want)
		}
	}
}
