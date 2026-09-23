package router

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
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
		writeJSON(w, http.StatusOK, Values{"status": "ready", "store": "sqlite"})
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
	// Layer 3 polls operator lines to speak while a human owns the call.
	mux.HandleFunc("GET /v1/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		after := 0
		if raw := r.URL.Query().Get("after"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				writeJSON(w, http.StatusBadRequest, Values{"error": "after must be a non-negative turn number"})
				return
			}
			after = n
		}
		feed, err := e.OperatorMessages(r.Context(), r.PathValue("id"), after)
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, feed)
	})
	// Operator console. Only OPERATOR_API_TOKEN credentials may take over a call.
	operatorOnly := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if actor, _ := r.Context().Value(reviewerContextKey{}).(string); actor == "operator" {
				next(w, r)
				return
			}
			switch {
			case operatorToken == "":
				writeJSON(w, http.StatusForbidden, Values{"error": "operator API requires OPERATOR_API_TOKEN"})
			case r.Header.Get("Authorization") == "":
				writeJSON(w, http.StatusUnauthorized, Values{"error": "operator credentials required"})
			default:
				writeJSON(w, http.StatusForbidden, Values{"error": "operator credentials required"})
			}
		}
	}
	mux.HandleFunc("GET /v1/operator/handoffs", operatorOnly(func(w http.ResponseWriter, r *http.Request) {
		views, err := e.OpenHandoffs(r.Context(), r.URL.Query().Get("queue"))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, Values{"handoffs": views})
	}))
	mux.HandleFunc("GET /v1/operator/handoffs/{id}", operatorOnly(func(w http.ResponseWriter, r *http.Request) {
		view, err := e.Handoff(r.Context(), r.PathValue("id"))
		if err != nil {
			writeAPIError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, view)
	}))
	mux.HandleFunc("POST /v1/operator/handoffs/{id}/claim", operatorOnly(func(w http.ResponseWriter, r *http.Request) {
		var in OperatorClaimRequest
		if !readJSON(w, r, &in) {
			return
		}
		writeOutput(w, func() (Output, error) { return e.ClaimHandoff(r.Context(), r.PathValue("id"), in) })
	}))
	mux.HandleFunc("POST /v1/operator/handoffs/{id}/messages", operatorOnly(func(w http.ResponseWriter, r *http.Request) {
		var in OperatorMessageRequest
		if !readJSON(w, r, &in) {
			return
		}
		writeOutput(w, func() (Output, error) { return e.SendOperatorMessage(r.Context(), r.PathValue("id"), in) })
	}))
	mux.HandleFunc("POST /v1/operator/handoffs/{id}/close", operatorOnly(func(w http.ResponseWriter, r *http.Request) {
		var in OperatorCloseRequest
		if !readJSON(w, r, &in) {
			return
		}
		writeOutput(w, func() (Output, error) { return e.CloseHandoff(r.Context(), r.PathValue("id"), in) })
	}))
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

func writeOutput(w http.ResponseWriter, action func() (Output, error)) {
	out, err := action()
	if err != nil {
		writeAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func writeAPIError(w http.ResponseWriter, err error) {
	code, message := http.StatusInternalServerError, "internal server error"
	switch {
	case errors.Is(err, ErrNotImplemented):
		code, message = http.StatusNotImplemented, "listing open handoffs needs a store that implements HandoffLister"
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
