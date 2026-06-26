package tenet

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseAttribution(t *testing.T) {
	h := http.Header{}
	h.Set("X-Tenet-Mode", "replacement")
	h.Set("X-Tenet-Served-Variant", "dominos-qwen3-bf16")
	h.Set("X-Tenet-Matched-Profile", "dominos-v1")
	h.Set("X-Tenet-Fallback-Used", "true")

	a := parseAttribution(h)
	if a.Mode != "replacement" || a.ServedVariant != "dominos-qwen3-bf16" ||
		a.MatchedProfile != "dominos-v1" || !a.FallbackUsed || a.ServedDirect {
		t.Errorf("unexpected attribution: %+v", a)
	}
}

func TestParseAttribution_Empty(t *testing.T) {
	a := parseAttribution(http.Header{})
	if a.Mode != "" || a.ServedVariant != "" || a.FallbackUsed || a.ServedDirect {
		t.Errorf("expected zero attribution, got %+v", a)
	}
}

func TestOnAttribution_ProxySuccess(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Tenet-Mode", "passthrough")
		w.Header().Set("X-Tenet-Served-Variant", "passthrough")
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer proxy.Close()

	var got Attribution
	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk", ProxyURL: proxy.URL,
		OnAttribution: func(a Attribution) { got = a },
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Mode != "passthrough" || got.ServedVariant != "passthrough" || got.ServedDirect {
		t.Errorf("expected passthrough attribution, got %+v", got)
	}
}

func TestOnAttribution_FailoverServedDirect(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503) // force failover
	}))
	defer proxy.Close()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer provider.Close()

	var got Attribution
	calls := 0
	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk", ProxyURL: proxy.URL, Failover: true,
		OnAttribution: func(a Attribution) { calls++; got = a },
	})

	req, _ := http.NewRequest("POST", provider.URL+"/v1/chat/completions", strings.NewReader(`{}`))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if calls != 1 || !got.ServedDirect {
		t.Errorf("expected one ServedDirect callback, got calls=%d attr=%+v", calls, got)
	}
}
