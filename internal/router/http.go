package router

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

func Handler(e *Engine, token string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Values{"status": "ok", "layer": 2}) })
	mux.HandleFunc("POST /v1/turns", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		defer r.Body.Close()
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		var in Input
		if err := dec.Decode(&in); err != nil {
			writeJSON(w, 400, Values{"error": "invalid JSON request"})
			return
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			writeJSON(w, 400, Values{"error": "one JSON object required"})
			return
		}
		out, err := e.Process(r.Context(), in)
		if err != nil {
			code := 500
			if errors.Is(err, ErrInvalidInput) {
				code = 400
			} else if errors.Is(err, ErrConflict) {
				code = 409
			} else if errors.Is(err, ErrCapacity) {
				code = 503
			}
			writeJSON(w, code, Values{"error": err.Error()})
			return
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !idPattern.MatchString(id) {
			writeJSON(w, 400, Values{"error": "invalid session ID"})
			return
		}
		s, ok := e.Store.Get(r.Context(), id)
		if !ok {
			writeJSON(w, 404, Values{"error": "session not found"})
			return
		}
		writeJSON(w, 200, s)
	})
	mux.HandleFunc("GET /v1/scenarios", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, Values{"scenarios": e.Catalog.Ordered, "system_intents": e.Catalog.System})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path != "/healthz" && token != "" {
			supplied := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
				writeJSON(w, 401, Values{"error": "unauthorized"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
