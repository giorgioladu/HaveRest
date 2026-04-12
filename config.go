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
 
 /* config.json 
		  {
			"repo_dir": "/var/lib/restic-repos",
			"append_only": true,
			"global_max_parallel": 4,
			"metrics_user": "prometheus",
			"metrics_pass": "passwordMetricsInChiaro",
			"users": {
				"user1": {
					"hash": "$2a$10$X................",
					"max_mbps": 50
				},
				"server2": {
					"hash": "$2a$10$X.................",
					"max_mbps": 0 // 0 = no limits!!
					"max_bytes": 0 // 0 = nessun limite
				}
			}
		}

*/
 
 
package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
)

// ---------------------------------------------------------------------------
// STRUTTURE DATI
// ---------------------------------------------------------------------------

type UserEntry struct {
	Hash    string `json:"hash"`
	MaxMbps int    `json:"max_mbps"`
	MaxBytes int64  `json:"max_bytes"` // 0 = nessun limite
}

type Config struct {
	RepoDir           string               `json:"repo_dir"`
	AppendOnly        bool                 `json:"append_only"`
	GlobalMaxParallel int                  `json:"global_max_parallel"`
	MetricsUser       string               `json:"metrics_user"`
	MetricsPass       string               `json:"metrics_pass"`
	Users             map[string]UserEntry `json:"users"`
}

// ---------------------------------------------------------------------------
// STATO GLOBALE CONFIG
// ---------------------------------------------------------------------------

var (
	config   Config
	configMu sync.RWMutex
	
   // Semaforo per limitare le operazioni pesanti concorrenti
    backupSemaphore chan struct{}
)

// ---------------------------------------------------------------------------
// FUNZIONI
// ---------------------------------------------------------------------------

// loadConfig legge e deserializza config.json dal path indicato.
func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// mustLoadConfig è usata all'avvio: fa crash esplicito se la config è assente
// o malformata, invece di partire silenziosamente con uno stato vuoto.
func mustLoadConfig(path string) Config {
	cfg, err := loadConfig(path)
	if err != nil {
		log.Fatalf("FATAL: impossibile leggere config %q: %v", path, err)
	}
	if cfg.RepoDir == "" {
		log.Fatal("FATAL: repo_dir mancante in config.json")
	}
	if len(cfg.Users) == 0 {
		log.Fatal("FATAL: nessun utente definito in config.json")
	}
	return cfg
}

// getConfig restituisce una copia thread-safe della config corrente.
// Gli handler devono sempre usare questa funzione, mai accedere a config direttamente.
func getConfig() Config {
	configMu.RLock()
	defer configMu.RUnlock()
	return config
}

// reloadConfig ricarica la config da disco e la sostituisce atomicamente.
// Chiamata dal goroutine che ascolta SIGHUP.
func reloadConfig(path string) {
	newCfg, err := loadConfig(path)
	if err != nil {
		log.Printf("WARN reload: config non ricaricata: %v", err)
		return
	}
	configMu.Lock()
	config = newCfg
	configMu.Unlock()
	initQuotas(newCfg) 
	log.Printf("INFO Config ricaricata: %d utenti", len(newCfg.Users))
}
