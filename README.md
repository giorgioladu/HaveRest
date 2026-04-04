# HaveRest 🐧

> Restic REST Server leggero, scritto in Go — backend per [restic](https://restic.net/) e [backrest](https://github.com/garethgeorge/backrest)

```
 ██╗  ██╗ █████╗ ██╗   ██╗███████╗██████╗ ███████╗███████╗████████╗
 ██║  ██║██╔══██╗██║   ██║██╔════╝██╔══██╗██╔════╝██╔════╝╚══██╔══╝
 ███████║███████║██║   ██║█████╗  ██████╔╝█████╗  ███████╗   ██║
 ██╔══██║██╔══██║╚██╗ ██╔╝██╔══╝  ██╔══██╗██╔══╝  ╚════██║   ██║
 ██║  ██║██║  ██║ ╚████╔╝ ███████╗██║  ██║███████╗███████║   ██║
 ╚═╝  ╚═╝╚═╝  ╚═╝  ╚═══╝  ╚══════╝╚═╝  ╚═╝╚══════╝╚══════╝   ╚═╝
```

## Perché HaveRest?

I server REST ufficiali per restic (come `rest-server`) funzionano bene, ma non offrono
rate limiting per-utente, metriche Prometheus integrate, o reload della configurazione
senza riavvio. HaveRest nasce per chi vuole un backend **personale**, controllato,
e monitorabile — da girare in un container Podman dietro Nginx Proxy Manager.

---

## Funzionalità

- **Autenticazione Basic Auth** con hash bcrypt per-utente
- **Rate limiting per-utente** (upload e download), configurabile in MB/s
- **Semaforo globale** per limitare le operazioni pesanti concorrenti
- **Append-only mode** — impedisce la cancellazione dei backup (protezione ransomware)
- **Metriche Prometheus** su `/metrics` con autenticazione separata
- **Reload configurazione** a caldo via `SIGHUP` (no downtime)
- **Graceful shutdown** via `SIGTERM`/`SIGINT`
- **Timeout HTTP** espliciti (anti slow-loris)
- **Scrittura atomica** su disco (file temporaneo + rename)
- **Buffer pool** per ridurre le allocazioni GC durante i trasferimenti

---

## Installazione

### Prerequisiti

- Go 1.22 o superiore
- Linux (testato su Fedora 43)

### Build

```bash
git clone https://github.com/TUO_USERNAME/haverest.git
cd haverest
go mod tidy
go build -o haverest .
```

### Esecuzione

```bash
# Avvio base (config.json nella directory corrente, porta 8080)
./haverest

# Con opzioni
./haverest -f /etc/haverest/config.json -p 9000

# Aiuto
./haverest -h

# Versione
./haverest -v
```

---

## Configurazione

Crea un file `config.json`:

```json
{
  "repo_dir":            "/var/lib/restic-repos",
  "append_only":         true,
  "global_max_parallel": 4,
  "metrics_user":        "prometheus",
  "metrics_pass":        "passwordMetriche",
  "users": {
    "utente1": { "hash": "$2a$10$...", "max_mbps": 50 },
    "utente2": { "hash": "$2a$10$...", "max_mbps": 0  }
  }
}
```

| Campo | Descrizione |
|---|---|
| `repo_dir` | Directory radice dove vengono salvati i repository |
| `append_only` | Se `true`, impedisce la cancellazione di oggetti (consigliato) |
| `global_max_parallel` | Numero massimo di operazioni pesanti simultanee |
| `metrics_user` | Utente per `/metrics` (lascia vuoto per accesso libero) |
| `metrics_pass` | Password per `/metrics` (in chiaro) |
| `users` | Mappa utenti: `hash` bcrypt + `max_mbps` (0 = nessun limite) |

### Generare l'hash bcrypt

Salva questo script una volta sola come `hashpw.go`:

```go
package main

import (
    "fmt"
    "os"
    "golang.org/x/crypto/bcrypt"
)

func main() {
    hash, _ := bcrypt.GenerateFromPassword([]byte(os.Args[1]), bcrypt.DefaultCost)
    fmt.Println(string(hash))
}
```

```bash
go run hashpw.go TuaPassword
# Output: $2a$10$... (copia questo nel config.json)
```

---

## Uso con restic

```bash
export RESTIC_REPOSITORY=rest:http://utente1:TuaPassword@localhost:8080
export RESTIC_PASSWORD=passwordCifraturaDeiDati
restic init
restic backup /percorso/da/salvare
restic snapshots
```

---

## Struttura del progetto

```
haverest/
├── main.go       — avvio, banner, flag CLI, router, shutdown
├── config.go     — struct Config, caricamento, reload via SIGHUP
├── handlers.go   — handler HTTP (save, load, list, delete, head)
├── middleware.go — auth, rate limiting, concorrenza, timeout
├── storage.go    — path resolution, buffer pool, throttling
├── metrics.go    — variabili Prometheus
└── go.mod
```

---

## Deploy con Podman + Nginx Proxy Manager

HaveRest è progettato per girare in un container Podman dietro un reverse proxy
che gestisce il TLS — niente certificati nel codice, niente ruote reinventate.

### Containerfile

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY . .
RUN go mod tidy && go build -o haverest .

FROM alpine:latest
RUN adduser -D -u 1000 haverest
WORKDIR /app
COPY --from=builder /src/haverest .
USER haverest
EXPOSE 8080
ENTRYPOINT ["./haverest"]
```

### Avvio container

```bash
podman build -t haverest .

podman run -d \
  --name haverest \
  -p 8080:8080 \
  -v /etc/haverest/config.json:/app/config.json:ro,Z \
  -v /var/lib/restic-repos:/var/lib/restic-repos:Z \
  haverest -f /app/config.json
```

### Reload config senza riavvio

```bash
podman kill --signal HUP haverest
```

---

## Segnali

| Segnale | Effetto |
|---|---|
| `SIGHUP` | Ricarica `config.json` a caldo (aggiunta/rimozione utenti) |
| `SIGTERM` | Graceful shutdown (attende max 30s le richieste in volo) |
| `SIGINT` | Graceful shutdown (Ctrl+C) |

---

## Metriche Prometheus

| Metrica | Tipo | Descrizione |
|---|---|---|
| `restic_ops_total` | Counter | Operazioni totali per utente e tipo |
| `restic_bytes_total` | Counter | Byte trasferiti per utente e direzione |
| `restic_active_requests` | Gauge | Richieste HTTP attive in questo momento |

---

## Come funziona restic (note tecniche)

Il repository restic è un **content-addressable store**: il nome di ogni oggetto
*è* il suo SHA-256. Tutta la cifratura, la deduplicazione e la verifica dell'integrità
avvengono **sul client** — il server è intenzionalmente uno storage stupido che
fa solo GET, PUT, DELETE, LIST. HaveRest aggiunge solo quello che ha senso
lato server: autenticazione, rate limiting e osservabilità.

---

## Licenza

MIT — fai quello che vuoi, ma un ⭐ fa sempre piacere. 😄

---

## Autore

Giorgio — Fedora Linux, Podman, restic, e caffè ☕
