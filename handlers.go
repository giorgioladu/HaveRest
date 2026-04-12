package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
	"path/filepath"
	"strconv"
	
	"crypto/sha256"
    "encoding/hex"

	"github.com/go-chi/chi/v5"
)

// ===========================================================================
// NOTA SUL PROTOCOLLO REST RESTIC — VERSIONING API
// ===========================================================================
//
// Il client restic supporta due versioni del protocollo REST:
//
//   v1 (default): la risposta di LIST è un array JSON di sole stringhe
//                 [ "abc123...", "def456..." ]
//
//   v2:           la risposta di LIST è un array JSON di oggetti con
//                 nome E dimensione del file:
//                 [ {"name":"abc123...","size":1234567}, ... ]
//
// Il client segnala quale versione vuole tramite l'header HTTP Accept:
//   Accept: application/vnd.x.restic.rest.v2   → vuole v2
//   Accept: application/vnd.x.restic.rest.v1   → vuole v1
//   (assente)                                  → v1 per retrocompatibilità
//
// Il server deve rispondere con il Content-Type corrispondente alla versione
// che ha effettivamente usato. Qualsiasi valore diverso da
// "application/vnd.x.restic.rest.v2" viene interpretato dal client come v1.
//
// IMPATTO PRATICO:
//   Con v2 restic conosce la dimensione di ogni pack file PRIMA di scaricarlo.
//   Questo elimina le HEAD request separate per ogni file durante operazioni
//   come "restic check" e "restic prune". Su repository con migliaia di
//   pack file la differenza di velocità è molto significativa.
//   Implementare v2 è quindi fortemente consigliato.
//
// ===========================================================================
// NOTA SUI FILE GRANDI (> 2GB)
// ===========================================================================
//
// Per file di grandi dimensioni le ottimizzazioni principali sono:
//
//   1. RANGE REQUEST: restic può richiedere solo una porzione di un file
//      (header "Range: bytes=X-Y"). Implementarla evita di trasferire
//      l'intero pack file quando serve solo un blob specifico.
//      È fondamentale per le performance di "restic restore" su repo grandi.
//
//   2. io.CopyBuffer con buffer dal pool: evita allocazioni ripetute.
//      Il buffer da 256KB è ottimale per trasferimenti su rete locale —
//      abbastanza grande da saturare la banda, abbastanza piccolo da non
//      stressare il GC.
//
//   3. O_DIRECT (Linux): bypasserebbe il page cache del kernel per
//      scritture grandi, riducendo la pressione sulla RAM. Non implementato
//      qui perché richiede buffer allineati a 512 byte e complica il codice;
//      valutare solo se il server ha RAM < 4GB.
//
//   4. Sync su disco: tmp.Sync() su file da 2GB+ è lento perché forza
//      il flush di tutto il dirty buffer. È necessario per l'integrità dei
//      dati ma causa la "pausa" percepita dopo il trasferimento.
//      Alternativa: usare sync_file_range() via syscall per flushare
//      in background durante il trasferimento — non implementato per
//      mantenere il codice portabile.
//
// ===========================================================================

const (
	contentTypeV1 = "application/vnd.x.restic.rest.v1"
	contentTypeV2 = "application/vnd.x.restic.rest.v2"
    DefaultDirMode  os.FileMode = 0700 // rwx------ (per le cartelle)
    DefaultFileMode os.FileMode = 0600 // rwx------ (per i file)
)

// isV2Request controlla se il client ha richiesto esplicitamente API v2.
// Confronto esatto: qualsiasi valore diverso da v2 è trattato come v1.
func isV2Request(r *http.Request) bool {
	return r.Header.Get("Accept") == contentTypeV2
}

// sendJSON serializza data, imposta Content-Type e Content-Length,
// e scrive la risposta. Centralizza la logica comune a handleList v1 e v2.
func sendJSON(w http.ResponseWriter, contentType string, data []byte) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// ===========================================================================
// SAVE  (POST /{user}/data/{type}/{id})
// ===========================================================================

// handleSave scrive un oggetto restic su disco in modo atomico.
//
// THROTTLING SUL READER (non sul writer):
//   Il rate limiting avviene su r.Body (ingresso dati), non sul file writer.
//   Motivo: frenare il reader alla fonte impedisce al buffer di riempirsi
//   prima che il rate limiter possa intervenire. Frenare il writer lascerebbe
//   accumulare dati in memoria senza controllo — sbagliato per file grandi.
//   ThrottledReader chiama WaitN() DOPO aver letto i byte effettivi, quindi
//   aspetta esattamente per i byte ricevuti, non per una stima.
//
// SCRITTURA ATOMICA (temp file + rename):
//   La scrittura avviene su un file temporaneo nella STESSA directory del
//   file finale. Il rename finale è atomico su Linux (POSIX garantito) solo
//   se sorgente e destinazione sono sullo stesso filesystem — per questo
//   il temp file deve stare nella stessa directory, non in /tmp.
//   Se il processo crasha durante la scrittura, il file temporaneo rimane
//   ma non è mai visibile come file restic valido.
//
// IMMUTABILITÀ:
//   I pack file restic non vengono mai sovrascritti. Se il file esiste già,
//   rispondiamo 200 senza toccare nulla. Questo è by design nel protocollo
//   restic: il nome del file È il suo SHA-256, quindi se esiste è già corretto.
func handleSave(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	bType := chi.URLParam(r, "type")
	id := chi.URLParam(r, "id")
	finalPath := getPath(cfg.RepoDir, user, bType, id)

	// Immutabilità: pack file restic identificati dal loro SHA-256 non cambiano mai.
	if _, err := os.Stat(finalPath); err == nil {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}
	
	  // deadline di lettura: se il client non invia dati per X secondi, chiudi
    if tc, ok := r.Body.(interface{ SetReadDeadline(time.Time) error }); ok {
        tc.SetReadDeadline(time.Now().Add(10 * time.Minute))
    }
 
	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, DefaultDirMode); err != nil {
		log.Printf("ERROR save MkdirAll %s: %v", dir, err)
		http.Error(w, "Errore directory", http.StatusInternalServerError)
		return
	}
 
	// File temporaneo nella stessa directory del file finale:
	// il rename è atomico solo se sorgente e destinazione sono sullo stesso filesystem.
	tmp, err := os.CreateTemp(dir, "up-*")
	if err != nil {
		log.Printf("ERROR save CreateTemp %s: %v", dir, err)
		http.Error(w, "Errore file temporaneo", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	// Remove è no-op se il rename ha già avuto successo — sicuro chiamarlo sempre.
	defer os.Remove(tmpName)
	
	if err := tmp.Chmod(DefaultFileMode); err != nil {
		log.Printf("ERROR chmod %s: %v", tmpName, err)
		http.Error(w, "Errore chmod file", http.StatusInternalServerError)
		return
	}
	
	//Check quota
	dst_quota, status, err := checkAndWrap(r, tmp, user)
	if err != nil {
		tmp.Close()
		os.Remove(tmpName)
		log.Printf("WARN quota %s: %v", user, err)
		http.Error(w, err.Error(), status)
		return
	}
 
	// Throttle sul reader: frena l'ingresso dei dati prima che entrino nel buffer.
	// Corretto perché WaitN viene chiamato DOPO aver letto i byte effettivi,
	// quindi aspetta esattamente per i byte letti, non per una stima.
	src := throttleR(r.Body, user, cfg, r.Context())
	des := throttleW(dst_quota, user, cfg, r.Context())
 
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)
 
	hasher := sha256.New()
	n, err := io.CopyBuffer(des, io.TeeReader(src, hasher), buf)
	
	if err == nil && hex.EncodeToString(hasher.Sum(nil)) != id {
		tmp.Close()
		os.Remove(tmpName)
		log.Printf("ERROR hash mismatch %s: %v", tmpName, err)
		http.Error(w, "ERROR hash mismatch", http.StatusBadRequest)
		return
	}
	
	if err != nil {
		tmp.Close()
		log.Printf("ERROR save CopyBuffer %s: %v", finalPath, err)
		http.Error(w, "Errore scrittura", http.StatusInternalServerError)
		return
	}

	// Sync: forza il flush su disco prima del rename.
	// Su file grandi (>2GB) questa operazione può richiedere secondi —
	// è il prezzo dell'integrità dei dati. Non c'è modo di evitarlo
	// senza rischiare corruzione in caso di crash del sistema.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		log.Printf("ERROR save Sync %s: %v", tmpName, err)
		http.Error(w, "Errore sync", http.StatusInternalServerError)
		return
	}
		
	tmp.Close()

	// Rename atomico: il file finale appare già completo sul filesystem.
	if err := os.Rename(tmpName, finalPath); err != nil {
		log.Printf("ERROR save Rename %s -> %s: %v", tmpName, finalPath, err)
		http.Error(w, "Errore rename", http.StatusInternalServerError)
		return
	}
 
	bytesTransferred.WithLabelValues(user, "up").Add(float64(n))
	opsProcessed.WithLabelValues(user, "save").Inc()
 
	w.Header().Set("X-Bytes-Received", fmt.Sprintf("%d", n))
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
	//w.Header().Set("Content-Length", "0")
	//w.WriteHeader(http.StatusOK)
}

// ===========================================================================
// LOAD  (GET /{user}/data/{type}/{id})
// ===========================================================================

// handleLoad invia un oggetto restic al client con supporto Range request.
//
// RANGE REQUEST (RFC 7233):
//   Restic può richiedere solo una porzione di un pack file tramite
//   l'header "Range: bytes=X-Y". Questo è fondamentale durante "restic restore":
//   invece di scaricare l'intero pack file (anche gigabyte), restic scarica
//   solo il blob specifico che gli serve, riducendo drasticamente il traffico.
//   Senza supporto Range, ogni restore di un singolo file richiederebbe il
//   download dell'intero pack file che lo contiene.
//
//   Risposta con Range: HTTP 206 Partial Content + Content-Range header.
//   Risposta senza Range: HTTP 200 OK + intero file (comportamento normale).
//
// CONTENT-LENGTH PRIMA DI WRITEHEADER:
//   In Go, qualsiasi chiamata a w.Header().Set() DOPO w.WriteHeader() viene
//   silenziosamente ignorata — gli header sono già stati inviati al client.
//   Tutti gli header devono essere impostati prima di WriteHeader().
func handleLoad(w http.ResponseWriter, r *http.Request) {
    user := chi.URLParam(r, "user")
    cfg := getConfig()
    path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

    f, err := os.Open(path)
    if err != nil {
        if os.IsNotExist(err) {
            w.Header().Set("Content-Length", "0")
            w.WriteHeader(http.StatusNotFound)
        } else {
            log.Printf("ERROR load Open %s: %v", path, err)
            http.Error(w, "Errore apertura", http.StatusInternalServerError)
        }
        return
    }
    defer f.Close()

    info, err := f.Stat()
    if err != nil {
        log.Printf("ERROR load Stat %s: %v", path, err)
        http.Error(w, "Errore stat", http.StatusInternalServerError)
        return
    }

    // http.ServeContent gestisce Range, 206, Content-Length, Accept-Ranges
    // in modo completamente corretto secondo RFC 7233.
    // Il throttling NON può essere applicato qui perché ServeContent
    // scrive direttamente su w — lo aggiungiamo solo se non c'è Range.
    if r.Header.Get("Range") == "" && cfg.Users[user].MaxMbps > 0 {
        // Nessun Range: possiamo fare throttling manuale
        w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
        w.Header().Set("Accept-Ranges", "bytes")
        w.WriteHeader(http.StatusOK)
        
        src := throttleR(f, user, cfg, r.Context())
        
        buf := bufferPool.Get().([]byte)
        defer bufferPool.Put(buf)
        n, err := io.CopyBuffer(w, src, buf)
        if err != nil {
            log.Printf("WARN load CopyBuffer %s: %v", path, err)
            return
        }
        bytesTransferred.WithLabelValues(user, "down").Add(float64(n))
    } else {
        // Con Range (o senza throttling): delega a ServeContent
        // che gestisce tutti i casi edge del protocollo HTTP Range.
        http.ServeContent(w, r, "", info.ModTime(), f)
        bytesTransferred.WithLabelValues(user, "down").Add(float64(info.Size()))
    }

    opsProcessed.WithLabelValues(user, "load").Inc()
}

// ===========================================================================
// LIST  (GET /{user}/data/{type}/)
// ===========================================================================

// handleList elenca gli oggetti restic con supporto API v1 e v2.
//
// VERSIONING v1 vs v2:
//   v1: array di stringhe  → ["abc123...", "def456..."]
//   v2: array di oggetti   → [{"name":"abc123...","size":1234567}, ...]
//
//   Il client segnala la versione desiderata via Accept header.
//   Il server risponde con il Content-Type corrispondente.
//   Con v2 restic evita HEAD request separate per conoscere la dimensione
//   dei file — su repo con migliaia di pack file è molto più veloce.
//
// STRUTTURA DIRECTORY RESTIC:
//   Restic organizza i pack file in sottodirectory a 2 caratteri:
//   data/ab/abcdef1234...  (evita directory con troppi file)
//   Scendiamo manualmente di un livello con os.ReadDir invece di
//   filepath.Walk — molto più veloce perché non è ricorsivo.
func handleList(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	bType := chi.URLParam(r, "type")
	prefix := filepath.Join(cfg.RepoDir, user, bType)
	v2 := isV2Request(r)

	entries, err := os.ReadDir(prefix)
	if err != nil {
		if os.IsNotExist(err) {
			// Directory non ancora creata: lista vuota è la risposta corretta,
			// non un errore — il repository potrebbe essere vuoto ma valido.
			if v2 {
				sendJSON(w, contentTypeV2, []byte("[]"))
			} else {
				sendJSON(w, contentTypeV1, []byte("[]"))
			}
			return
		}
		log.Printf("ERROR list ReadDir %s: %v", prefix, err)
		http.Error(w, "Errore listing", http.StatusInternalServerError)
		return
	}

	if v2 {
		// --- API v2: oggetti con name + size ---
		// os.DirEntry.Info() non fa una syscall aggiuntiva se i metadati
		// sono già stati letti da ReadDir (Linux li include nella getdents64).
		type fileEntry struct {
			Name string `json:"name"`
			Size int64  `json:"size"`
		}
		files := make([]fileEntry, 0, len(entries))

		for _, e := range entries {
			if e.IsDir() {
				subDir := filepath.Join(prefix, e.Name())
				subEntries, err := os.ReadDir(subDir)
				if err != nil {
					log.Printf("WARN list v2 ReadDir subdir %s: %v", subDir, err)
					continue
				}
				for _, se := range subEntries {
					if se.IsDir() {
						continue
					}
					info, err := se.Info()
					if err != nil {
						log.Printf("WARN list v2 Info %s: %v", se.Name(), err)
						continue
					}
					files = append(files, fileEntry{Name: se.Name(), Size: info.Size()})
				}
			} else {
				info, err := e.Info()
				if err != nil {
					log.Printf("WARN list v2 Info %s: %v", e.Name(), err)
					continue
				}
				files = append(files, fileEntry{Name: e.Name(), Size: info.Size()})
			}
		}

		data, err := json.Marshal(files)
		if err != nil {
			log.Printf("ERROR list v2 Marshal: %v", err)
			http.Error(w, "Errore encoding", http.StatusInternalServerError)
			return
		}
		sendJSON(w, contentTypeV2, data)

	} else {
		// --- API v1: solo nomi ---
		ids := make([]string, 0, len(entries))

		for _, e := range entries {
			if e.IsDir() {
				subDir := filepath.Join(prefix, e.Name())
				subEntries, err := os.ReadDir(subDir)
				if err != nil {
					log.Printf("WARN list v1 ReadDir subdir %s: %v", subDir, err)
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
			log.Printf("ERROR list v1 Marshal: %v", err)
			http.Error(w, "Errore encoding", http.StatusInternalServerError)
			return
		}
		sendJSON(w, contentTypeV1, data)
	}

	opsProcessed.WithLabelValues(user, "list").Inc()
}

// ===========================================================================
// DELETE  (DELETE /{user}/data/{type}/{id})
// ===========================================================================

// handleDelete rimuove un oggetto dal repository.
//
// APPEND-ONLY MODE:
//   Se attivo, qualsiasi tentativo di cancellazione risponde 403 Forbidden.
//   Questo protegge i backup anche se le credenziali restic vengono
//   compromesse — un attaccante non può cancellare gli snapshot.
//   La cancellazione deve avvenire manualmente sul filesystem del server.
//
// AGGIORNAMENTO QUOTA:
//   Leggiamo la dimensione del file PRIMA di cancellarlo per aggiornare
//   il contatore di utilizzo disco del quota manager.
func handleDelete(w http.ResponseWriter, r *http.Request) {
	cfg := getConfig()
	if cfg.AppendOnly {
		http.Error(w, "Append-only mode attivo", http.StatusForbidden)
		return
	}
	user := chi.URLParam(r, "user")
	path := getPath(cfg.RepoDir, user, chi.URLParam(r, "type"), chi.URLParam(r, "id"))

	// Leggi la dimensione prima di rimuovere per aggiornare la quota.
	if info, err := os.Stat(path); err == nil {
	size := info.Size()
	if qm := getQuota(user); qm != nil {
				qm.decUsage(size)
			}
		}

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		log.Printf("ERROR delete Remove %s: %v", path, err)
		http.Error(w, "Errore eliminazione", http.StatusInternalServerError)
		return
	}
	
	

	opsProcessed.WithLabelValues(user, "delete").Inc()
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// ===========================================================================
// HEAD  (HEAD /{user}/data/{type}/{id})
// ===========================================================================

// handleHead verifica l'esistenza di un oggetto senza trasferirne il contenuto.
//
// USO DA PARTE DI RESTIC:
//   Restic chiama HEAD prima di ogni upload per la deduplicazione:
//   se il pack file esiste già (200), non lo rimanda.
//   Con API v2 questa chiamata è quasi eliminata per il listing (la dimensione
//   è già inclusa), ma resta necessaria per verifiche puntuali.
//
// REGOLA FONDAMENTALE IN GO:
//   Content-Length DEVE essere impostato PRIMA di WriteHeader().
//   Dopo WriteHeader() gli header sono già stati inviati e ogni
//   modifica viene ignorata silenziosamente — bug difficile da debuggare.
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

	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
}

// ===========================================================================
// CONFIG RESTIC  (HEAD|GET|POST /{user}/config)
// ===========================================================================

// handleConfigHead verifica se il repository restic è già inizializzato.
//
// Restic chiama HEAD /config come PRIMA operazione di qualsiasi comando
// (backup, restore, snapshots, ecc.) per verificare che il repository esista.
// Se risponde 404, restic si rifiuta di procedere con qualsiasi operazione.
//
// ERRORE "negative content length":
//   Se Content-Length non viene impostato prima di WriteHeader(), il valore
//   risultante è -1 (non impostato). Restic interpreta questo come errore
//   e riprova indefinitamente con backoff esponenziale.
//   Soluzione: impostare sempre Content-Length prima di WriteHeader().
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

	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
}

// handleConfigLoad invia la config cifrata del repository restic.
// La config è un piccolo file JSON cifrato creato da "restic init".
// Contiene i parametri di cifratura (NON la chiave in chiaro).
func handleConfigLoad(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, "config", "")

	f, err := os.Open(path)
	if err != nil {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		log.Printf("ERROR config load Stat %s: %v", path, err)
		http.Error(w, "Errore stat", http.StatusInternalServerError)
		return
	}

	// Content-Length prima di WriteHeader — regola fondamentale.
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	io.Copy(w, f)
	opsProcessed.WithLabelValues(user, "load_config").Inc()
}

// handleConfigSave salva la config cifrata del repository restic.
//
// Chiamata UNA SOLA VOLTA da "restic init" — non viene mai sovrascritta.
// Se esiste già, rispondiamo 200 (repository già inizializzato).
// Usa scrittura atomica (temp + rename) come handleSave.
func handleConfigSave(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	path := getPath(cfg.RepoDir, user, "config", "")

	if _, err := os.Stat(path); err == nil {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusOK)
		return
	}

	if err := os.MkdirAll(filepath.Dir(path), DefaultDirMode); err != nil {
                log.Printf("ERROR config save MkdirAll %s: %v", path, err)
		http.Error(w, "Errore directory", http.StatusInternalServerError)
		return
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "cfg-*")
	if err != nil {
		log.Printf("ERROR config save CreateTemp: %v", err)
		http.Error(w, "Errore temp", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())

	if _, err := io.Copy(tmp, r.Body); err != nil {
		tmp.Close()
		log.Printf("ERROR config save Copy: %v", err)
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
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// ===========================================================================
// CREATE REPO  (POST /{user}/?create=true)
// ===========================================================================

// handleCreateRepo crea la struttura di directory per un nuovo repository.
//
// Restic chiama POST /{user}/?create=true come primo passo di "restic init".
// Deve rispondere 200 sia se la directory viene creata ora,
// sia se esisteva già (idempotente).
func handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	user := chi.URLParam(r, "user")
	cfg := getConfig()
	path := filepath.Join(cfg.RepoDir, user)

	// Crea la cartella dell'utente se non esiste
	if err := os.MkdirAll(path, DefaultDirMode); err != nil {
		log.Printf("ERROR create repo %s: %v", path, err)
		http.Error(w, "Errore creazione repository", http.StatusInternalServerError)
		return
	}

	log.Printf("INFO repository inizializzato per utente: %s", user)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// handleConfigHead verifica se la config del repository esiste.
// Restic lo chiama prima di init per sapere se il repo è già inizializzato.
//func handleConfigHead(w http.ResponseWriter, r *http.Request) {
    //user := chi.URLParam(r, "user")
    //cfg := getConfig()
    //path := getPath(cfg.RepoDir, user, "config", "")

    //info, err := os.Stat(path)
    //if err != nil {
        //w.Header().Set("Content-Length", "0") 
        //w.WriteHeader(http.StatusNotFound)     
        //return
    //}
    //w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size())) 
    //w.WriteHeader(http.StatusOK)                                      
//}
