package main

import (
	"encoding/json"
	"fmt"
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
	user := chi.URLParam(r, "user")
	cfg := getConfig()

	bType := chi.URLParam(r, "type")
	id := chi.URLParam(r, "id")

	finalPath := getPath(cfg.RepoDir, user, bType, id)
	tmpPath := finalPath + ".tmp"

	// crea directory se non esiste
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		log.Printf("ERROR save MkdirAll %s: %v", finalPath, err)
		http.Error(w, "Errore directory", http.StatusInternalServerError)
		return
	}

	// file temporaneo (atomic write)
	f, err := os.Create(tmpPath)
	if err != nil {
		log.Printf("ERROR save Create %s: %v", tmpPath, err)
		http.Error(w, "Errore creazione file", http.StatusInternalServerError)
		return
	}
	defer func() {
		f.Close()
		os.Remove(tmpPath) // cleanup se qualcosa va male
	}()

	// throttle sul WRITER
	dst := throttleWriter(f, user, cfg, r.Context())

	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	n, err := io.CopyBuffer(dst, r.Body, buf)
	if err != nil {
		log.Printf("ERROR save CopyBuffer %s: %v", finalPath, err)
		http.Error(w, "Errore scrittura", http.StatusInternalServerError)
		return
	}

	// flush su disco (importante)
	if err := f.Sync(); err != nil {
		log.Printf("ERROR save Sync %s: %v", finalPath, err)
		http.Error(w, "Errore sync", http.StatusInternalServerError)
		return
	}

	if err := f.Close(); err != nil {
		log.Printf("ERROR save Close %s: %v", finalPath, err)
		http.Error(w, "Errore close", http.StatusInternalServerError)
		return
	}

	// rename atomico → evita file corrotti
	if err := os.Rename(tmpPath, finalPath); err != nil {
		log.Printf("ERROR save Rename %s: %v", finalPath, err)
		http.Error(w, "Errore rename", http.StatusInternalServerError)
		return
	}

	// risposta restic-friendly
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)

	bytesTransferred.WithLabelValues(user, "up").Add(float64(n))
	opsProcessed.WithLabelValues(user, "save").Inc()
}

// ---------------------------------------------------------------------------
// LOAD  (GET /{type}/{id})
// ---------------------------------------------------------------------------

// handleLoad invia un oggetto restic al client.
// Applica il throttling anche sul download, simmetrico all'upload.
func handleLoad(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			w.Header().Set("Content-Length", "0")
			http.Error(w, "Not found", http.StatusNotFound)
		} else {
			log.Printf("ERROR load Open %s: %v", path, err)
			http.Error(w, "Errore apertura file", http.StatusInternalServerError)
		}
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("ERROR load Stat %s: %v", path, err)
		http.Error(w, "Errore stat file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.WriteHeader(http.StatusOK)

	src := throttle(f, user, cfg, r.Context())

	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	n, err := io.CopyBuffer(w, src, buf)
	if err != nil {
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
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	prefix := filepath.Join(cfg.RepoDir, user, chi.URLParam(r, "type"))

	entries, err := os.ReadDir(prefix)
	if err != nil {
		if os.IsNotExist(err) {
			// lista vuota ma con Content-Length corretto
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "2")
			w.WriteHeader(http.StatusOK)
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

	data, err := json.Marshal(ids)
	if err != nil {
		log.Printf("ERROR list Marshal: %v", err)
		http.Error(w, "Errore encoding", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)

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
	user := chi.URLParam(r, "user")
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("ERROR delete Remove %s: %v", path, err)
		http.Error(w, "Errore eliminazione", http.StatusInternalServerError)
		return
	}

	opsProcessed.WithLabelValues(user, "delete").Inc()
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// HEAD  (HEAD /{type}/{id})
// ---------------------------------------------------------------------------

// handleHead verifica l'esistenza di un oggetto senza trasferirne il contenuto.
// Restic lo usa per il controllo di deduplicazione prima di caricare un pack file.
func handleHead(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	info, err := os.Stat(path)
	if err != nil {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// CONFIG RESTIC  (GET /config  POST /config)
// ---------------------------------------------------------------------------

// handleConfigLoad invia la config cifrata del repository restic.
// È distinta dagli altri oggetti perché non ha un id nel path.
func handleConfigLoad(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
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
	user := chi.URLParam(r, "user")
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


// handleCreateRepo gestisce la creazione della directory del repository.
// Restic chiama POST /?create=true durante l'init.
func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	path := filepath.Join(cfg.RepoDir, user)

	// Crea la cartella dell'utente se non esiste
	if err := os.MkdirAll(path, 0755); err != nil {
		log.Printf("ERROR create repo %s: %v", path, err)
		http.Error(w, "Errore creazione repository", http.StatusInternalServerError)
		return
	}

	log.Printf("INFO Repository creato per l'utente: %s", user)
	w.WriteHeader(http.StatusOK)
}

// handleConfigHead verifica se la config del repository esiste.
// Restic lo chiama prima di init per sapere se il repo è già inizializzato.
func handleConfigHead(w http.ResponseWriter, r *http.Request) {
    user := chi.URLParam(r, "user")
    cfg := getConfig()
    path := getPath(cfg.RepoDir, user, "config", "")

    info, err := os.Stat(path)
    if err != nil {
        w.Header().Set("Content-Length", "0") 
        w.WriteHeader(http.StatusNotFound)     
        return
    }
    w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size())) 
    w.WriteHeader(http.StatusOK)                                      
}
