package auth

import (
	"strings"
	"testing"
)

func TestGenerateKey_NamedPrefix(t *testing.T) {
	k := GenerateKey("Gal Client")
	if !strings.HasPrefix(k, "gw-gal-client-") {
		t.Fatalf("prefix: %q", k)
	}
	if len(k) != len("gw-gal-client-")+24 {
		t.Fatalf("len %d: %q", len(k), k)
	}
}

func TestGenerateKey_EmptyBackCompat(t *testing.T) {
	k := GenerateKey("")
	if !strings.HasPrefix(k, "gw-") || len(k) != 35 {
		t.Fatalf("back-compat shape: %q", k)
	}
	// No slug chars at all -> same back-compat shape.
	k = GenerateKey("!!!")
	if len(k) != 35 || strings.Count(k, "-") != 1 {
		t.Fatalf("no-slug shape: %q", k)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Gal Client":            "gal-client",
		"  Spaces  Everywhere ": "spaces-everywhere",
		"a__b--c":               "a-b-c",
		"A VERY LONG KEY NAME THAT EXCEEDS TWENTY CHARS": "a-very-long-key-name",
		"":    "",
		"!!!": "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q want %q", in, got, want)
		}
	}
}
