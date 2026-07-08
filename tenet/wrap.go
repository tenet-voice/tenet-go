// Package tenet routes OpenAI-compatible HTTP requests through the Tenet
// inference proxy for production observability, A/B model routing, and
// automatic failover.
//
// Usage is a single-line change to any [github.com/openai/openai-go] client:
//
//	client := openai.NewClient(
//		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
//		option.WithHTTPClient(tenet.WrapHTTPClient(http.DefaultClient, tenet.Config{
//			TenetKey:    os.Getenv("TENET_API_KEY"),
//			SessionID:   "caller_123",
//			SessionTags: []string{"beta", "internal"},
//		})),
//	)
//
// All requests — including streaming — are transparently proxied. If the proxy
// is unreachable or returns a 5xx, the SDK falls back to calling the provider
// directly so your agent never goes silent.
package tenet

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const defaultProxyURL = "https://inference.trytenet.ai"

// Config holds settings for the Tenet proxy transport.
type Config struct {
	// TenetKey authenticates requests to the Tenet proxy.
	TenetKey string

	// SessionID identifies the caller/session for sticky A/B routing. The
	// proxy uses this to ensure the same session always hits the same model
	// variant (via FNV-1a hash).
	SessionID string

	// SessionTags attaches cohort tags to the session (e.g. for cohort-based
	// routing or analysis).
	SessionTags []string

	// ProxyURL overrides the default proxy endpoint (https://inference.trytenet.ai).
	// Use this for self-hosted or staging deployments.
	ProxyURL string

	// Failover controls whether the SDK falls back to calling the provider
	// directly when the proxy is unreachable or returns a 5xx. Default is false;
	// set to true for production resilience.
	Failover bool

	// Timeout sets the HTTP client timeout. Zero means no timeout.
	Timeout time.Duration

	// OnAttribution, if set, is called once per request with how the proxy
	// served it. On client-side failover it is called with ServedDirect:true.
	OnAttribution func(Attribution)
}

// Attribution describes how the proxy served a request, parsed from the
// X-Tenet-* response headers. ServedDirect is set by the SDK (not a header)
// when it failed over to the provider directly, bypassing Tenet.
type Attribution struct {
	Mode           string // "passthrough" | "replacement" | "managed"
	ServedVariant  string
	MatchedProfile string
	FallbackUsed   bool
	ServedDirect   bool
}

func parseAttribution(h http.Header) Attribution {
	return Attribution{
		Mode:           h.Get("X-Tenet-Mode"),
		ServedVariant:  h.Get("X-Tenet-Served-Variant"),
		MatchedProfile: h.Get("X-Tenet-Matched-Profile"),
		FallbackUsed:   h.Get("X-Tenet-Fallback-Used") == "true",
	}
}

type tenetTransport struct {
	inner         http.RoundTripper
	tenetKey      string
	proxyURL      string
	failover      bool
	sessionID     atomic.Value
	sessionTags   atomic.Value
	onAttribution func(Attribution)
}

// WrapHTTPClient returns a new [http.Client] whose transport routes requests
// through the Tenet inference proxy. The original client's transport is
// preserved and used for the underlying HTTP calls (and for failover).
//
// Pass the returned client to [github.com/openai/openai-go/option.WithHTTPClient]
// or use it directly — any OpenAI-compatible HTTP request is supported.
func WrapHTTPClient(client *http.Client, config Config) *http.Client {
	proxyURL := config.ProxyURL
	if proxyURL == "" {
		proxyURL = defaultProxyURL
	}

	inner := client.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}

	t := &tenetTransport{
		inner:         inner,
		tenetKey:      config.TenetKey,
		proxyURL:      proxyURL,
		failover:      config.Failover,
		onAttribution: config.OnAttribution,
	}
	t.sessionID.Store(config.SessionID)
	t.sessionTags.Store(config.SessionTags)

	return &http.Client{
		Transport: t,
		Timeout:   client.Timeout,
	}
}

// SetSessionID attaches a session identifier to the wrapped client for sticky
// A/B routing. The proxy uses this to ensure the same session always hits the
// same model variant (via FNV-1a hash). Safe for concurrent use.
func SetSessionID(client *http.Client, id string) {
	if t, ok := client.Transport.(*tenetTransport); ok {
		t.sessionID.Store(id)
	}
}

// ClearSessionID removes the session identifier so subsequent requests use
// per-request weighted-random routing instead of sticky assignment.
func ClearSessionID(client *http.Client) {
	if t, ok := client.Transport.(*tenetTransport); ok {
		t.sessionID.Store("")
	}
}

// SetSessionTags attaches cohort tags to the wrapped client's session. Safe
// for concurrent use.
func SetSessionTags(client *http.Client, tags []string) {
	if t, ok := client.Transport.(*tenetTransport); ok {
		t.sessionTags.Store(tags)
	}
}

// ClearSessionTags removes the session tags.
func ClearSessionTags(client *http.Client) {
	if t, ok := client.Transport.(*tenetTransport); ok {
		t.sessionTags.Store([]string{})
	}
}

func (t *tenetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	originalURL := req.URL.String()

	var bodyBytes []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		bodyBytes, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}

	proxyParsed, _ := url.Parse(t.proxyURL)
	proxyReq := req.Clone(req.Context())
	proxyReq.URL.Scheme = proxyParsed.Scheme
	proxyReq.URL.Host = proxyParsed.Host
	proxyReq.Host = proxyParsed.Host
	if len(bodyBytes) > 0 {
		proxyReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		proxyReq.ContentLength = int64(len(bodyBytes))
	}

	proxyReq.Header.Set("X-Tenet-Key", t.tenetKey)
	proxyReq.Header.Set("X-Provider-URL", originalURL)

	if id, ok := t.sessionID.Load().(string); ok && id != "" {
		proxyReq.Header.Set("X-Tenet-Session-Id", id)
	}
	if tags, ok := t.sessionTags.Load().([]string); ok && len(tags) > 0 {
		proxyReq.Header.Set("X-Tenet-Session-Tags", strings.Join(tags, ","))
	}

	resp, err := t.inner.RoundTrip(proxyReq)
	if err != nil {
		if t.failover {
			t.reportTelemetry(err.Error())
			t.emitAttribution(Attribution{ServedDirect: true})
			fallbackReq := req.Clone(req.Context())
			if len(bodyBytes) > 0 {
				fallbackReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				fallbackReq.ContentLength = int64(len(bodyBytes))
			}
			return t.inner.RoundTrip(fallbackReq)
		}
		return nil, err
	}

	if resp.StatusCode >= 500 && t.failover {
		resp.Body.Close()
		t.reportTelemetry("proxy returned " + resp.Status)
		t.emitAttribution(Attribution{ServedDirect: true})
		fallbackReq := req.Clone(req.Context())
		if len(bodyBytes) > 0 {
			fallbackReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			fallbackReq.ContentLength = int64(len(bodyBytes))
		}
		return t.inner.RoundTrip(fallbackReq)
	}

	t.emitAttribution(parseAttribution(resp.Header))
	return resp, nil
}

func (t *tenetTransport) emitAttribution(a Attribution) {
	if t.onAttribution != nil {
		t.onAttribution(a)
	}
}

func (t *tenetTransport) reportTelemetry(errMsg string) {
	sessionID, _ := t.sessionID.Load().(string)
	body, _ := json.Marshal(map[string]string{
		"type":      "failover",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"caller_id": sessionID,
		"error":     errMsg,
	})

	go func() {
		req, _ := http.NewRequest("POST", t.proxyURL+"/v1/telemetry",
			bytes.NewReader(body))
		req.Header.Set("X-Tenet-Key", t.tenetKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := t.inner.RoundTrip(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
}
