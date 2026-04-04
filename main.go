package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	VERSION = "1.0.0"
	AUTHOR  = "Giorgio"
	YEAR    = "2025"
)

// ---------------------------------------------------------------------------
// BANNER
// ---------------------------------------------------------------------------

func printBanner() {
	fmt.Println(`
 ██╗  ██╗ █████╗ ██╗   ██╗███████╗██████╗ ███████╗███████╗████████╗
 ██║  ██║██╔══██╗██║   ██║██╔════╝██╔══██╗██╔════╝██╔════╝╚══██╔══╝
 ███████║███████║██║   ██║█████╗  ██████╔╝█████╗  ███████╗   ██║   
 ██╔══██║██╔══██║╚██╗ ██╔╝██╔══╝  ██╔══██╗██╔══╝  ╚════██║   ██║   
 ██║  ██║██║  ██║ ╚████╔╝ ███████╗██║  ██║███████╗███████║   ██║   
 ╚═╝  ╚═╝╚═╝  ╚═╝  ╚═══╝  ╚══════╝╚═╝  ╚═╝╚══════╝╚══════╝   ╚═╝   `)
	fmt.Println(`
 ╔══════════════════════════════════════════════════════════════════╗`)
	fmt.Printf(" ║  Restic REST Server  v%-8s  by %-12s  (c) %s        ║\n", VERSION, AUTHOR, YEAR)
	fmt.Println(` ║  Backend per restic + backrest                                   ║
 ╚══════════════════════════════════════════════════════════════════╝`)
}

func printUsage(progname string) {
	printBanner()
	fmt.Printf(`
 UTILIZZO:
   %s [opzioni]

 OPZIONI:
   -f <path>    Percorso del file di configurazione
                (default: config.json nella directory corrente)

   -p <porta>   Porta TCP su cui ascoltare
                (default: 8000)

   -v           Mostra la versione ed esce

   -h           Mostra questo aiuto ed esce

 ESEMPI:
   %s
   %s -p 9000
   %s -f /etc/haverest/config.json
   %s -f /etc/haverest/config.json -p 9000

 CONFIG JSON:
   {
     "repo_dir":            "/var/lib/restic-repos",
     "append_only":         true,
     "global_max_parallel": 4,
     "metrics_user":        "prometheus",
     "metrics_pass":        "secret",
     "users": {
       "utente1": { "hash": "$2a$10$...", "max_mbps": 50 },
       "utente2": { "hash": "$2a$10$...", "max_mbps": 0  }
     }
   }

 SEGNALI:
   SIGHUP    Ricarica la configurazione senza riavviare
   SIGTERM   Graceful shutdown (attende max 30s)
   SIGINT    Graceful shutdown (Ctrl+C)

 REPOSITORY RESTIC:
   export RESTIC_REPOSITORY=rest:http://utente:password@localhost:8000
   export RESTIC_PASSWORD=passwordCifratura
   restic init

`, progname, progname, progname, progname, progname)
}

// ---------------------------------------------------------------------------
// MAIN
// ---------------------------------------------------------------------------

func main() {
	progname := "haverest"

	// Sostituisce il messaggio di errore di default di Go ("Usage of ...:")
	flag.Usage = func() { printUsage(progname) }

	flagConfig  := flag.String("f", "config.json", "")
	flagPort    := flag.String("p", "8000", "")
	flagVersion := flag.Bool("v", false, "")
	flagHelp    := flag.Bool("h", false, "")

	flag.Parse()

	if *flagHelp {
		printUsage(progname)
		os.Exit(0)
	}

	if *flagVersion {
		fmt.Printf("%s v%s\n", progname, VERSION)
		os.Exit(0)
	}

	// --- Banner di avvio ---
	printBanner()
	fmt.Println()

	// --- Carica config ---
	config = mustLoadConfig(*flagConfig)

	fmt.Printf(" [*] Config       : %s\n", *flagConfig)
	fmt.Printf(" [*] Repository   : %s\n", config.RepoDir)
	fmt.Printf(" [*] Append-only  : %v\n", config.AppendOnly)
	fmt.Printf(" [*] Max parallel : %d\n", config.GlobalMaxParallel)
	fmt.Printf(" [*] Utenti       : %d\n", len(config.Users))
	fmt.Printf(" [*] Metriche     : /metrics")
	if config.MetricsUser != "" {
		fmt.Printf(" (autenticato)\n")
	} else {
		fmt.Printf(" (aperto)\n")
	}
	fmt.Println()

	// --- Semaforo ---
	if config.GlobalMaxParallel <= 0 {
		config.GlobalMaxParallel = 5
	}
	backupSemaphore = make(chan struct{}, config.GlobalMaxParallel)

	// --- Router ---
	r := chi.NewRouter()
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(RequestTimeout(15 * time.Minute))

	r.With(MetricsAuthMiddleware).Handle("/metrics", promhttp.Handler())

	r.Group(func(r chi.Router) {
		r.Use(AuthMiddleware)
		r.Use(LimitConcurrency)

		r.Get("/config", handleConfigLoad)
		r.Post("/config", handleConfigSave)

		r.Route("/data/{type}", func(r chi.Router) {
			r.Get("/", handleList)
			r.Get("/{id}", handleLoad)
			r.Post("/{id}", handleSave)
			r.Delete("/{id}", handleDelete)
			r.Head("/{id}", handleHead)
		})
	})

	// --- Server HTTP ---
	addr := ":" + *flagPort
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 15 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	// --- SIGHUP: reload config a caldo ---
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			reloadConfig(*flagConfig)
		}
	}()

	// --- Graceful shutdown ---
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("[*] Listen on %s \n", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("FATAL ListenAndServe: %v", err)
		}
	}()

	<-quit
	fmt.Println()
	log.Println("[*] Shutdown (max 30s)...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[!] Error shutdown: %v", err)
	}
	log.Println("[*] Server stopped.")
}
