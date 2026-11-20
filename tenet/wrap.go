// Package tenet sends an OpenAI-compatible client's chat completions to a
// Tenet Project, and fetches the Project's prompts.
//
// Wrap the HTTP client of any [github.com/openai/openai-go] client. The client
// keeps its own provider, key and model; those are the failover:
//
//	client := openai.NewClient(
//		option.WithAPIKey(os.Getenv("OPENAI_API_KEY")),
//		option.WithHTTPClient(tenet.WrapHTTPClient(http.DefaultClient, tenet.Config{
//			Key:      os.Getenv("TENET_API_KEY"),
//			Project:  "support-agent",
//			Failover: true,
//		})),
//	)
//
// Every chat completion, streaming included, goes to Tenet as model
// "<Project>[:<Tag>]". Tenet serves the version the tag names, and its own
// fallback model when that version fails. With Failover set, a request Tenet
// cannot answer (network error or 5xx) is sent unchanged to the provider.
// Requests other than chat completions go to the provider directly.
package tenet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const defaultBaseURL = "https://inference.trytenet.ai"

// name is what a Project ID or tag may hold in a model name; Tenet checks
// the rest.
var name = regexp.MustCompile(`^[^\s:/]+$`)

// Config holds settings for the Tenet transport.
type Config struct {
	// Key is the Organization's Tenet key (sk-tnt-…).
	Key string

	// Project is the Tenet Project requests are sent to.
	Project string

	// Tag names the Project version to serve: production (the default),
	// latest, a branch, a custom tag, or vN.
	Tag string

	// BaseURL overrides the Tenet endpoint (https://inference.trytenet.ai),
	// for example to use staging.
	BaseURL string

	// Failover sends a request unchanged to the provider when Tenet is
	// unreachable or returns a 5xx.
	Failover bool

	// Timeout sets the HTTP client timeout. Zero keeps the wrapped client's.
	Timeout time.Duration

	// AgentID, SessionID and SessionTags are recorded with captured traffic
	// for analysis. They do not change which version serves.
	AgentID     string
	SessionID   string
	SessionTags []string

	// OnAttribution, if set, is called once per chat completion with how it
	// was served.
	OnAttribution func(Attribution)
}

// Attribution describes how a chat completion was served.
type Attribution struct {
	// Version is the version that served it as "<project>:v<N>", from
	// X-Tenet-Served-Version; empty when Tenet's fallback model did.
	Version string
	// FallbackUsed reports that Tenet served it with the Project's fallback.
	FallbackUsed bool
	// ServedDirect reports that the SDK failed over to the provider.
	ServedDirect bool
}

func parseAttribution(h http.Header) Attribution {
	return Attribution{
		Version:      h.Get("X-Tenet-Served-Version"),
		FallbackUsed: h.Get("X-Tenet-Fallback-Used") == "true",
	}
}

// Model is the model name a chat completion is sent to Tenet with.
func (c Config) Model() string {
	if c.Tag == "" {
		return c.Project
	}
	return c.Project + ":" + c.Tag
}

func (c Config) baseURL() string {
	if c.BaseURL == "" {
		return defaultBaseURL
	}
	return strings.TrimRight(c.BaseURL, "/")
}

// ValidateConfig checks a Config before its values reach URLs and headers.
func ValidateConfig(config Config) error {
	if !strings.HasPrefix(strings.TrimSpace(config.Key), "sk-tnt-") {
		return fmt.Errorf("tenet: Key must be a Tenet key (sk-tnt-…)")
	}
	if !name.MatchString(config.Project) {
		return fmt.Errorf("tenet: Project is required and cannot contain spaces, ':' or '/'")
	}
	if config.Tag != "" && !name.MatchString(config.Tag) {
		return fmt.Errorf("tenet: Tag cannot contain spaces, ':' or '/'")
	}
	if config.Timeout < 0 {
		return fmt.Errorf("tenet: Timeout cannot be negative")
	}
	if err := checkHeaderValue("AgentID", config.AgentID); err != nil {
		return err
	}
	if err := checkHeaderValue("SessionID", config.SessionID); err != nil {
		return err
	}
	if err := checkSessionTags(config.SessionTags); err != nil {
		return err
	}
	parsed, err := url.Parse(config.baseURL())
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("tenet: BaseURL must be an absolute HTTP(S) URL")
	}
	return nil
}

func checkHeaderValue(field, value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("tenet: %s cannot contain line breaks", field)
	}
	return nil
}

func checkSessionTags(tags []string) error {
	for i, tag := range tags {
		if strings.TrimSpace(tag) == "" {
			return fmt.Errorf("tenet: SessionTags[%d] cannot be empty", i)
		}
		if strings.ContainsAny(tag, ",\r\n") {
			return fmt.Errorf("tenet: SessionTags[%d] cannot contain commas or line breaks", i)
		}
	}
	return nil
}

type transport struct {
	inner         http.RoundTripper
	config        Config
	configErr     error
	agentID       atomic.Value
	sessionID     atomic.Value
	sessionTags   atomic.Value
	onAttribution func(Attribution)
}

// WrapHTTPClient returns a new [http.Client] that sends chat completions to
// the Tenet Project. The wrapped client's transport makes the underlying
// calls, Tenet's and the provider's.
func WrapHTTPClient(client *http.Client, config Config) *http.Client {
	inner := client.Transport
	if inner == nil {
		inner = http.DefaultTransport
	}
	t := &transport{
		inner:         inner,
		config:        config,
		configErr:     ValidateConfig(config),
		onAttribution: config.OnAttribution,
	}
	t.agentID.Store(config.AgentID)
	t.sessionID.Store(config.SessionID)
	t.sessionTags.Store(config.SessionTags)

	timeout := client.Timeout
	if config.Timeout > 0 {
		timeout = config.Timeout
	}
	return &http.Client{Transport: t, Timeout: timeout, CheckRedirect: client.CheckRedirect, Jar: client.Jar}
}

// SetAgentID records an agent identifier with subsequent requests. Safe for
// concurrent use.
func SetAgentID(client *http.Client, id string) {
	if t, ok := client.Transport.(*transport); ok {
		t.agentID.Store(id)
	}
}

// SetSessionID records a session identifier with subsequent requests, so
// Tenet groups them into one session. Safe for concurrent use.
func SetSessionID(client *http.Client, id string) {
	if t, ok := client.Transport.(*transport); ok {
		t.sessionID.Store(id)
	}
}

// SetSessionTags records tags with subsequent requests' session. Safe for
// concurrent use.
func SetSessionTags(client *http.Client, tags []string) {
	if t, ok := client.Transport.(*transport); ok {
		t.sessionTags.Store(append([]string(nil), tags...))
	}
}

func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.configErr != nil {
		return nil, t.configErr
	}
	if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/chat/completions") {
		return t.inner.RoundTrip(req)
	}

	var original []byte
	if req.Body != nil && req.Body != http.NoBody {
		var err error
		original, err = io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	body, err := withModel(original, t.config.Model())
	if err != nil {
		return nil, err
	}
	tenetReq, err := t.tenetRequest(req, body)
	if err != nil {
		return nil, err
	}

	resp, err := t.inner.RoundTrip(tenetReq)
	switch {
	case err != nil && t.failover():
		return t.direct(req, original, err.Error())
	case err != nil:
		return nil, err
	case resp.StatusCode >= 500 && t.failover():
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return t.direct(req, original, "tenet returned "+resp.Status)
	}
	t.emit(parseAttribution(resp.Header))
	return resp, nil
}

func (t *transport) failover() bool { return t.config.Failover }

// tenetRequest is req sent to Tenet's chat completions endpoint with the
// Tenet key in place of the provider's.
func (t *transport) tenetRequest(req *http.Request, body []byte) (*http.Request, error) {
	out, err := http.NewRequestWithContext(req.Context(), http.MethodPost, t.config.baseURL()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, values := range req.Header {
		if strings.EqualFold(key, "Authorization") || strings.EqualFold(key, "Api-Key") {
			continue
		}
		out.Header[key] = values
	}
	out.Header.Set("Authorization", "Bearer "+strings.TrimSpace(t.config.Key))
	out.Header.Set("Content-Type", "application/json")
	if id, _ := t.agentID.Load().(string); id != "" {
		out.Header.Set("X-Tenet-Agent-ID", id)
	}
	if id, _ := t.sessionID.Load().(string); id != "" {
		out.Header.Set("X-Tenet-Session-Id", id)
	}
	if tags, _ := t.sessionTags.Load().([]string); len(tags) > 0 && checkSessionTags(tags) == nil {
		out.Header.Set("X-Tenet-Session-Tags", strings.Join(tags, ","))
	}
	return out, nil
}

// direct sends the original request to the provider.
func (t *transport) direct(req *http.Request, original []byte, reason string) (*http.Response, error) {
	t.reportFailover(reason)
	t.emit(Attribution{ServedDirect: true})
	out := req.Clone(req.Context())
	out.Body = io.NopCloser(bytes.NewReader(original))
	out.ContentLength = int64(len(original))
	return t.inner.RoundTrip(out)
}

// withModel is a chat completion body with its model replaced; every other
// field is kept as sent.
func withModel(body []byte, model string) ([]byte, error) {
	fields := map[string]json.RawMessage{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &fields); err != nil {
			return nil, fmt.Errorf("tenet: chat completion body is not a JSON object: %w", err)
		}
	}
	encoded, _ := json.Marshal(model)
	fields["model"] = encoded
	return json.Marshal(fields)
}

func (t *transport) emit(a Attribution) {
	if t.onAttribution != nil {
		t.onAttribution(a)
	}
}

// reportFailover tells Tenet, in the background, that a request bypassed it.
func (t *transport) reportFailover(reason string) {
	sessionID, _ := t.sessionID.Load().(string)
	body, _ := json.Marshal(map[string]string{
		"type":      "failover",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"caller_id": sessionID,
		"error":     reason,
	})
	go func() {
		req, err := http.NewRequest(http.MethodPost, t.config.baseURL()+"/v1/telemetry", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(t.config.Key))
		req.Header.Set("Content-Type", "application/json")
		resp, err := t.inner.RoundTrip(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
}
