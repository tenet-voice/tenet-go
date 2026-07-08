package tenet

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRewritesURL(t *testing.T) {
	var gotURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	client.Do(req)

	if gotURL != "/v1/chat/completions" {
		t.Errorf("expected /v1/chat/completions, got %s", gotURL)
	}
}

func TestInjectsTenetKey(t *testing.T) {
	var gotHeader string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenet-Key")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	client.Do(req)

	if gotHeader != "tk_xxx" {
		t.Errorf("expected tk_xxx, got %s", gotHeader)
	}
}

func TestPreservesAuth(t *testing.T) {
	var gotAuth string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_provider")
	client.Do(req)

	if gotAuth != "Bearer sk_provider" {
		t.Errorf("expected Bearer sk_provider, got %s", gotAuth)
	}
}

func TestInjectsProviderURL(t *testing.T) {
	var gotHeader string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Provider-URL")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
	})

	req, _ := http.NewRequest("POST", "https://api.groq.com/openai/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	client.Do(req)

	if gotHeader != "https://api.groq.com/openai/v1/chat/completions" {
		t.Errorf("expected groq URL, got %s", gotHeader)
	}
}

func TestInjectsSessionID(t *testing.T) {
	var gotHeader string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenet-Session-Id")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
	})
	SetSessionID(client, "caller_123")

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	client.Do(req)

	if gotHeader != "caller_123" {
		t.Errorf("expected caller_123, got %s", gotHeader)
	}
}

func TestInjectsSessionTags(t *testing.T) {
	var gotHeader string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Tenet-Session-Tags")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
	})
	SetSessionTags(client, []string{"beta", "internal"})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	client.Do(req)

	if gotHeader != "beta,internal" {
		t.Errorf("expected beta,internal, got %s", gotHeader)
	}
}

func TestNoSessionIDWhenUnset(t *testing.T) {
	var hasHeader bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasHeader = r.Header["X-Tenet-Session-Id"]
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	client.Do(req)

	if hasHeader {
		t.Error("expected no X-Tenet-Session-Id header")
	}
}

func TestFailoverOn5xx(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer proxy.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"fallback"}}]}`))
	}))
	defer fallback.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
		Failover: true,
	})

	req, _ := http.NewRequest("POST", fallback.URL+"/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "fallback") {
		t.Errorf("expected fallback response, got %s", string(body))
	}
}

func TestNoFailoverOn4xx(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer proxy.Close()

	client := WrapHTTPClient(http.DefaultClient, Config{
		TenetKey: "tk_xxx",
		ProxyURL: proxy.URL,
		Failover: true,
	})

	req, _ := http.NewRequest("POST", "https://api.openai.com/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer sk_xxx")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	if resp.StatusCode != 401 {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}
