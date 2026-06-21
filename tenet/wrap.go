package tenet

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
)

const defaultProxyURL = "https://inference.trytenet.ai"

// Config holds settings for the Tenet proxy transport.
type Config struct {
	TenetKey string
	ProxyURL string
	Failover bool
	Timeout  time.Duration
}

type tenetTransport struct {
	inner    http.RoundTripper
	tenetKey string
	proxyURL string
	failover bool
	callerID atomic.Value
}

// WrapHTTPClient returns a new *http.Client whose transport routes requests
// through the Tenet inference proxy, injecting auth headers and supporting
// optional failover to the original endpoint on 5xx responses.
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
		inner:    inner,
		tenetKey: config.TenetKey,
		proxyURL: proxyURL,
		failover: config.Failover,
	}

	return &http.Client{
		Transport: t,
		Timeout:   client.Timeout,
	}
}

// SetCallerID stores a caller ID on the wrapped client for sticky A/B routing.
// Safe to call from multiple goroutines.
func SetCallerID(client *http.Client, id string) {
	if t, ok := client.Transport.(*tenetTransport); ok {
		t.callerID.Store(id)
	}
}

// ClearCallerID removes the caller ID so requests use per-request weighted routing.
func ClearCallerID(client *http.Client) {
	if t, ok := client.Transport.(*tenetTransport); ok {
		t.callerID.Store("")
	}
}

func (t *tenetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	originalURL := req.URL.String()

	// Buffer the body so it can be replayed on failover.
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

	if id, ok := t.callerID.Load().(string); ok && id != "" {
		proxyReq.Header.Set("X-Caller-ID", id)
	}

	resp, err := t.inner.RoundTrip(proxyReq)
	if err != nil {
		if t.failover {
			t.reportTelemetry(err.Error())
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
		fallbackReq := req.Clone(req.Context())
		if len(bodyBytes) > 0 {
			fallbackReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			fallbackReq.ContentLength = int64(len(bodyBytes))
		}
		return t.inner.RoundTrip(fallbackReq)
	}

	return resp, nil
}

func (t *tenetTransport) reportTelemetry(errMsg string) {
	callerID, _ := t.callerID.Load().(string)
	body, _ := json.Marshal(map[string]string{
		"type":      "failover",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"caller_id": callerID,
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
