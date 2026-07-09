package automation

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

const DefaultBodyLimit int64 = 1 << 20

type Store interface {
	ClientAuthenticator
	IdempotencyStore
}

func NewHandler(store Store) http.Handler {
	router := chi.NewRouter()
	router.Use(BodyLimit(DefaultBodyLimit))
	router.Use(NewAuthenticator(store).Middleware)
	router.Use(NewIdempotency(store, IdempotencyOptions{}).Middleware)
	router.Get("/health", health)
	return router
}

func health(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFromContext(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"version":   "v1",
		"client_id": actor.ClientID,
	})
}
