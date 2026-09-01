package server

import (
	"context"
	"net/http"

	"github.com/google/uuid"
)

// CorrelationHeader is propagated to upstream and echoed downstream.
const CorrelationHeader = "X-Correlation-ID"

const ctxCorrelationID ctxKey = ctxAPIKey + 1

// correlationID generates X-Correlation-ID when absent, stores it in the
// request context, and echoes it on the response. Runs outermost so every
// downstream handler and log line can reference the same ID.
func correlationID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(CorrelationHeader)
		if id == "" {
			id = uuid.NewString()
			r.Header.Set(CorrelationHeader, id)
		}
		w.Header().Set(CorrelationHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxCorrelationID, id)))
	})
}

// correlationIDFrom returns the request's correlation ID, or "".
func correlationIDFrom(r *http.Request) string {
	id, _ := r.Context().Value(ctxCorrelationID).(string)
	return id
}
