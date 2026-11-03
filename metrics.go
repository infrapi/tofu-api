package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The pool's metrics, on the default registry, which the server's
// /-/metrics exposes. No namespace label: namespaces are unbounded, and
// GET /-/providers lists them.
var (
	providersRunning = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "tofu_api_providers",
		Help: "Provider processes running, one per namespace with recent requests.",
	})
	providerStarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tofu_api_provider_starts_total",
		Help: "Provider processes started for a namespace, by result: ok, or failed (an unknown namespace included).",
	}, []string{"result"})
	providerStops = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tofu_api_provider_stops_total",
		Help: "Provider processes stopped, by reason: idle, max_age, evicted, gone or shutdown.",
	}, []string{"reason"})
)
