package config

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// typedFixture is a minimal valid base for typed-error tests.
const typedFixture = `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
    priority: 1
`

func typedStore(t *testing.T) (*Store, string) {
	t.Helper()
	p := writeStoreFixture(t, typedFixture)
	return NewStore(p, 5), p
}

func currentRevision(t *testing.T, s *Store) string {
	t.Helper()
	snap, err := s.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snap.Revision
}

func requireValidationErr(t *testing.T, err error, wantPath, wantCode string) *ValidationError {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected *ValidationError, got %T: %v", err, err)
	}
	if ve.Code != wantCode {
		t.Fatalf("code = %q, want %q (err %v)", ve.Code, wantCode, err)
	}
	if ve.Path != wantPath {
		t.Fatalf("path = %q, want %q (err %v)", ve.Path, wantPath, err)
	}
	return ve
}

func TestStoreValidateEmptyRawTyped(t *testing.T) {
	s, _ := typedStore(t)
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "raw",
		RawYAML:          "   \n",
	})
	ve := requireValidationErr(t, err, "", CodeEmptyRaw)
	if !strings.Contains(ve.Message, "empty") {
		t.Fatalf("message = %q", ve.Message)
	}
}

func TestStoreValidateStructuredMissingDocumentTyped(t *testing.T) {
	s, _ := typedStore(t)
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "structured",
	})
	requireValidationErr(t, err, "", CodeDocumentRequired)
}

func TestStoreValidateDuplicateIdentityTyped(t *testing.T) {
	s, _ := typedStore(t)
	dup := `admin_key: ${ADMIN_KEY}
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: ${P1_KEY}
    priority: 1
  - name: p1
    type: openai
    base_url: http://y
    api_key: ${P1_KEY}
    priority: 2
`
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "raw",
		RawYAML:          dup,
	})
	ve := requireValidationErr(t, err, "", CodeCandidateInvalid)
	_ = ve
	// Structured duplicate identity hits the merge path with a stable path.
	doc := map[string]any{
		"admin_key": "${ADMIN_KEY}",
		"providers": []any{
			map[string]any{"name": "p1", "type": "openai", "base_url": "http://x", "api_key": "${P1_KEY}", "priority": 1},
			map[string]any{"name": "p1", "type": "openai", "base_url": "http://y", "api_key": "${P1_KEY}", "priority": 2},
		},
	}
	_, err = s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "structured",
		Document:         doc,
	})
	ve = requireValidationErr(t, err, "providers", CodeMergeConflict)
	if !strings.Contains(ve.Message, "duplicate identity") {
		t.Fatalf("message = %q", ve.Message)
	}
}

func TestStoreValidateSentinelMisuseTyped(t *testing.T) {
	s, _ := typedStore(t)
	// Sentinel at a non-secret path: priority is not secret.
	bad := strings.Replace(typedFixture, "priority: 1", "priority: "+SecretKeep, 1)
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "raw",
		RawYAML:          bad,
	})
	ve := requireValidationErr(t, err, "providers[].priority", CodeSentinelMisuse)
	if !strings.Contains(ve.Message, "sentinel") {
		t.Fatalf("message = %q", ve.Message)
	}
}

func TestStoreValidateSyntaxTyped(t *testing.T) {
	s, _ := typedStore(t)
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "raw",
		RawYAML:          "listen: [unclosed\n",
	})
	requireValidationErr(t, err, "", CodeYAMLSyntax)
}

func TestStoreValidateRootNotMappingTyped(t *testing.T) {
	s, _ := typedStore(t)
	_, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "raw",
		RawYAML:          "- just\n- a\n- list\n",
	})
	requireValidationErr(t, err, "", CodeRootNotMapping)
}

// No-op validate must never leak literal secrets in RawYAML (leak fix).
func TestStoreValidateNoOpDoesNotLeakLiterals(t *testing.T) {
	const fixture = `admin_key: literal-admin-secret
providers:
  - name: p1
    type: openai
    base_url: http://x
    api_key: literal-p1-secret
    priority: 1
`
	p := writeStoreFixture(t, fixture)
	s := NewStore(p, 5)
	cand, err := s.Validate(context.Background(), EditRequest{
		ExpectedRevision: currentRevision(t, s),
		Mode:             "raw",
		RawYAML: strings.ReplaceAll(
			strings.ReplaceAll(fixture, "literal-admin-secret", SecretKeep),
			"literal-p1-secret", SecretKeep),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cand.Diff) != 0 {
		t.Fatalf("expected no-op diff, got %+v", cand.Diff)
	}
	if strings.Contains(cand.RawYAML, "literal-admin-secret") || strings.Contains(cand.RawYAML, "literal-p1-secret") {
		t.Fatalf("no-op validate leaked literal secrets:\n%s", cand.RawYAML)
	}
	if !strings.Contains(cand.RawYAML, SecretKeep) {
		t.Fatal("redaction sentinel missing from no-op raw_yaml")
	}
}
