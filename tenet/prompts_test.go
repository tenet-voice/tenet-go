package tenet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetPromptVersion(t *testing.T) {
	paths := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Error("missing inference key")
		}
		if r.URL.Path == "/v1/prompts/support-agent:v13" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(Prompt{Agent: "support-agent", Version: 12, Text: "hello"})
	}))
	defer srv.Close()
	config := Config{Key: testKey, Agent: "support-agent", Tag: "model-tag", BaseURL: srv.URL}
	for _, fetch := range []func() (Prompt, error){
		func() (Prompt, error) { return GetPromptVersion(context.Background(), nil, config, 12) },
		func() (Prompt, error) { return GetPrompt(context.Background(), nil, config, "v12") },
	} {
		prompt, err := fetch()
		if err != nil || prompt.Version != 12 {
			t.Fatalf("fetch = %+v, %v", prompt, err)
		}
		if path := <-paths; path != "/v1/prompts/support-agent:v12" {
			t.Fatalf("path = %q", path)
		}
	}
	for _, version := range []int{0, -1} {
		if _, err := GetPromptVersion(context.Background(), nil, config, version); err == nil {
			t.Fatalf("version %d accepted", version)
		}
	}
	if _, err := GetPromptVersion(context.Background(), nil, config, 13); !errors.Is(err, ErrPromptNotFound) {
		t.Fatalf("missing version error = %v, want ErrPromptNotFound", err)
	}
}

func promptConfig(baseURL string) Config {
	return Config{Key: testKey, Agent: "support-agent", BaseURL: baseURL}
}

// Expire a cache entry deterministically, without relying on scheduler timing.
func expirePrompt(p *Prompts, tag string) {
	p.mu.Lock()
	p.entries[tag].next = time.Time{}
	p.mu.Unlock()
}

func waitPrompt(t *testing.T, p *Prompts, tag string, version int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		prompt, err := p.Get(context.Background(), tag)
		if err != nil {
			t.Fatal(err)
		}
		if prompt.Version == version {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("cache did not reach version %d", version)
}

func TestPromptsCacheHitAndPinnedVersion(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(Prompt{Agent: "support-agent", Version: 4, Text: "hello"})
	}))
	defer srv.Close()
	p := NewPrompts(nil, promptConfig(srv.URL), PromptsOptions{Refresh: time.Hour})
	defer p.Close()
	for _, tag := range []string{"", "production", "v4"} {
		if _, err := p.Get(context.Background(), tag); err != nil {
			t.Fatal(err)
		}
	}
	expirePrompt(p, "v4")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tag := range []string{"", "production", "v4"} {
		prompt, err := p.Get(canceled, tag)
		if err != nil || prompt.Version != 4 {
			t.Fatalf("cached %q = %+v, %v", tag, prompt, err)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (production and v4)", got)
	}
	p.mu.Lock()
	refreshing := p.entries["v4"].loading != nil
	p.mu.Unlock()
	if refreshing {
		t.Fatal("pinned version started a refresh")
	}
}

func TestPromptsBackgroundRefresh(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := int(calls.Add(1))
		if version == 2 {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		json.NewEncoder(w).Encode(Prompt{Agent: "support-agent", Version: version})
	}))
	defer srv.Close()
	p := NewPrompts(nil, promptConfig(srv.URL), PromptsOptions{Refresh: time.Hour})
	defer p.Close()
	if _, err := p.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	expirePrompt(p, "production")
	// A request's cancellation must not cancel its background refresh.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := make(chan Prompt, 1)
	go func() {
		prompt, _ := p.Get(ctx, "")
		result <- prompt
	}()
	select {
	case prompt := <-result:
		if prompt.Version != 1 {
			t.Fatalf("stale Get = %+v", prompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cached Get blocked on refresh")
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prompt, err := p.Get(context.Background(), "")
			if err != nil || prompt.Version != 1 {
				t.Errorf("concurrent stale Get = %+v, %v", prompt, err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("requests during refresh = %d, want 2", got)
	}
	close(release)
	waitPrompt(t, p, "", 2)
	// The moved tag is cached, with no further refresh before the interval.
	waitPrompt(t, p, "", 2)
	if got := calls.Load(); got != 2 {
		t.Fatalf("requests after refresh = %d, want 2", got)
	}
}

func TestPromptsRefreshFailureKeepsLastGood(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	errorsSeen := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		version := int(calls.Add(1))
		if fail.Load() {
			http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(Prompt{Agent: "support-agent", Version: version})
	}))
	defer srv.Close()
	p := NewPrompts(nil, promptConfig(srv.URL), PromptsOptions{
		Refresh: time.Hour,
		OnError: func(err error) { errorsSeen <- err },
	})
	defer p.Close()
	if _, err := p.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	expirePrompt(p, "production")
	waitPrompt(t, p, "", 1)
	select {
	case err := <-errorsSeen:
		if err == nil {
			t.Fatal("nil refresh error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing refresh error")
	}
	for i := 0; i < 10; i++ {
		waitPrompt(t, p, "", 1)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2; failed refresh should be throttled", got)
	}
	fail.Store(false)
	expirePrompt(p, "production")
	waitPrompt(t, p, "", 3)
}

func TestPromptsConcurrentInitialGet(t *testing.T) {
	var calls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		json.NewEncoder(w).Encode(Prompt{Agent: "support-agent", Version: 1})
	}))
	defer srv.Close()
	p := NewPrompts(nil, promptConfig(srv.URL), PromptsOptions{})
	defer p.Close()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prompt, err := p.Get(context.Background(), "")
			if err != nil || prompt.Version != 1 {
				t.Errorf("Get = %+v, %v", prompt, err)
			}
		}()
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("initial fetch did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Get(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting caller error = %v", err)
	}
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestPromptsCloseCancelsRefresh(t *testing.T) {
	var calls atomic.Int32
	started, canceled := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			json.NewEncoder(w).Encode(Prompt{Agent: "support-agent", Version: 1})
			return
		}
		close(started)
		<-r.Context().Done()
		close(canceled)
	}))
	defer srv.Close()
	p := NewPrompts(nil, promptConfig(srv.URL), PromptsOptions{Refresh: time.Hour})
	defer p.Close()
	if _, err := p.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	expirePrompt(p, "production")
	waitPrompt(t, p, "", 1)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh did not start")
	}
	p.Close()
	p.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not cancel refresh")
	}
	if _, err := p.Get(context.Background(), ""); !errors.Is(err, ErrPromptsClosed) {
		t.Fatalf("Get after Close = %v", err)
	}
}

func TestPromptsInitialContextAndRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(Prompt{Agent: "support-agent", Version: 1})
	}))
	defer srv.Close()
	p := NewPrompts(nil, promptConfig(srv.URL), PromptsOptions{})
	defer p.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Get(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("initial caller error = %v", err)
	}
	if _, err := p.Get(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}
