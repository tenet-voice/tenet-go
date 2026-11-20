package tenet

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "sk-tnt-test"

type recorded struct {
	path, auth, sessionID, sessionTags, agentID, extra string
	body                                               map[string]any
}

// server records the requests it gets and answers with status and headers.
func server(t *testing.T, status int, headers map[string]string) (*httptest.Server, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		got = append(got, recorded{
			path: r.URL.Path, auth: r.Header.Get("Authorization"),
			sessionID: r.Header.Get("X-Tenet-Session-Id"), sessionTags: r.Header.Get("X-Tenet-Session-Tags"),
			agentID: r.Header.Get("X-Tenet-Agent-ID"), extra: r.Header.Get("X-Extra"), body: body,
		})
		mu.Unlock()
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		io.WriteString(w, `{"choices":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func chat(t *testing.T, client *http.Client, providerURL string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, providerURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4.1","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer sk-provider")
	req.Header.Set("X-Extra", "kept")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestValidateConfig(t *testing.T) {
	valid := Config{Key: testKey, Project: "support-agent"}
	if err := ValidateConfig(valid); err != nil {
		t.Fatalf("ValidateConfig(valid) = %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"provider key":     func(c *Config) { c.Key = "sk-proj-123" },
		"missing project":  func(c *Config) { c.Project = "" },
		"project with tag": func(c *Config) { c.Project = "support:v2" },
		"tag with space":   func(c *Config) { c.Tag = "a b" },
		"bad base URL":     func(c *Config) { c.BaseURL = "not-a-url" },
		"negative timeout": func(c *Config) { c.Timeout = -time.Second },
		"session newline":  func(c *Config) { c.SessionID = "a\nb" },
		"tag with comma":   func(c *Config) { c.SessionTags = []string{"a,b"} },
	} {
		t.Run(name, func(t *testing.T) {
			config := valid
			mutate(&config)
			if ValidateConfig(config) == nil {
				t.Fatal("ValidateConfig() = nil, want an error")
			}
		})
	}
}

func TestChatCompletionGoesToTheProject(t *testing.T) {
	tenet, tenetGot := server(t, http.StatusOK, map[string]string{"X-Tenet-Served-Version": "v7"})
	provider, providerGot := server(t, http.StatusOK, nil)
	var attribution Attribution
	client := WrapHTTPClient(http.DefaultClient, Config{
		Key: testKey, Project: "support-agent", Tag: "canary", BaseURL: tenet.URL,
		SessionID: "call-1", SessionTags: []string{"beta"}, AgentID: "front-desk",
		OnAttribution: func(a Attribution) { attribution = a },
	})
	chat(t, client, provider.URL)

	if len(*providerGot) != 0 {
		t.Fatalf("provider got %d requests, want 0", len(*providerGot))
	}
	got := (*tenetGot)[0]
	if got.path != "/v1/chat/completions" || got.auth != "Bearer "+testKey {
		t.Fatalf("Tenet got path %q auth %q", got.path, got.auth)
	}
	if got.body["model"] != "support-agent:canary" || got.body["stream"] != true || got.body["messages"] == nil {
		t.Fatalf("Tenet got body %v", got.body)
	}
	if got.sessionID != "call-1" || got.sessionTags != "beta" || got.agentID != "front-desk" || got.extra != "kept" {
		t.Fatalf("Tenet got headers %+v", got)
	}
	if attribution != (Attribution{Version: "v7"}) {
		t.Fatalf("attribution = %+v", attribution)
	}
}

func TestBareProjectMeansProduction(t *testing.T) {
	tenet, got := server(t, http.StatusOK, nil)
	client := WrapHTTPClient(http.DefaultClient, Config{Key: testKey, Project: "support-agent", BaseURL: tenet.URL})
	chat(t, client, "http://provider.invalid")
	if model := (*got)[0].body["model"]; model != "support-agent" {
		t.Fatalf("model = %v, want support-agent", model)
	}
}

func TestFailoverSendsTheOriginalRequestToTheProvider(t *testing.T) {
	tenet, _ := server(t, http.StatusBadGateway, nil)
	provider, providerGot := server(t, http.StatusOK, nil)
	var attribution Attribution
	client := WrapHTTPClient(http.DefaultClient, Config{
		Key: testKey, Project: "support-agent", BaseURL: tenet.URL, Failover: true,
		OnAttribution: func(a Attribution) { attribution = a },
	})
	chat(t, client, provider.URL)

	got := (*providerGot)[0]
	if got.auth != "Bearer sk-provider" || got.body["model"] != "gpt-4.1" {
		t.Fatalf("provider got auth %q model %v, want the original request", got.auth, got.body["model"])
	}
	if !attribution.ServedDirect {
		t.Fatalf("attribution = %+v, want ServedDirect", attribution)
	}
}

func TestFailoverOnUnreachableTenet(t *testing.T) {
	provider, providerGot := server(t, http.StatusOK, nil)
	client := WrapHTTPClient(http.DefaultClient, Config{
		Key: testKey, Project: "support-agent", BaseURL: "http://127.0.0.1:1", Failover: true,
	})
	chat(t, client, provider.URL)
	if len(*providerGot) != 1 {
		t.Fatalf("provider got %d requests, want 1", len(*providerGot))
	}
}

func TestNoFailoverReturnsTenetsAnswer(t *testing.T) {
	tenet, _ := server(t, http.StatusBadGateway, nil)
	provider, providerGot := server(t, http.StatusOK, nil)
	client := WrapHTTPClient(http.DefaultClient, Config{Key: testKey, Project: "support-agent", BaseURL: tenet.URL})
	if resp := chat(t, client, provider.URL); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if len(*providerGot) != 0 {
		t.Fatal("provider was called without Failover")
	}
}

func TestClientErrorsAreNotFailedOver(t *testing.T) {
	tenet, _ := server(t, http.StatusNotFound, nil)
	provider, providerGot := server(t, http.StatusOK, nil)
	client := WrapHTTPClient(http.DefaultClient, Config{Key: testKey, Project: "support-agent", BaseURL: tenet.URL, Failover: true})
	if resp := chat(t, client, provider.URL); resp.StatusCode != http.StatusNotFound || len(*providerGot) != 0 {
		t.Fatalf("status = %d, provider calls = %d; want 404 from Tenet", resp.StatusCode, len(*providerGot))
	}
}

func TestOtherRequestsGoToTheProvider(t *testing.T) {
	tenet, tenetGot := server(t, http.StatusOK, nil)
	provider, providerGot := server(t, http.StatusOK, nil)
	client := WrapHTTPClient(http.DefaultClient, Config{Key: testKey, Project: "support-agent", BaseURL: tenet.URL})
	resp, err := client.Post(provider.URL+"/v1/embeddings", "application/json", strings.NewReader(`{"model":"text-embedding-3-small"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(*tenetGot) != 0 || len(*providerGot) != 1 {
		t.Fatalf("tenet calls %d, provider calls %d; want 0 and 1", len(*tenetGot), len(*providerGot))
	}
}

func TestSessionSetters(t *testing.T) {
	tenet, got := server(t, http.StatusOK, nil)
	client := WrapHTTPClient(http.DefaultClient, Config{Key: testKey, Project: "support-agent", BaseURL: tenet.URL})
	SetSessionID(client, "call-2")
	SetSessionTags(client, []string{"a", "b"})
	SetAgentID(client, "agent")
	chat(t, client, "http://provider.invalid")
	if r := (*got)[0]; r.sessionID != "call-2" || r.sessionTags != "a,b" || r.agentID != "agent" {
		t.Fatalf("headers = %+v", r)
	}
}

func TestInvalidConfigFailsRequests(t *testing.T) {
	client := WrapHTTPClient(http.DefaultClient, Config{Key: "sk-provider", Project: "support-agent"})
	req, _ := http.NewRequest(http.MethodPost, "http://provider.invalid/v1/chat/completions", strings.NewReader(`{}`))
	if _, err := client.Do(req); err == nil {
		t.Fatal("request with an invalid Config succeeded")
	}
}

func TestGetPrompt(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		if strings.HasSuffix(r.URL.Path, ":missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		io.WriteString(w, `{"project":"support-agent","tag":"canary","version":4,"text":"Hello {{ name }}"}`)
	}))
	defer srv.Close()
	config := Config{Key: testKey, Project: "support-agent", BaseURL: srv.URL}
	client := WrapHTTPClient(http.DefaultClient, config)

	prompt, err := GetPrompt(context.Background(), client, config, "canary")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/prompts/support-agent:canary" || gotAuth != "Bearer "+testKey {
		t.Fatalf("request path %q auth %q", gotPath, gotAuth)
	}
	if prompt != (Prompt{Project: "support-agent", Tag: "canary", Version: 4, Text: "Hello {{ name }}"}) {
		t.Fatalf("prompt = %+v", prompt)
	}
	if _, err := GetPrompt(context.Background(), nil, config, ""); err != nil || gotPath != "/v1/prompts/support-agent" {
		t.Fatalf("production prompt: path %q err %v", gotPath, err)
	}
	if _, err := GetPrompt(context.Background(), nil, config, "missing"); !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("missing tag err = %v, want ErrPromptNotFound", err)
	}
}
