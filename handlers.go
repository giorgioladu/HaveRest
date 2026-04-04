/*
 * This program is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * (at your option) any later version.
 * 
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 * 
 * You should have received a copy of the GNU General Public License
 * along with this program; if not, write to the Free Software
 * Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston,
 * MA 02110-1301, USA.
 * 
 */
 
package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"
)

// ---------------------------------------------------------------------------
// SAVE  (POST /{type}/{id})
// ---------------------------------------------------------------------------

// handleSave scrive un oggetto restic su disco in modo atomico.
// La scrittura avviene su un file temporaneo nella stessa directory,
// poi viene rinominato nella destinazione finale: su Linux il rename è atomico,
// quindi un client non vedrà mai un file parzialmente scritto.
// Se il file esiste già, risponde 200 senza toccare nulla (immutabilità).
func handleSave(w http.ResponseWriter, r *http.Request) {
	user, _, _ := r.BasicAuth()
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	// Immutabilità: i pack file restic non vengono mai sovrascritti.
	if _, err := os.Stat(path); err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("ERROR save MkdirAll %s: %v", dir, err)
		http.Error(w, "Errore creazione directory", http.StatusInternalServerError)
		return
	}

	tmp, err := os.CreateTemp(dir, "up-*")
	if err != nil {
		log.Printf("ERROR save CreateTemp %s: %v", dir, err)
		http.Error(w, "Errore file temporaneo", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	// Remove è no-op se il Rename ha avuto successo — sicuro chiamarlo sempre.
	defer os.Remove(tmpName)

	body := throttle(r.Body, user, cfg, r.Context())

	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	n, err := io.CopyBuffer(tmp, body, buf)
	if err != nil {
		tmp.Close()
		log.Printf("ERROR save CopyBuffer %s: %v", path, err)
		http.Error(w, "Errore scrittura", http.StatusInternalServerError)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		log.Printf("ERROR save Sync %s: %v", tmpName, err)
		http.Error(w, "Errore sync", http.StatusInternalServerError)
		return
	}
	tmp.Close()

	if err := os.Rename(tmpName, path); err != nil {
		log.Printf("ERROR save Rename %s -> %s: %v", tmpName, path, err)
		http.Error(w, "Errore salvataggio finale", http.StatusInternalServerError)
		return
	}

	bytesTransferred.WithLabelValues(user, "up").Add(float64(n))
	opsProcessed.WithLabelValues(user, "save").Inc()
	w.WriteHeader(http.StatusCreated)
}

// ---------------------------------------------------------------------------
// LOAD  (GET /{type}/{id})
// ---------------------------------------------------------------------------

// handleLoad invia un oggetto restic al client.
// Applica il throttling anche sul download, simmetrico all'upload.
func handleLoad(w http.ResponseWriter, r *http.Request) {
	user, _, _ := r.BasicAuth()
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "Not found", http.StatusNotFound)
		} else {
			log.Printf("ERROR load Open %s: %v", path, err)
			http.Error(w, "Errore apertura file", http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()

	src := throttle(f, user, cfg, r.Context())

	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	n, err := io.CopyBuffer(w, src, buf)
	if err != nil {
		// Spesso è il client che chiude la connessione — WARN, non ERROR.
		log.Printf("WARN load CopyBuffer %s: %v", path, err)
		return
	}

	bytesTransferred.WithLabelValues(user, "down").Add(float64(n))
	opsProcessed.WithLabelValues(user, "load").Inc()
}

// ---------------------------------------------------------------------------
// LIST  (GET /{type}/)
// ---------------------------------------------------------------------------

// handleList elenca tutti gli oggetti di un tipo nel repository dell'utente.
// Usa os.ReadDir invece di filepath.Walk: molto più veloce su repo grandi
// perché evita la ricorsione — scende manualmente solo di un livello
// nelle sottodirectory a 2 caratteri usate da restic.
func handleList(w http.ResponseWriter, r *http.Request) {
	user, _, _ := r.BasicAuth()
	cfg := getConfig()
	prefix := filepath.Join(cfg.RepoDir, user, chi.URLParam(r, "type"))

	entries, err := os.ReadDir(prefix)
	if err != nil {
		if os.IsNotExist(err) {
			// Directory non ancora creata: lista vuota è la risposta corretta.
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("[]"))
			return
		}
		log.Printf("ERROR list ReadDir %s: %v", prefix, err)
		http.Error(w, "Errore listing", http.StatusInternalServerError)
		return
	}

	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			// Sottodirectory tipo "ab/" — scendi di un livello.
			subDir := filepath.Join(prefix, e.Name())
			subEntries, err := os.ReadDir(subDir)
			if err != nil {
				log.Printf("WARN list ReadDir subdir %s: %v", subDir, err)
				continue
			}
			for _, se := range subEntries {
				if !se.IsDir() {
					ids = append(ids, se.Name())
				}
			}
		} else {
			ids = append(ids, e.Name())
		}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(ids); err != nil {
		log.Printf("ERROR list Encode: %v", err)
	}
	opsProcessed.WithLabelValues(user, "list").Inc()
}

// ---------------------------------------------------------------------------
// DELETE  (DELETE /{type}/{id})
// ---------------------------------------------------------------------------

// handleDelete rimuove un oggetto dal repository.
// In append-only mode risponde 403: restic non potrà mai cancellare snapshot,
// il che garantisce che i backup non vengano eliminati accidentalmente
// (o da un attaccante che ha compromesso le credenziali restic).
func handleDelete(w http.ResponseWriter, r *http.Request) {
	cfg := getConfig()
	if cfg.AppendOnly {
		http.Error(w, "Append-only mode attivo", http.StatusForbidden)
		return
	}
	user, _, _ := r.BasicAuth()
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("ERROR delete Remove %s: %v", path, err)
		http.Error(w, "Errore eliminazione", http.StatusInternalServerError)
		return
	}

	opsProcessed.WithLabelValues(user, "delete").Inc()
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// HEAD  (HEAD /{type}/{id})
// ---------------------------------------------------------------------------

// handleHead verifica l'esistenza di un oggetto senza trasferirne il contenuto.
// Restic lo usa per il controllo di deduplicazione prima di caricare un pack file.
func handleHead(w http.ResponseWriter, r *http.Request) {
	user, _, _ := r.BasicAuth()
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	if _, err := os.Stat(path); err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// CONFIG RESTIC  (GET /config  POST /config)
// ---------------------------------------------------------------------------

// handleConfigLoad invia la config cifrata del repository restic.
// È distinta dagli altri oggetti perché non ha un id nel path.
func handleConfigLoad(w http.ResponseWriter, r *http.Request) {
	user, _, _ := r.BasicAuth()
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, "config", "")

	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	io.Copy(w, f)
	opsProcessed.WithLabelValues(user, "load_config").Inc()
}

// handleConfigSave salva la config cifrata del repository restic.
// Viene chiamata una sola volta da "restic init" e mai più sovrascritta.
func handleConfigSave(w http.ResponseWriter, r *http.Request) {
	user, _, _ := r.BasicAuth()
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, "config", "")

	if _, err := os.Stat(path); err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		http.Error(w, "Errore directory", http.StatusInternalServerError)
		return
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "cfg-*")
	if err != nil {
		http.Error(w, "Errore temp", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, r.Body); err != nil {
		tmp.Close()
		http.Error(w, "Errore scrittura", http.StatusInternalServerError)
		return
	}
	tmp.Sync()
	tmp.Close()

	if err := os.Rename(tmp.Name(), path); err != nil {
		http.Error(w, "Errore salvataggio", http.StatusInternalServerError)
		return
	}

	opsProcessed.WithLabelValues(user, "save_config").Inc()
	w.WriteHeader(http.StatusCreated)
}
