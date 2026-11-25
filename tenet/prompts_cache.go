package tenet

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrPromptsClosed is returned by Get after the prompt cache is closed.
var ErrPromptsClosed = errors.New("tenet: prompt cache closed")

// PromptsOptions controls prompt caching.
type PromptsOptions struct {
	// Refresh is the minimum interval between refresh attempts for each tag.
	// Nonpositive values use 30 seconds. Refreshes start on a subsequent Get;
	// idle caches do no work. Pinned versions never refresh.
	Refresh time.Duration

	// OnError reports background refresh failures. It runs outside the cache
	// lock, may be called concurrently for different tags, and should return
	// promptly. Initial fetch errors are returned by Get instead.
	OnError func(error)
}

// Prompts caches one Agent's prompts. It is safe for concurrent use.
// Create it with NewPrompts and call Close when it is no longer needed.
type Prompts struct {
	client  *http.Client
	config  Config
	options PromptsOptions
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	closed  bool
	entries map[string]*promptEntry
}

type promptEntry struct {
	prompt  Prompt
	valid   bool
	loading *promptFetch
	next    time.Time
}

type promptFetch struct {
	done   chan struct{}
	prompt Prompt
	err    error
}

// NewPrompts creates a cache without making a request. A nil client uses
// http.DefaultClient. Configuration errors are returned on Get.
func NewPrompts(client *http.Client, config Config, options PromptsOptions) *Prompts {
	if options.Refresh <= 0 {
		options.Refresh = 30 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Prompts{
		client: client, config: config, options: options,
		ctx: ctx, cancel: cancel, entries: make(map[string]*promptEntry),
	}
}

// Get returns a tag's prompt; an empty tag means production. A reference of
// "v<N>" pins a version permanently. The first fetch uses the caller's context;
// concurrent initial callers wait with their own contexts. Once cached, Get
// returns immediately, even if the caller's context is canceled, and starts at
// most one background refresh per Refresh interval. Failed refreshes retain the
// last good prompt and call OnError, if set.
func (p *Prompts) Get(ctx context.Context, tag string) (Prompt, error) {
	if tag == "" {
		tag = "production"
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return Prompt{}, ErrPromptsClosed
	}
	e := p.entries[tag]
	if e == nil {
		e = &promptEntry{}
		p.entries[tag] = e
	}
	if e.valid {
		prompt := e.prompt
		if e.loading == nil && !pinnedPrompt(tag) && !time.Now().Before(e.next) {
			e.loading = &promptFetch{done: make(chan struct{})}
			e.next = time.Now().Add(p.options.Refresh)
			go p.fetch(p.ctx, tag, e, true)
		}
		p.mu.Unlock()
		return prompt, nil
	}
	if e.loading != nil {
		fetch := e.loading
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return Prompt{}, ctx.Err()
		case <-p.ctx.Done():
			return Prompt{}, ErrPromptsClosed
		case <-fetch.done:
			return fetch.prompt, fetch.err
		}
	}
	e.loading = &promptFetch{done: make(chan struct{})}
	p.mu.Unlock()

	// The first caller owns the fetch, but Close must also be able to cancel it.
	fetchCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	defer cancel()
	return p.fetch(fetchCtx, tag, e, false)
}

func (p *Prompts) fetch(ctx context.Context, tag string, e *promptEntry, background bool) (Prompt, error) {
	prompt, err := GetPrompt(ctx, p.client, p.config, tag)
	p.mu.Lock()
	if err == nil {
		e.prompt, e.valid = prompt, true
	}
	e.loading.prompt, e.loading.err = prompt, err
	if !background {
		e.next = time.Now().Add(p.options.Refresh)
	}
	close(e.loading.done)
	e.loading = nil
	report := background && err != nil && !p.closed && p.options.OnError != nil
	p.mu.Unlock()
	if report {
		p.options.OnError(err)
	}
	return prompt, err
}

func pinnedPrompt(tag string) bool {
	if !strings.HasPrefix(tag, "v") || len(tag) < 2 {
		return false
	}
	for _, digit := range tag[1:] {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	version, err := strconv.Atoi(strings.TrimPrefix(tag, "v"))
	return err == nil && version > 0
}

// Close stops future refreshes and cancels in-flight fetches. It is idempotent.
// It does not wait for HTTP transports or OnError callbacks to return.
// Subsequent Get calls return ErrPromptsClosed.
func (p *Prompts) Close() {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	p.mu.Unlock()
}
