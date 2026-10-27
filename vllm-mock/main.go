// vllm-mock is a minimal HTTP server that imitates the public surface
// of a vLLM inference endpoint (streaming /v1/chat/completions, plus a
// Prometheus /metrics endpoint that exposes the vllm:* metric families
// a real vLLM 0.6.x runtime emits).
//
// The point of the mock is to be able to validate SLO rules, burn-rate
// alerts and Grafana dashboards without paying for a GPU. Metric shapes
// are identical to real vLLM; distributions are tunable via env vars
// so the demo can inject latency spikes and errors on demand.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Tunable distributions. Defaults are chosen to match a healthy
	// 7B-13B model on a single NVIDIA T4 under moderate load.
	ttftMedianMs  = envFloat("TTFT_MEDIAN_MS", 50)   // log-normal median
	ttftSigma     = envFloat("TTFT_SIGMA", 1.0)      // log-normal sigma
	tpotMedianMs  = envFloat("TPOT_MEDIAN_MS", 20)   // log-normal median
	tpotSigma     = envFloat("TPOT_SIGMA", 0.7)      // log-normal sigma
	errorRatePct  = envFloat("ERROR_RATE_PCT", 0.0)  // 0.0..100.0
	degradedMode  = os.Getenv("DEGRADED") == "true"  // injects +500ms TTFT bias
	modelName     = envStr("MODEL_NAME", "mistral-7b-instruct")

	// Counters we need to track outside Prometheus too, for the gauges.
	inflightRequests atomic.Int64
	waitingRequests  atomic.Int64

	// Simulated KV cache utilization. Climbs under load, decays idle.
	cacheMu    sync.Mutex
	cacheUsage float64
)

var (
	ttftHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vllm:time_to_first_token_seconds",
			Help:    "Time from request receipt to first output token, in seconds.",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 0.75, 1.0, 2.0, 5.0, 10.0, 30.0},
		},
		[]string{"model"},
	)
	tpotHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vllm:time_per_output_token_seconds",
			Help:    "Time per output token during the decode phase, in seconds.",
			Buckets: []float64{0.005, 0.01, 0.02, 0.03, 0.05, 0.075, 0.1, 0.15, 0.25, 0.5, 1.0, 2.0, 5.0},
		},
		[]string{"model"},
	)
	requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vllm:request_total",
			Help: "Total inference requests received.",
		},
		[]string{"model"},
	)
	requestsSuccessTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vllm:request_success_total",
			Help: "Inference requests that completed successfully (status label mirrors real vLLM).",
		},
		[]string{"model", "status"},
	)
	runningRequests = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "vllm:num_requests_running",
			Help: "Current number of requests being generated.",
		},
		func() float64 { return float64(inflightRequests.Load()) },
	)
	waitingRequestsGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "vllm:num_requests_waiting",
			Help: "Current number of requests waiting for a slot in the scheduler.",
		},
		func() float64 { return float64(waitingRequests.Load()) },
	)
	gpuCacheUsage = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "vllm:gpu_cache_usage_perc",
			Help: "Fraction of GPU KV cache in use (0..1).",
		},
		func() float64 {
			cacheMu.Lock()
			defer cacheMu.Unlock()
			return cacheUsage
		},
	)
	outputTokensTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vllm:generation_tokens_total",
			Help: "Total output tokens generated.",
		},
		[]string{"model"},
	)
)

func init() {
	prometheus.MustRegister(
		ttftHistogram, tpotHistogram, requestsTotal, requestsSuccessTotal,
		runningRequests, waitingRequestsGauge, gpuCacheUsage, outputTokensTotal,
	)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Background cache-decay loop so the gauge drifts realistically.
	go cacheDecayLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/v1/chat/completions", handleChatCompletions)
	mux.Handle("/metrics", promhttp.Handler())

	addr := envStr("LISTEN_ADDR", ":8000")
	logger.Info("vllm-mock listening",
		"addr", addr,
		"model", modelName,
		"ttft_median_ms", ttftMedianMs,
		"tpot_median_ms", tpotMedianMs,
		"error_rate_pct", errorRatePct,
		"degraded", degradedMode,
	)
	if err := http.ListenAndServe(addr, mux); err != nil {
		logger.Error("server exited", "err", err)
		os.Exit(1)
	}
}

func handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	requestsTotal.WithLabelValues(modelName).Inc()

	// Queue simulation: pretend to wait briefly when inflight > 4.
	for inflightRequests.Load() >= 4 {
		waitingRequests.Add(1)
		time.Sleep(50 * time.Millisecond)
		waitingRequests.Add(-1)
	}

	inflightRequests.Add(1)
	defer inflightRequests.Add(-1)

	// Inject errors per ERROR_RATE_PCT.
	if rand.Float64()*100 < errorRatePct {
		requestsSuccessTotal.WithLabelValues(modelName, "failure").Inc()
		http.Error(w, "simulated inference failure", http.StatusInternalServerError)
		slog.Warn("simulated failure", "model", modelName)
		return
	}

	// TTFT: log-normal around median, with degraded-mode bias.
	ttft := logNormalMs(ttftMedianMs, ttftSigma)
	if degradedMode {
		ttft += 500
	}
	time.Sleep(time.Duration(ttft) * time.Millisecond)
	ttftHistogram.WithLabelValues(modelName).Observe(ttft / 1000.0)

	// Simulate output: 50..500 tokens, each with a TPOT sample.
	nTokens := 50 + rand.IntN(451)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	for i := 0; i < nTokens; i++ {
		tpot := logNormalMs(tpotMedianMs, tpotSigma)
		time.Sleep(time.Duration(tpot) * time.Millisecond)
		tpotHistogram.WithLabelValues(modelName).Observe(tpot / 1000.0)
		outputTokensTotal.WithLabelValues(modelName).Inc()
		if flusher != nil {
			chunk := map[string]any{
				"id":      "chatcmpl-mock",
				"object":  "chat.completion.chunk",
				"model":   modelName,
				"choices": []map[string]any{{"delta": map[string]string{"content": "x "}}},
			}
			b, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		// Nudge cache usage up.
		cacheMu.Lock()
		cacheUsage = math.Min(0.99, cacheUsage+0.0005)
		cacheMu.Unlock()
	}
	if flusher != nil {
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}
	requestsSuccessTotal.WithLabelValues(modelName, "success").Inc()
}

// cacheDecayLoop drifts the simulated KV cache usage down over time
// when the service is idle so the gauge is not stuck at 1.0.
func cacheDecayLoop() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	for range t.C {
		cacheMu.Lock()
		cacheUsage = math.Max(0, cacheUsage-0.003)
		cacheMu.Unlock()
	}
}

// logNormalMs draws a sample from a log-normal distribution with the
// given median (in ms) and sigma. Returns milliseconds.
func logNormalMs(medianMs, sigma float64) float64 {
	mu := math.Log(medianMs)
	return math.Exp(mu + sigma*rand.NormFloat64())
}

func envFloat(k string, def float64) float64 {
	if v := os.Getenv(k); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	return def
}

func envStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
