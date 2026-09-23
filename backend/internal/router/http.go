package router

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

type reviewerContextKey struct{}

// Handler accepts an optional separate operator token. Reviewer identity comes
// from credentials, never from a field in the request body.
func Handler(e *Engine, token string, operatorTokens ...string) http.Handler {
	operatorToken := ""
	if len(operatorTokens) > 0 {
		operatorToken = operatorTokens[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Values{"status": "ok", "layer": 2})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := e.Store.Ready(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, Values{"error": "database is not ready"})
			return
		}
		writeJSON(w, http.StatusOK, Values{"status": "ready", "store": "postgresql"})
	})
	mux.HandleFunc("POST /v1/turns", func(w http.ResponseWriter, r *http.Request) {
		var in Input
		if !readJSON(w, r, &in) {
			return
		}
		if in.ReviewMode == "operator" && operatorToken == "" {
			writeJSON(w, http.StatusBadRequest, Values{"error": "operator review requires OPERATOR_API_TOKEN"})
			return
		}
		out, err := e.Process(r.Context(), in)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /v1/sessions/{id}/reviews", func(w http.ResponseWriter, r *http.Request) {
		var in ReviewInput
		if !readJSON(w, r, &in) {
			return
		}
		actor, _ := r.Context().Value(reviewerContextKey{}).(string)
		out, err := e.Review(r.Context(), r.PathValue("id"), in, actor)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/turns/{request_id}", func(w http.ResponseWriter, r *http.Request) {
		if !idPattern.MatchString(r.PathValue("id")) || !idPattern.MatchString(r.PathValue("request_id")) {
			writeJSON(w, http.StatusBadRequest, Values{"error": "invalid session or request ID"})
			return
		}
		run, err := e.Store.GetTurn(r.Context(), r.PathValue("id"), r.PathValue("request_id"))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, Values{"phase": run.Phase, "output": run.Output})
	})
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !idPattern.MatchString(r.PathValue("id")) {
			writeJSON(w, http.StatusBadRequest, Values{"error": "invalid session ID"})
			return
		}
		s, err := e.Store.Get(r.Context(), r.PathValue("id"))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("GET /v1/scenarios", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Values{"scenarios": e.Catalog.Ordered, "system_intents": e.Catalog.System})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			mux.ServeHTTP(w, r)
			return
		}
		actor := "user"
		authorization := r.Header.Get("Authorization")
		supplied, bearer := strings.CutPrefix(authorization, "Bearer ")
		matches := func(expected string) bool {
			return bearer && expected != "" && subtle.ConstantTimeCompare([]byte(supplied), []byte(expected)) == 1
		}
		if matches(operatorToken) {
			actor = "operator"
		} else if token != "" && !matches(token) || token == "" && authorization != "" {
			writeJSON(w, http.StatusUnauthorized, Values{"error": "unauthorized"})
			return
		}
		mux.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), reviewerContextKey{}, actor)))
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, Values{"error": "invalid JSON request"})
		return false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		writeJSON(w, http.StatusBadRequest, Values{"error": "one JSON object required"})
		return false
	}
	return true
}

func writeAPIError(w http.ResponseWriter, err error) {
	code, message := http.StatusInternalServerError, "internal server error"
	switch {
	case errors.Is(err, ErrInvalidInput):
		code, message = http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrConflict), errors.Is(err, ErrBusy):
		code, message = http.StatusConflict, err.Error()
	case errors.Is(err, ErrNotFound):
		code, message = http.StatusNotFound, "record not found"
	case errors.Is(err, ErrDatabase), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		code, message = http.StatusServiceUnavailable, "database or processing unavailable; retry the same request"
	}
	writeJSON(w, code, Values{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
