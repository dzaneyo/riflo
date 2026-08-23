package web

import (
	"strings"
	"testing"
)

func TestEmbeddedUIIncludesTransientDownloadControls(t *testing.T) {
	content, err := files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	html := string(content)
	for _, marker := range []string{
		`id="origin"`,
		`params.get('origin')`,
		`params.get('user_agent')`,
		`hls_variant_index`,
		`'variant-picker'`,
		`transientTaskRequests`,
		`function retryTask`,
		`variant_unavailable`,
	} {
		if !strings.Contains(html, marker) {
			t.Errorf("embedded UI is missing marker %q", marker)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("embedded UI must not use browser storage API %q", forbidden)
		}
	}
}
