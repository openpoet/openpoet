package handlers

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"openpoet/internal/application"
)

// Links returns the service that builds the links OpenPoet hands out. It is
// created on first use: it only needs the database.
func (a *API) Links() *application.LinkService {
	a.linksOnce.Do(func() {
		a.links = application.NewLinkService(a.db)
	})
	return a.links
}

// PublicHost returns the host name of the configured public base URL, or "".
// The Host guard accepts it, so the address the instance is published at does
// not have to be repeated in OPENPOET_ALLOWED_HOSTS.
func (a *API) PublicHost() string {
	if a == nil || a.db == nil {
		return ""
	}
	return a.Links().PublicHost(context.Background())
}

// requestLocalBase is the scheme and host a request came in on. A link built
// on it only works where that address does (for a session calling the API,
// that is the machine OpenPoet runs on).
func requestLocalBase(r *http.Request) string {
	if r == nil || r.Host == "" {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// GetLink returns the link of an artifact, Doc, session, task or project:
// GET /api/links?kind=artifact&id=ab12cd34[&version=2]. The answer carries the
// in-app path and, when a public base URL is configured, the absolute URL to
// pass on.
func (a *API) GetLink(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	version := 0
	if raw := strings.TrimSpace(query.Get("version")); raw != "" && raw != "current" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			respondError(w, http.StatusBadRequest, "version must be a positive integer")
			return
		}
		version = parsed
	}
	id := strings.TrimSpace(query.Get("id"))
	// A session asking for "its own" link does not have to know its id.
	if id == "" && strings.EqualFold(strings.TrimSpace(query.Get("kind")), application.LinkKindSession) {
		if actor := requestActor(r); actor.Type == "session" {
			id = actor.ID
		}
	}
	view, err := a.Links().Link(r.Context(), application.LinkQuery{
		Kind: query.Get("kind"), ID: id, Version: version, LocalBase: requestLocalBase(r),
	})
	if err != nil {
		respondApplicationError(w, err)
		return
	}
	respondJSON(w, http.StatusOK, view)
}
