package config

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseDocumentRejectsTrailingYAMLDocuments(t *testing.T) {
	for _, trailing := range []string{"", "{}", "value", "null"} {
		t.Run(trailing, func(t *testing.T) {
			_, err := parseDocument([]byte("listen: :8400\n---\n" + trailing + "\n"))
			var syntax *yamlTypeError
			if !errors.As(err, &syntax) {
				t.Fatalf("error = %#v, want yamlTypeError", err)
			}
		})
	}
}

func TestStoreValidateRejectsTrailingYAMLDocumentTyped(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          candidateBase + "---\n{}\n",
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || ve.Code != CodeYAMLSyntax {
		t.Fatalf("error = %#v, want typed yaml_syntax", err)
	}
}

func TestStoreReadRejectsTrailingYAMLDocument(t *testing.T) {
	path := writeStoreFixture(t, candidateBase+"---\nvalue\n")
	_, err := NewStore(path, 5).Read(context.Background())
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("error = %v", err)
	}
}

const aliasSecretFixture = `admin_key: ${ADMIN_KEY}
aliases:
  copied_secret: &provider_secret literal-provider-secret
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: *provider_secret
    api_keys: [literal-pool-secret, "${POOL_REF}"]
    priority: 1
  - name: p2
    type: openai
    base_url: http://y
    api_key: *provider_secret
    api_keys: [literal-pool-secret, "${POOL_REF}"]
    priority: 2
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`

func assertNoAliasSecretLeak(t *testing.T, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"literal-provider-secret", "literal-pool-secret"} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("secret %q leaked: %s", secret, body)
		}
	}
}

func TestStoreReadRedactsSecretsReachedThroughAliases(t *testing.T) {
	s := NewStore(writeStoreFixture(t, aliasSecretFixture), 5)
	snap := readSnapshot(t, s)

	assertNoAliasSecretLeak(t, snap)
	if !strings.Contains(snap.RawYAML, "*provider_secret") {
		t.Fatalf("aliases not preserved:\n%s", snap.RawYAML)
	}
	if !strings.Contains(snap.RawYAML, SecretKeep) || !strings.Contains(snap.RawYAML, "${POOL_REF}") {
		t.Fatalf("secret redaction or env reference missing:\n%s", snap.RawYAML)
	}
}

func TestCloneNodeRetargetsAliasesToClonedGraph(t *testing.T) {
	root, err := parseDocument([]byte(`value: &shared original
copy: *shared
`))
	if err != nil {
		t.Fatal(err)
	}
	clone := cloneNode(root)
	originalValue := childKey(docRoot(root), "value")
	clonedValue := childKey(docRoot(clone), "value")
	clonedAlias := childKey(docRoot(clone), "copy")
	if clonedAlias == nil || clonedAlias.Kind != yaml.AliasNode {
		t.Fatalf("copy = %#v, want alias", clonedAlias)
	}
	if clonedAlias.Alias != clonedValue || clonedAlias.Alias == originalValue {
		t.Fatal("cloned alias points outside cloned graph")
	}
	clonedValue.Value = "changed"
	if originalValue.Value != "original" {
		t.Fatalf("clone mutation changed original: %q", originalValue.Value)
	}
}

func TestStoreValidateRejectsAliasToLiteralSecretWithoutLeak(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)
	const literal = "alias-injected-secret"
	raw := strings.Replace(candidateBase, "admin_key: ${ADMIN_KEY}", "admin_key: ${ADMIN_KEY}\naliases:\n  injected: &injected "+literal, 1)
	raw = strings.Replace(raw, "api_key: ${P1_KEY}", "api_key: *injected", 1)

	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          raw,
	})
	ve := requireValidationErr(t, err, "providers[p1].api_key", CodeLiteralSecretForbidden)
	if strings.Contains(ve.Error(), literal) {
		t.Fatalf("validation error leaked literal secret: %v", ve)
	}
}

func TestStoreValidateRejectsAliasToSentinelAtDifferentSecretPath(t *testing.T) {
	s := validateFixture(t, `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: literal-p1-secret
    priority: 1
  - name: p2
    type: openai
    base_url: http://y
    api_key: literal-p2-secret
    priority: 2
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`)
	snap := readSnapshot(t, s)
	raw := strings.Replace(snap.RawYAML, "api_key: "+SecretKeep, "api_key: &keep "+SecretKeep, 1)
	second := strings.Index(raw, "api_key: "+SecretKeep)
	if second < 0 {
		t.Fatal("second redacted secret missing")
	}
	raw = raw[:second] + "api_key: *keep" + raw[second+len("api_key: "+SecretKeep):]

	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          raw,
	})
	requireValidationErr(t, err, "providers[p2].api_key", CodeSentinelMisuse)
}

func TestStoreValidateRejectsAliasesNestedInSecretContainers(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)
	const literal = "nested-alias-secret"
	tests := []struct {
		name string
		raw  string
		path string
	}{
		{
			name: "sequence",
			raw:  strings.Replace(candidateBase, "api_key: ${P1_KEY}", "api_key: ${P1_KEY}\n    api_keys: [&nested "+literal+", *nested]", 1),
			path: "providers[p1].api_keys[0]",
		},
		{
			name: "malformed map",
			raw:  strings.Replace(candidateBase, "api_key: ${P1_KEY}", "api_key: ${P1_KEY}\n    api_keys: {primary: &nested "+literal+", backup: *nested}", 1),
			path: "providers[p1].api_keys.primary",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Validate(context.Background(), EditRequest{
				ExpectedRevision: snap.Revision,
				Mode:             "raw",
				RawYAML:          tc.raw,
			})
			ve := requireValidationErr(t, err, tc.path, CodeLiteralSecretForbidden)
			if strings.Contains(ve.Error(), literal) {
				t.Fatalf("validation error leaked literal secret: %v", ve)
			}
		})
	}
}

func TestStoreValidatePreservesNonSecretAliases(t *testing.T) {
	fixture := strings.Replace(candidateBase, "base_url: http://x", "base_url: &shared_url http://x", 1)
	s := validateFixture(t, fixture)
	snap := readSnapshot(t, s)
	raw := strings.Replace(snap.RawYAML, "base_url: http://y", "base_url: *shared_url", 1)

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cand.RawYAML, "&shared_url") || !strings.Contains(cand.RawYAML, "*shared_url") {
		t.Fatalf("non-secret alias not preserved:\n%s", cand.RawYAML)
	}

	s = validateFixture(t, strings.Replace(fixture, "base_url: http://y", "base_url: *shared_url", 1))
	snap = readSnapshot(t, s)
	cand, err = s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         snap.Document,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cand.CandidateRevision != snap.Revision || len(cand.Diff) != 0 {
		t.Fatalf("structured no-op changed aliased YAML: revision %s, diff %+v", cand.CandidateRevision, cand.Diff)
	}
}

func TestReviewStructuredEditPreservesUnrelatedAlias(t *testing.T) {
	s := validateFixture(t, `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: &shared_url http://x
    api_key: ${P1_KEY}
    priority: 1
  - name: p2
    type: openai
    base_url: *shared_url # keep alias comment
    api_key: ${P2_KEY}
    priority: 2
routes:
  - model: auto
    candidates:
      - {provider: p1, model: up}
`)
	snap := readSnapshot(t, s)
	doc := snap.Document
	doc["providers"].([]any)[0].(map[string]any)["priority"] = 3
	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cand.RawYAML, "&shared_url") || !strings.Contains(cand.RawYAML, "*shared_url # keep alias comment") {
		t.Fatalf("unrelated alias or comment lost:\n%s", cand.RawYAML)
	}
}

func TestStoreValidateStructuredRoundTripsSecretAliases(t *testing.T) {
	s := NewStore(writeStoreFixture(t, aliasSecretFixture), 5)
	snap := readSnapshot(t, s)
	doc := snap.Document
	doc["providers"].([]any)[0].(map[string]any)["priority"] = 3

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "structured",
		Document:         doc,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoAliasSecretLeak(t, cand)
	for _, alias := range []string{"*provider_secret"} {
		if !strings.Contains(cand.RawYAML, alias) {
			t.Fatalf("secret alias %q not preserved:\n%s", alias, cand.RawYAML)
		}
	}
	for i, provider := range cand.config.Providers {
		if provider.APIKey != "literal-provider-secret" {
			t.Errorf("providers[%d].api_key = %q", i, provider.APIKey)
		}
		if len(provider.APIKeys) != 2 || provider.APIKeys[0] != "literal-pool-secret" || provider.APIKeys[1] != "" {
			t.Errorf("providers[%d].api_keys = %#v", i, provider.APIKeys)
		}
	}
}

func TestStoreValidateRejectsUnmatchedAliasToSentinel(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)
	raw := strings.Replace(candidateBase, "base_url: http://x", "base_url: &keep "+SecretKeep, 1)
	raw = strings.Replace(raw, "api_key: ${P1_KEY}", "api_key: *keep", 1)

	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          raw,
	})
	requireValidationErr(t, err, "providers[p1].api_key", CodeSentinelMisuse)
}

func TestStoreValidateRejectsLiteralSecretReachedThroughAlias(t *testing.T) {
	s := validateFixture(t, candidateBase)
	snap := readSnapshot(t, s)
	raw := strings.Replace(candidateBase, "base_url: http://x", "base_url: &literal new-literal-secret", 1)
	raw = strings.Replace(raw, "api_key: ${P1_KEY}", "api_key: *literal", 1)

	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          raw,
	})
	ve := requireValidationErr(t, err, "providers[p1].api_key", CodeLiteralSecretForbidden)
	if strings.Contains(ve.Error(), "new-literal-secret") {
		t.Fatalf("validation error leaked literal secret: %v", ve)
	}
}

func TestStoreValidateDiffRedactsNonSecretAliasToSecret(t *testing.T) {
	fixture := strings.Replace(candidateBase, "api_key: ${P1_KEY}", "api_key: &provider_secret literal-provider-secret", 1)
	fixture = strings.Replace(fixture, "routes:", "model_catalog: *provider_secret\nroutes:", 1)
	s := validateFixture(t, fixture)
	snap := readSnapshot(t, s)
	raw := strings.Replace(snap.RawYAML, "model_catalog: *provider_secret\n", "", 1)

	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: snap.Revision,
		Mode:             "raw",
		RawYAML:          raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoAliasSecretLeak(t, cand)
}

func TestStoreReadRedactsAliasBeforeAnchorAtSecretPath(t *testing.T) {
	s := NewStore(writeStoreFixture(t, `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: *late_secret
    priority: 1
aliases:
  late: &late_secret literal-late-secret
`), 5)

	snap, err := s.Read(context.Background())
	if err != nil {
		if strings.Contains(err.Error(), "literal-late-secret") {
			t.Fatalf("read error leaked literal secret: %v", err)
		}
		return
	}
	body, _ := json.Marshal(snap)
	if strings.Contains(string(body), "literal-late-secret") {
		t.Fatalf("forward alias leaked literal secret: %s", body)
	}
}
