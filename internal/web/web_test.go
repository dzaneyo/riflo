package web

import (
	"strings"
	"testing"
)

func TestEmbeddedUIIncludesTransientDownloadControls(t *testing.T) {
	htmlBytes, err := files.ReadFile("index.html")
	if err != nil {
		t.Fatalf("read embedded index: %v", err)
	}
	scriptBytes, err := files.ReadFile("app.js")
	if err != nil {
		t.Fatalf("read embedded app.js: %v", err)
	}
	html := string(htmlBytes)
	script := string(scriptBytes)

	if !strings.Contains(html, `id="origin"`) {
		t.Error("embedded HTML is missing origin input")
	}
	for _, marker := range []string{
		`params.get('origin')`,
		`params.get('user_agent')`,
		`hls_variant_index`,
		`'variant-picker'`,
		`transientTaskRequests`,
		`function retryTask`,
		`variant_unavailable`,
	} {
		if !strings.Contains(script, marker) {
			t.Errorf("embedded app.js is missing marker %q", marker)
		}
	}
	for _, forbidden := range []string{"localStorage", "sessionStorage"} {
		if strings.Contains(html, forbidden) || strings.Contains(script, forbidden) {
			t.Errorf("embedded UI must not use browser storage API %q", forbidden)
		}
	}
}
