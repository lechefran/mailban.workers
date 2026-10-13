package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// staler reports stale jobs; *scheduler.Scheduler implements it.
type staler interface {
	Stale(ctx context.Context, now time.Time) ([]string, error)
}

// healthResponse is the GET /health body.
type healthResponse struct {
	Status    string   `json:"status"`
	StaleJobs []string `json:"staleJobs,omitempty"`
}

// healthHandler answers 200 {"status":"ok"} while every job has succeeded
// recently, 503 {"status":"stale","staleJobs":[…]} when some have not, and
// 503 {"status":"error"} when job history cannot be read.
func healthHandler(s staler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		stale, err := s.Stale(ctx, time.Now().UTC())
		switch {
		case err != nil:
			log.Printf("health: read job history: %v", err)
			writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "error"})
		case len(stale) > 0:
			writeJSON(w, http.StatusServiceUnavailable, healthResponse{Status: "stale", StaleJobs: stale})
		default:
			writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
		}
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
