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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Tutte le metriche Prometheus sono dichiarate qui.
// promauto le registra automaticamente all'avvio.

var (
	opsProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "restic_ops_total",
		Help: "Operazioni totali per utente e tipo (save, load, list, delete...)",
	}, []string{"user", "op"})

	bytesTransferred = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "restic_bytes_total",
		Help: "Byte trasferiti per utente e direzione (up, down)",
	}, []string{"user", "dir"})

	activeRequests = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "restic_active_requests",
		Help: "Numero di richieste HTTP attive in questo momento",
	})
)
