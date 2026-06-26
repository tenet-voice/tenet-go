package tenet

import (
	"net/http"
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
