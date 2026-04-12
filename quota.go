package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
)

// ---------------------------------------------------------------------------
// QUOTA MANAGER
// ---------------------------------------------------------------------------

// quotaManager tiene traccia dell'utilizzo disco per ogni utente.
// L'accesso a repoSize avviene sempre tramite sync/atomic — thread-safe.
type quotaManager struct {
	path        string
	maxBytes    int64 // 0 = nessun limite
	repoSize    int64 // accesso solo via atomic
}

// quotaRegistry mappa ogni utente al proprio quotaManager.
// Viene inizializzato all'avvio e aggiornato al reload della config.
var (
	quotas   = make(map[string]*quotaManager)
	quotasMu sync.RWMutex
)

// initQuotas inizializza i quota manager per tutti gli utenti configurati.
// Calcola la dimensione attuale del repository su disco per ogni utente.
// Chiamata all'avvio e ad ogni reload SIGHUP.
func initQuotas(cfg Config) {
	newQuotas := make(map[string]*quotaManager)

	for user, entry := range cfg.Users {
		if entry.MaxBytes <= 0 {
			// Nessun limite configurato per questo utente — skip.
			continue
		}
		userPath := filepath.Join(cfg.RepoDir, user)
		qm := &quotaManager{
			path:     userPath,
			maxBytes: entry.MaxBytes,
		}
		// Calcola la dimensione attuale su disco (può essere lento su repo grandi,
		// ma avviene solo all'avvio o al reload — non nel path critico).
		if size, err := tallySize(userPath); err == nil {
			atomic.StoreInt64(&qm.repoSize, size)
		} else {
			// Directory non ancora esistente = 0 byte usati. Normale per utenti nuovi.
			atomic.StoreInt64(&qm.repoSize, 0)
		}
		newQuotas[user] = qm
	}

	quotasMu.Lock()
	quotas = newQuotas
	quotasMu.Unlock()
}

// getQuota restituisce il quotaManager di un utente, o nil se non ha limiti.
func getQuota(user string) *quotaManager {
	quotasMu.RLock()
	defer quotasMu.RUnlock()
	return quotas[user]
}

// ---------------------------------------------------------------------------
// CONTROLLO QUOTA
// ---------------------------------------------------------------------------

// checkAndWrap verifica la quota prima di accettare un upload.
// Se il Content-Length dichiarato supererebbe il limite, risponde subito 507.
// Restituisce un writer che aggiorna il contatore di utilizzo durante la scrittura.
// Se l'utente non ha limiti, restituisce il writer originale invariato.
func checkAndWrap(req *http.Request, w io.Writer, user string) (io.Writer, int, error) {
	qm := getQuota(user)
	if qm == nil {
		// Nessuna quota per questo utente.
		return w, 0, nil
	}

	currentSize := atomic.LoadInt64(&qm.repoSize)

	// Controllo preventivo sul Content-Length dichiarato dal client.
	// Restic è onesto sul Content-Length — questo ci permette di rifiutare
	// subito senza aspettare la fine del trasferimento.
	if clStr := req.Header.Get("Content-Length"); clStr != "" {
		cl, err := strconv.ParseInt(clStr, 10, 64)
		if err != nil {
			return nil, http.StatusLengthRequired,
				fmt.Errorf("Content-Length non valido: %w", err)
		}
		if currentSize+cl > qm.maxBytes {
			return nil, http.StatusInsufficientStorage,
				fmt.Errorf("quota superata: %d + %d > %d bytes",
					currentSize, cl, qm.maxBytes)
		}
	}

	// Wrap del writer: aggiorna il contatore byte per byte durante la scrittura.
	return &quotaWriter{Writer: w, qm: qm}, 0, nil
}

// quotaWriter è un io.Writer che aggiorna il contatore di utilizzo atomicamente.
type quotaWriter struct {
	io.Writer
	qm *quotaManager
}

func (w *quotaWriter) Write(p []byte) (int, error) {
	// Controllo anche durante la scrittura: protegge da client che mentono
	// sul Content-Length o trasferimenti senza Content-Length.
	if w.qm.spaceRemaining() >= 0 && int64(len(p)) > w.qm.spaceRemaining() {
		return 0, fmt.Errorf("quota raggiunta (%d bytes massimi)", w.qm.maxBytes)
	}
	n, err := w.Writer.Write(p)
	if n > 0 {
		atomic.AddInt64(&w.qm.repoSize, int64(n))
	}
	return n, err
}

// ---------------------------------------------------------------------------
// UTILITY
// ---------------------------------------------------------------------------

// spaceRemaining restituisce i byte disponibili, o -1 se non c'è limite.
func (qm *quotaManager) spaceRemaining() int64 {
	if qm.maxBytes == 0 {
		return -1
	}
	return qm.maxBytes - atomic.LoadInt64(&qm.repoSize)
}

// spaceUsed restituisce i byte attualmente utilizzati.
func (qm *quotaManager) spaceUsed() int64 {
	return atomic.LoadInt64(&qm.repoSize)
}

// decUsage decrementa il contatore quando un file viene cancellato.
func (qm *quotaManager) decUsage(by int64) {
	atomic.AddInt64(&qm.repoSize, -by)
}

// tallySize calcola ricorsivamente la dimensione di una directory su disco.
// Usato all'avvio per inizializzare i contatori.
func tallySize(path string) (int64, error) {
	var size int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			size += info.Size()
		}
		return nil
	})
	return size, err
}
