package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const reservoirLimit = 100000

type Summary struct {
	URL                   string  `json:"url"`
	Method                string  `json:"method"`
	Duration              string  `json:"duration"`
	ElapsedSeconds        float64 `json:"elapsed_seconds"`
	Concurrency           int     `json:"concurrency"`
	TotalRequests         uint64  `json:"total_requests"`
	RequestsPerSecond     float64 `json:"requests_per_second"`
	Errors                uint64  `json:"errors"`
	Non2xx                uint64  `json:"non_2xx"`
	LatencySamples        uint64  `json:"latency_samples"`
	LatencyReservoirLimit int     `json:"latency_reservoir_limit"`
	LatencyP50MS          float64 `json:"latency_p50_ms"`
	LatencyP90MS          float64 `json:"latency_p90_ms"`
	LatencyP99MS          float64 `json:"latency_p99_ms"`
	LatencyMaxMS          float64 `json:"latency_max_ms"`
}

type collector struct {
	mu       sync.Mutex
	rng      *rand.Rand
	total    uint64
	errors   uint64
	non2xx   uint64
	max      time.Duration
	latency  []time.Duration
	observed uint64
}

func newCollector(limit int) *collector {
	if limit <= 0 {
		limit = reservoirLimit
	}
	return &collector{
		rng:     rand.New(rand.NewSource(time.Now().UnixNano())),
		latency: make([]time.Duration, 0, limit),
	}
}

func (c *collector) record(latency time.Duration, status int, requestErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	if requestErr != nil {
		c.errors++
	}
	if status != 0 && (status < http.StatusOK || status >= http.StatusMultipleChoices) {
		c.non2xx++
	}
	if latency > c.max {
		c.max = latency
	}
	c.observed++
	if len(c.latency) < cap(c.latency) {
		c.latency = append(c.latency, latency)
		return
	}
	index := c.rng.Int63n(int64(c.observed))
	if index < int64(cap(c.latency)) {
		c.latency[index] = latency
	}
}

func (c *collector) summary(url, method string, duration time.Duration, elapsed time.Duration, concurrency int) Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	samples := append([]time.Duration(nil), c.latency...)
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	total := c.total
	seconds := elapsed.Seconds()
	requestsPerSecond := 0.0
	if seconds > 0 {
		requestsPerSecond = floorDecimal(float64(total)/seconds, 3)
	}
	return Summary{
		URL:                   url,
		Method:                method,
		Duration:              duration.String(),
		ElapsedSeconds:        floorDecimal(seconds, 6),
		Concurrency:           concurrency,
		TotalRequests:         total,
		RequestsPerSecond:     requestsPerSecond,
		Errors:                c.errors,
		Non2xx:                c.non2xx,
		LatencySamples:        uint64(len(samples)),
		LatencyReservoirLimit: cap(c.latency),
		LatencyP50MS:          percentileMS(samples, 0.50),
		LatencyP90MS:          percentileMS(samples, 0.90),
		LatencyP99MS:          percentileMS(samples, 0.99),
		LatencyMaxMS:          floorDecimal(float64(c.max)/float64(time.Millisecond), 3),
	}
}

func percentile(samples []time.Duration, quantile float64) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	if quantile <= 0 {
		return samples[0]
	}
	if quantile >= 1 {
		return samples[len(samples)-1]
	}
	index := int(math.Ceil(quantile*float64(len(samples)))) - 1
	if index < 0 {
		index = 0
	}
	return samples[index]
}

func percentileMS(samples []time.Duration, quantile float64) float64 {
	return floorDecimal(float64(percentile(samples, quantile))/float64(time.Millisecond), 3)
}

func floorDecimal(value float64, digits int) float64 {
	scale := math.Pow10(digits)
	return math.Floor(value*scale) / scale
}

func runPhase(client *http.Client, target, method string, concurrency int, duration time.Duration, results *collector) time.Duration {
	start := time.Now()
	stop := make(chan struct{})
	timer := time.NewTimer(duration)
	var workers sync.WaitGroup
	workers.Add(concurrency)
	for range concurrency {
		go func() {
			defer workers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				requestStart := time.Now()
				req, err := http.NewRequest(method, target, nil)
				if err != nil {
					if results != nil {
						results.record(time.Since(requestStart), 0, err)
					}
					return
				}
				resp, err := client.Do(req)
				status := 0
				if resp != nil {
					status = resp.StatusCode
					if resp.Body != nil {
						if _, readErr := io.Copy(io.Discard, resp.Body); err == nil && readErr != nil {
							err = readErr
						}
						if closeErr := resp.Body.Close(); err == nil && closeErr != nil {
							err = closeErr
						}
					}
				}
				if results != nil {
					results.record(time.Since(requestStart), status, err)
				}
			}
		}()
	}
	<-timer.C
	close(stop)
	timer.Stop()
	workers.Wait()
	return time.Since(start)
}

func run(urlValue, method string, duration, warmup time.Duration, concurrency int) (Summary, error) {
	parsed, err := url.ParseRequestURI(urlValue)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return Summary{}, fmt.Errorf("invalid --url %q", urlValue)
	}
	if concurrency <= 0 {
		return Summary{}, errors.New("--concurrency must be positive")
	}
	if duration <= 0 {
		return Summary{}, errors.New("--duration must be positive")
	}
	if warmup < 0 {
		return Summary{}, errors.New("--warmup cannot be negative")
	}
	if _, err := http.NewRequest(method, urlValue, nil); err != nil {
		return Summary{}, fmt.Errorf("invalid --method: %w", err)
	}
	transport := &http.Transport{
		MaxIdleConns:        concurrency,
		MaxIdleConnsPerHost: concurrency,
		IdleConnTimeout:     90 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	if warmup > 0 {
		runPhase(client, urlValue, method, concurrency, warmup, nil)
	}
	results := newCollector(reservoirLimit)
	elapsed := runPhase(client, urlValue, method, concurrency, duration, results)
	return results.summary(urlValue, method, duration, elapsed, concurrency), nil
}

func writeSummary(summary Summary, format string) error {
	switch strings.ToLower(format) {
	case "json":
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(summary)
	case "markdown", "md":
		_, err := fmt.Fprintf(os.Stdout,
			"| URL | Method | Duration | Concurrency | Requests | Requests/s | Errors | Non-2xx | p50 ms | p90 ms | p99 ms | max ms |\n"+
				"|---|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---:|\n"+
				"| %s | %s | %s | %d | %d | %.3f | %d | %d | %.3f | %.3f | %.3f | %.3f |\n",
			summary.URL, summary.Method, summary.Duration, summary.Concurrency,
			summary.TotalRequests, summary.RequestsPerSecond, summary.Errors, summary.Non2xx,
			summary.LatencyP50MS, summary.LatencyP90MS, summary.LatencyP99MS, summary.LatencyMaxMS)
		return err
	default:
		return fmt.Errorf("unsupported --format %q; use json or markdown", format)
	}
}

func main() {
	target := flag.String("url", "http://127.0.0.1:8080/", "request URL")
	duration := flag.Duration("duration", 10*time.Second, "measured run duration")
	concurrency := flag.Int("concurrency", 64, "number of concurrent workers")
	warmup := flag.Duration("warmup", 5*time.Second, "warmup duration excluded from results")
	method := flag.String("method", http.MethodGet, "HTTP method")
	format := flag.String("format", "json", "summary format: json or markdown")
	freePort := flag.Bool("free-port", false, "print a currently free loopback TCP port for benchmark scripts")
	flag.Parse()
	if *freePort {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(listener.Addr().(*net.TCPAddr).Port)
		_ = listener.Close()
		return
	}
	summary, err := run(*target, *method, *duration, *warmup, *concurrency)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := writeSummary(summary, *format); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}
