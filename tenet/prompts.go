package tenet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ErrPromptNotFound means the Project has no prompt under the tag.
var ErrPromptNotFound = errors.New("tenet: prompt not found")

// Prompt is one version of a Project's prompt. Tenet stores the text as
// written (plain text, Jinja, or anything else) and never renders it.
type Prompt struct {
	Project string `json:"project"`
	Tag     string `json:"tag"`
	Version int    `json:"version"`
	Text    string `json:"text"`
}

// GetPrompt fetches the prompt version a tag names in config.Project's prompt
// store. An empty tag means production. Prompt tags are separate from version
// tags: config.Tag is not used.
//
// It makes one request; callers that need the prompt when Tenet cannot be
// reached keep their own copy.
func GetPrompt(ctx context.Context, client *http.Client, config Config, tag string) (Prompt, error) {
	if err := ValidateConfig(config); err != nil {
		return Prompt{}, err
	}
	if tag != "" && !name.MatchString(tag) {
		return Prompt{}, fmt.Errorf("tenet: prompt tag cannot contain spaces, ':' or '/'")
	}
	if client == nil {
		client = http.DefaultClient
	}
	if t, ok := client.Transport.(*transport); ok {
		client = &http.Client{Transport: t.inner, Timeout: client.Timeout}
	}
	ref := config.Project
	if tag != "" {
		ref += ":" + tag
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.baseURL()+"/v1/prompts/"+url.PathEscape(ref), nil)
	if err != nil {
		return Prompt{}, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(config.Key))
	resp, err := client.Do(req)
	if err != nil {
		return Prompt{}, fmt.Errorf("tenet: fetch prompt %s: %w", ref, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Prompt{}, fmt.Errorf("%w: %s", ErrPromptNotFound, ref)
	case resp.StatusCode != http.StatusOK:
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return Prompt{}, fmt.Errorf("tenet: fetch prompt %s: %s: %s", ref, resp.Status, strings.TrimSpace(string(detail)))
	}
	var prompt Prompt
	if err := json.NewDecoder(resp.Body).Decode(&prompt); err != nil {
		return Prompt{}, fmt.Errorf("tenet: decode prompt %s: %w", ref, err)
	}
	return prompt, nil
}
