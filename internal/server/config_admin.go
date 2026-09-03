package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Jarvisagentic/tokenroute/internal/config"
)

// requireAdminHeader gates /admin/config on the X-Admin-Key header only.
// Unlike requireAdmin, query-string auth is never accepted and the auth
// store is not required — config endpoints must work before any key store
// exists and must not be reachable via a URL that leaks into logs.
func (s *srv) requireAdminHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.adminKey == "" {
			writeErr(w, http.StatusServiceUnavailable, "admin disabled", "admin_disabled")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Admin-Key")), []byte(s.adminKey)) != 1 {
			writeErr(w, http.StatusUnauthorized, "invalid admin key", "invalid_admin_key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireConfigStore 503s every config endpoint when no store is configured.
func (s *srv) requireConfigStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.configStore == nil {
			writeErr(w, http.StatusServiceUnavailable, "config store unavailable", "config_unavailable")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type configFieldError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeConfigErr maps store errors to the API status taxonomy:
// 409 revision conflicts, 422 typed validation failures, 500 everything
// else (backup/write/apply/rollback). Malformed requests are 400 at the
// handler.
func writeConfigErr(w http.ResponseWriter, err error) {
	var ve *config.ValidationError
	switch {
	case errors.Is(err, config.ErrConflict), errors.Is(err, config.ErrCandidateChanged),
		strings.HasPrefix(err.Error(), "revision conflict:"):
		writeErr(w, http.StatusConflict, scrubConfigErr(err), "config_conflict")
	case errors.As(err, &ve):
		writeValidationErr(w, ve)
	default:
		writeErr(w, http.StatusInternalServerError, scrubConfigErr(err), "internal_error")
	}
}

func writeValidationErr(w http.ResponseWriter, ve *config.ValidationError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"valid":  false,
		"errors": []configFieldError{{Path: ve.Path, Code: ve.Code, Message: firstLineOfErr(ve.Message)}},
	})
}

// scrubConfigErr strips the config file path from store errors; contents
// (secrets) are already redacted/scrubbed inside the store.
func scrubConfigErr(err error) string {
	return firstLineOfErr(err.Error())
}

func firstLineOfErr(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func (s *srv) adminConfigGet(w http.ResponseWriter, r *http.Request) {
	snap, err := s.configStore.Read(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, scrubConfigErr(err), "internal_error")
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *srv) decodeEditRequest(w http.ResponseWriter, r *http.Request, req *config.EditRequest) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return false
	}
	if err := json.Unmarshal(body, req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return false
	}
	if req.Mode != "raw" && req.Mode != "structured" {
		writeErr(w, http.StatusBadRequest, "mode must be \"raw\" or \"structured\"", "invalid_request_error")
		return false
	}
	return true
}

func (s *srv) adminConfigValidate(w http.ResponseWriter, r *http.Request) {
	var req config.EditRequest
	if !s.decodeEditRequest(w, r, &req) {
		return
	}
	cand, err := s.configStore.Validate(r.Context(), req)
	if err != nil {
		writeConfigErr(w, err)
		return
	}
	if s.validateConfig != nil {
		if err := s.validateConfig(r.Context(), cand.RuntimeConfig(), cand.RestartRequiredPaths); err != nil {
			writeValidationErr(w, &config.ValidationError{
				Code:    config.CodeRuntimeInvalid,
				Message: "runtime configuration is invalid",
			})
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(struct {
		Valid bool `json:"valid"`
		*config.Candidate
	}{Valid: true, Candidate: cand})
}

func (s *srv) adminConfigPut(w http.ResponseWriter, r *http.Request) {
	var req config.CommitRequest
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read request body", "invalid_request_error")
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body", "invalid_request_error")
		return
	}
	if req.Mode != "raw" && req.Mode != "structured" {
		writeErr(w, http.StatusBadRequest, "mode must be \"raw\" or \"structured\"", "invalid_request_error")
		return
	}
	apply := s.applyConfig
	if apply == nil {
		apply = func(context.Context, *config.Config, []string) error { return nil }
	}
	res, err := s.configStore.Commit(r.Context(), req, apply)
	if res != nil && !res.Applied && res.Saved {
		// Apply failed and rollback ran; surface the CommitResult fields.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	if err != nil {
		writeConfigErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
