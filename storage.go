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
	"context"
	"io"
	"path/filepath"
	"sync"

	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// BUFFER POOL
// ---------------------------------------------------------------------------

// bufferPool riduce le allocazioni durante i trasferimenti di dati.
// Ogni goroutine prende un buffer da 64KB, lo usa, e lo rimette nel pool.
var bufferPool = sync.Pool{
	New: func() interface{} { return make([]byte, 64*1024) },
}

// ---------------------------------------------------------------------------
// RATE LIMITING
// ---------------------------------------------------------------------------

var (
	limiters   = make(map[string]*rate.Limiter)
	limitersMu sync.RWMutex
)

// getLimiter restituisce il rate limiter per un utente, creandolo se necessario.
// Il limiter è condiviso tra upload e download dello stesso utente.
func getLimiter(user string, mbps int) *rate.Limiter {
	limitersMu.Lock()
	defer limitersMu.Unlock()
	if l, ok := limiters[user]; ok {
		return l
	}
	bytesPerSec := rate.Limit(mbps * 1024 * 1024 / 8)
	l := rate.NewLimiter(bytesPerSec, 1024*1024) // burst: 1 MB
	limiters[user] = l
	return l
}

// ThrottledReader applica il rate limiting su qualsiasi io.Reader.
// Funziona sia per upload (r.Body) che per download (os.File).
type ThrottledReader struct {
	r   io.Reader
	l   *rate.Limiter
	ctx context.Context
}

func (t *ThrottledReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		if waitErr := t.l.WaitN(t.ctx, n); waitErr != nil {
			return n, waitErr
		}
	}
	return n, err
}

// throttle avvolge un reader con il ThrottledReader se l'utente ha un limite
// configurato, altrimenti restituisce il reader originale invariato.
func throttle(r io.Reader, user string, cfg Config, ctx context.Context) io.Reader {
	entry := cfg.Users[user]
	if entry.MaxMbps <= 0 {
		return r
	}
	return &ThrottledReader{r: r, l: getLimiter(user, entry.MaxMbps), ctx: ctx}
}

// ---------------------------------------------------------------------------
// PATH RESOLUTION
// ---------------------------------------------------------------------------

// getPath costruisce il percorso su disco di un oggetto restic.
// Restic organizza i pack file in sottodirectory a 2 caratteri (es. "ab/abcdef...")
// per evitare directory con troppi file su filesystem lenti.
func getPath(repoDir, user, bType, id string) string {
	if bType == "config" {
		return filepath.Join(repoDir, user, "config")
	}
	if len(id) > 2 {
		return filepath.Join(repoDir, user, bType, id[:2], id)
	}
	return filepath.Join(repoDir, user, bType, id)
}
