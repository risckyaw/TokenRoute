// Package config: typed validation errors for the admin config API.
// ValidationError carries a stable field Path, machine Code, and human
// Message for every client-originated Store.Validate/Commit failure so the
// API layer can serialize 422 field errors without string matching.
package config

import "fmt"

// Validation codes returned by Store.Validate / validateAgainst.
const (
	CodeEmptyRaw               = "empty_raw"
	CodeYAMLSyntax             = "yaml_syntax"
	CodeRootNotMapping         = "root_not_mapping"
	CodeDocumentRequired       = "document_required"
	CodeDocumentInvalid        = "document_invalid"
	CodeMergeConflict          = "merge_conflict"
	CodeSentinelMisuse         = "sentinel_misuse"
	CodeLiteralSecretForbidden = "literal_secret_forbidden"
	CodeCandidateInvalid       = "candidate_invalid"
	CodeRuntimeInvalid         = "runtime_invalid"
	CodeUnknownMode            = "unknown_mode"
)

// ValidationError is a client-originated config validation failure.
type ValidationError struct {
	Path    string
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Path == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Path, e.Message)
}

func newValidationErr(path, code, msg string) *ValidationError {
	return &ValidationError{Path: path, Code: code, Message: msg}
}
