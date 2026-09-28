package main

import "testing"

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
