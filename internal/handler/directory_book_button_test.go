package handler_test

import (
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Bulletproof Scheduling: the directory's Book buttons are brand royal (#2366e9), not the
// upstream near-black, and their text keeps WCAG AA contrast (4.5:1 for normal-size text).

func cssRule(t *testing.T, body, selector string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(selector) + `\s*\{([^}]*)\}`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %q rule in the directory page", selector)
	}
	return m[1]
}

func cssColor(t *testing.T, rule, prop string) string {
	t.Helper()
	m := regexp.MustCompile(`(?:^|[;\s])` + prop + `:\s*(#[0-9a-fA-F]{3,6})`).FindStringSubmatch(rule)
	if m == nil {
		t.Fatalf("no %s in rule %q", prop, rule)
	}
	return strings.ToLower(m[1])
}

func relLuminance(hex string) float64 {
	hex = strings.TrimPrefix(hex, "#")
	if len(hex) == 3 {
		hex = string([]byte{hex[0], hex[0], hex[1], hex[1], hex[2], hex[2]})
	}
	var ch [3]float64
	for i := range ch {
		v, _ := strconv.ParseUint(hex[2*i:2*i+2], 16, 8)
		c := float64(v) / 255
		if c <= 0.03928 {
			ch[i] = c / 12.92
		} else {
			ch[i] = math.Pow((c+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*ch[0] + 0.7152*ch[1] + 0.0722*ch[2]
}

func contrast(a, b string) float64 {
	la, lb := relLuminance(a), relLuminance(b)
	return (math.Max(la, lb) + 0.05) / (math.Min(la, lb) + 0.05)
}

func TestDirectoryBookButton_brandRoyalWithAccessibleText(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	if code, _ := patchMeHandle(t, h, key, `{"handle":"button-host"}`); code != http.StatusOK {
		t.Fatalf("set handle: %d", code)
	}
	slug, _ := seedEventTypeHTTP(t, h, key)
	code, body := personPage(t, h, "button-host")
	if code != http.StatusOK {
		t.Fatalf("directory: %d", code)
	}
	if !strings.Contains(body, `class="dir-book" href="/book/`+slug+`"`) {
		t.Fatal("directory does not render a Book button for the event type")
	}

	rule := cssRule(t, body, ".dir-book")
	bg, fg := cssColor(t, rule, "background"), cssColor(t, rule, "color")
	if bg != "#2366e9" {
		t.Errorf("Book button background = %s; want royal #2366e9", bg)
	}
	if r := contrast(bg, fg); r < 4.5 {
		t.Errorf("Book button text %s on %s is %.2f:1; want at least 4.5:1", fg, bg, r)
	}
	hover := cssColor(t, cssRule(t, body, ".dir-book:hover"), "background")
	if r := contrast(hover, fg); r < 4.5 {
		t.Errorf("hovered Book button text %s on %s is %.2f:1; want at least 4.5:1", fg, hover, r)
	}
}
