package main

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
)

// ---------------------------------------------------------------------------
// AUTH UTENTI
// ---------------------------------------------------------------------------

// AuthMiddleware verifica le credenziali Basic Auth contro la config corrente.
// Controlla inoltre che l'utente autenticato corrisponda al {user} nel path,
// impedendo a un utente di accedere ai repository di un altro.
func AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		cfg := getConfig()
		entry, exists := cfg.Users[user]
		if !ok || !exists || bcrypt.CompareHashAndPassword([]byte(entry.Hash), []byte(pass)) != nil {
			w.Header().Set("WWW-Authenticate", `Basic realm="Restic"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		// L'utente autenticato deve corrispondere al {user} nel path URL.
		// Evita che giorgio possa accedere a /mario/data/...
		if pathUser := chi.URLParam(r, "user"); pathUser != "" && pathUser != user {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// AUTH METRICHE
// ---------------------------------------------------------------------------

// MetricsAuthMiddleware protegge /metrics con credenziali separate dagli utenti
// restic. Se metrics_user non è configurato, l'endpoint è accessibile liberamente
// (utile in ambienti dove la rete è già ristretta, es. dentro Podman).
func MetricsAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := getConfig()
		if cfg.MetricsUser == "" {
			next.ServeHTTP(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != cfg.MetricsUser || pass != cfg.MetricsPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="Metrics"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// CONCORRENZA
// ---------------------------------------------------------------------------

// LimitConcurrency usa il semaforo globale per limitare le operazioni pesanti
// (upload e download di pack file) al numero massimo configurato.
// Le operazioni leggere (list, head, delete) passano senza acquisire il semaforo.
func LimitConcurrency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isHeavy := r.Method == http.MethodPost ||
			(r.Method == http.MethodGet && chi.URLParam(r, "id") != "")

		if isHeavy {
			select {
			case backupSemaphore <- struct{}{}:
				defer func() { <-backupSemaphore }()
			case <-r.Context().Done():
				http.Error(w, "Request cancelled", http.StatusServiceUnavailable)
				return
			}
		}

		activeRequests.Inc()
		defer activeRequests.Dec()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// TIMEOUT
// ---------------------------------------------------------------------------

// RequestTimeout imposta un deadline sul context di ogni richiesta.
// Impostato a 15 minuti per coprire upload di pack file molto grandi
// su connessioni lente, che è il caso d'uso principale di restic.
func RequestTimeout(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
