package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// serverRateInterval mirrors rateInterval in the server's ratelimit.go: the
// ingest endpoint accepts at most one report per driver per this interval.
const serverRateInterval = 5 * time.Second

type locationReport struct {
	VehicleID string  `json:"vehicle_id"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Bearing   float64 `json:"bearing"`
	Speed     float64 `json:"speed"`
	Accuracy  float64 `json:"accuracy"`
	Timestamp int64   `json:"timestamp"`
}

type stats struct {
	succeeded   atomic.Int64
	failed      atomic.Int64
	rateLimited atomic.Int64
	totalMS     atomic.Int64
	// unauthorized is set when the server rejects a token. Every worker checks
	// it before sending, so a bad token stops the run instead of producing a
	// wall of 401s for the whole configured duration.
	unauthorized atomic.Bool
}

func main() {
	baseURL := flag.String("url", "http://localhost:8080", "Server base URL")
	numVehicles := flag.Int("vehicles", 10, "Number of simulated vehicles")
	interval := flag.Duration("interval", 10*time.Second, "Time between location reports per vehicle")
	duration := flag.Duration("duration", 5*time.Minute, "Total simulation duration (0 = run until Ctrl+C)")
	// Not defaulted to os.Getenv directly: flag defaults are echoed by -h, and
	// a JWT does not belong in help output.
	tokenList := flag.String("token", "", "Driver JWT(s) from POST /api/v1/auth/login, comma-separated (falls back to $SIM_TOKEN)")
	flag.Parse()

	if *tokenList == "" {
		*tokenList = os.Getenv("SIM_TOKEN")
	}

	if *numVehicles <= 0 {
		log.Fatal("vehicles must be positive")
	}
	if *interval <= 0 {
		log.Fatal("interval must be positive")
	}

	tokens := parseTokens(*tokenList)
	if len(tokens) == 0 {
		log.Fatal("no token supplied: POST /api/v1/locations requires a driver JWT.\n" +
			"Obtain one with:\n" +
			"  curl -s -X POST http://localhost:8080/api/v1/auth/login \\\n" +
			"    -H 'Content-Type: application/json' \\\n" +
			"    -d '{\"email\":\"driver@example.com\",\"password\":\"...\"}'\n" +
			"then pass it via -token or $SIM_TOKEN.")
	}

	// The server rate limits per driver, not per vehicle, so vehicles sharing a
	// token share one bucket. Warn rather than fail: a 429-heavy run is still a
	// valid way to exercise the limiter.
	offered := float64(*numVehicles) / interval.Seconds()
	allowed := float64(len(tokens)) / serverRateInterval.Seconds()
	if offered > allowed {
		log.Printf("warning: %d vehicles every %s offers %.2f reports/sec, but %d token(s) allow only %.2f/sec "+
			"(server limit: 1 per %s per driver) — expect 429s. Supply more tokens or raise -interval.",
			*numVehicles, *interval, offered, len(tokens), allowed, serverRateInterval)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}

	client := &http.Client{Timeout: 10 * time.Second}
	s := &stats{}

	log.Printf("starting simulator: %d vehicles, %d token(s), interval=%s, duration=%s", *numVehicles, len(tokens), *interval, *duration)

	var wg sync.WaitGroup
	for i := 0; i < *numVehicles; i++ {
		wg.Add(1)
		vehicleID := fmt.Sprintf("sim-vehicle-%03d", i+1)
		route := routes[i%len(routes)]
		token := tokens[i%len(tokens)]
		go func() {
			defer wg.Done()
			simulateVehicle(ctx, client, *baseURL, token, vehicleID, route, *interval, s)
		}()
	}
	wg.Wait()

	ok := s.succeeded.Load()
	fail := s.failed.Load()
	limited := s.rateLimited.Load()
	avgMS := int64(0)
	if ok > 0 {
		avgMS = s.totalMS.Load() / ok
	}
	log.Printf("simulation complete: %d requests, %d ok, %d failed, %d rate limited, avg=%dms",
		ok+fail+limited, ok, fail, limited, avgMS)
	if s.unauthorized.Load() {
		log.Fatal("aborted: the server rejected the supplied token (401)")
	}
}

// parseTokens splits a comma-separated token list, discarding empty entries so
// a trailing comma or an unset $SIM_TOKEN does not yield a blank token.
func parseTokens(list string) []string {
	var tokens []string
	for _, t := range strings.Split(list, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	return tokens
}

func simulateVehicle(ctx context.Context, client *http.Client, baseURL, token, vehicleID string, route []Waypoint, interval time.Duration, s *stats) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	waypointIdx := 0
	segmentStart := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if s.unauthorized.Load() {
				return
			}

			from := route[waypointIdx]
			to := route[(waypointIdx+1)%len(route)]

			segmentDist := haversineDistance(from, to)
			segmentDuration := segmentDist / 8.0 // assume ~8 m/s (~29 km/h, realistic urban bus)
			if segmentDuration <= 0 {
				segmentDuration = 1
			}

			elapsed := now.Sub(segmentStart).Seconds()
			t := elapsed / segmentDuration
			if t >= 1.0 {
				waypointIdx = (waypointIdx + 1) % len(route)
				segmentStart = now
				t = 0
				from = route[waypointIdx]
				to = route[(waypointIdx+1)%len(route)]
				segmentDist = haversineDistance(from, to)
				segmentDuration = segmentDist / 8.0
				if segmentDuration <= 0 {
					segmentDuration = 1
				}
			}

			pos := interpolate(from, to, t)
			brng := bearing(from, to)
			spd := speed(segmentDist, segmentDuration)

			report := locationReport{
				VehicleID: vehicleID,
				Latitude:  pos.Lat,
				Longitude: pos.Lon,
				Bearing:   brng,
				Speed:     spd,
				Accuracy:  5.0, // assume ~5m GPS accuracy for simulated reports
				Timestamp: now.Unix(),
			}

			sendReport(ctx, client, baseURL, token, vehicleID, &report, s)
		}
	}
}

func sendReport(ctx context.Context, client *http.Client, baseURL, token, vehicleID string, report *locationReport, s *stats) {
	body, err := json.Marshal(report)
	if err != nil {
		log.Printf("%s: marshal error: %v", vehicleID, err)
		s.failed.Add(1)
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/locations", bytes.NewReader(body))
	if err != nil {
		log.Printf("%s: request error: %v", vehicleID, err)
		s.failed.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)

	if err != nil {
		if ctx.Err() != nil {
			return // clean shutdown, not a real failure
		}
		log.Printf("%s: POST failed: %v", vehicleID, err)
		s.failed.Add(1)
		return
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusCreated:
		io.Copy(io.Discard, resp.Body)
		s.succeeded.Add(1)
		s.totalMS.Add(latency.Milliseconds())
		log.Printf("%s: POST %d (%dms)", vehicleID, resp.StatusCode, latency.Milliseconds())
	case http.StatusTooManyRequests:
		// Expected when vehicles share a token; tracked separately so it does
		// not read as a server or payload error in the summary.
		io.Copy(io.Discard, resp.Body)
		s.rateLimited.Add(1)
		log.Printf("%s: POST 429 (%dms): rate limited", vehicleID, latency.Milliseconds())
	case http.StatusUnauthorized:
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		s.failed.Add(1)
		s.unauthorized.Store(true)
		log.Printf("%s: POST 401 (%dms): %s", vehicleID, latency.Milliseconds(), string(bodyBytes))
	default:
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		s.failed.Add(1)
		log.Printf("%s: POST %d (%dms): %s", vehicleID, resp.StatusCode, latency.Milliseconds(), string(bodyBytes))
	}
}
