package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/moonrhythm/parapet"
	"github.com/moonrhythm/parapet/pkg/location"
	"github.com/moonrhythm/parapet/pkg/logger"
	"github.com/moonrhythm/parapet/pkg/prom"
	"github.com/moonrhythm/parapet/pkg/stripprefix"
	"github.com/moonrhythm/parapet/pkg/upstream"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	healthyDuration time.Duration

	lbUpstream lb
	lbFallback lb
	lbWs       lb
)

func main() {
	var (
		addr                        = flag.String("addr", ":80", "HTTP address")
		logEnable                   = flag.Bool("log", true, "Enable request log")
		tlsAddr                     = flag.String("tls.addr", "", "HTTPS address")
		tlsKey                      = flag.String("tls.key", "", "TLS private key file")
		tlsCert                     = flag.String("tls.cert", "", "TLS certificate file")
		upstreamList                = flag.String("upstream", "", "Upstream list")
		gethHealthyDuration         = flag.Duration("healthy-duration", time.Minute, "duration from last block that mark as healthy")
		healthCheckDeadline         = flag.Duration("health-check.deadline", 4*time.Second, "deadline when run health check")
		healthCheckInterval         = flag.Duration("health-check.interval", 2*time.Second, "health check interval")
		fallbackUpstreamList        = flag.String("fallback.upstream", "", "fallback upstream list")
		fallbackHealthCheckDeadline = flag.Duration("fallback.health-check.deadline", 5*time.Second, "fallback deadline when run health check")
		fallbackHealthCheckInterval = flag.Duration("fallback.health-check.interval", 5*time.Second, "fallback health check interval")
	)

	flag.Parse()

	log.Printf("geth-gateway")
	log.Printf("HTTP address: %s", *addr)
	log.Printf("HTTPS address: %s", *tlsAddr)
	log.Printf("Upstream: %s", *upstreamList)
	log.Printf("Healthy Duration: %s", *gethHealthyDuration)
	log.Printf("Health Check Deadline: %s", *healthCheckDeadline)
	log.Printf("Health Check Interval: %s", *healthCheckInterval)
	log.Printf("Fallback Upstream: %s", *fallbackUpstreamList)
	log.Printf("Fallback Health Check Deadline: %s", *fallbackHealthCheckDeadline)
	log.Printf("Fallback Health Check Interval: %s", *fallbackHealthCheckInterval)

	healthyDuration = *gethHealthyDuration
	lbUpstream.fallback = &lbFallback

	prom.Registry().MustRegister(headDuration, headNumber)
	go prom.Start(":6060")

	for _, addr := range strings.Split(*upstreamList, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}

		u, err := url.Parse(addr)
		if err != nil {
			log.Fatalf("can not parse url; %v", err)
		}

		if u.Scheme == "ws" || u.Scheme == "wss" {
			u.Scheme = "http" + strings.TrimPrefix(u.Scheme, "ws") // http or https
			lbWs.addr = append(lbWs.addr, addr)
			lbWs.urls = append(lbWs.urls, u)
			lbWs.blocks = append(lbWs.blocks, lastBlock{})
		} else {
			lbUpstream.addr = append(lbUpstream.addr, addr)
			lbUpstream.urls = append(lbUpstream.urls, u)
			lbUpstream.blocks = append(lbUpstream.blocks, lastBlock{})
		}
	}

	for _, addr := range strings.Split(*fallbackUpstreamList, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}

		u, err := url.Parse(addr)
		if err != nil {
			log.Fatalf("can not parse url; %v", err)
		}

		lbFallback.addr = append(lbFallback.addr, addr)
		lbFallback.urls = append(lbFallback.urls, u)
		lbFallback.blocks = append(lbFallback.blocks, lastBlock{})
	}

	{
		lbs := []*lb{&lbUpstream, &lbWs, &lbFallback}

		ctx := context.Background()
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)

		var wg sync.WaitGroup
		for _, x := range lbs {
			x := x

			if len(x.addr) == 0 {
				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				x.updateLastBlock(ctx)
			}()
		}
		wg.Wait()
		cancel()
	}

	go lbUpstream.runUpdateLoop(*healthCheckInterval, *healthCheckDeadline)
	go lbWs.runUpdateLoop(*healthCheckInterval, *healthCheckDeadline)
	go lbFallback.runUpdateLoop(*fallbackHealthCheckInterval, *fallbackHealthCheckDeadline)

	var s parapet.Middlewares

	if *logEnable {
		s.Use(logger.Stdout())
	}
	s.Use(prom.Requests())

	// healthz
	{
		l := location.Exact("/healthz")
		l.Use(parapet.Handler(healthz))
		s.Use(l)
	}

	// upstreams
	{
		l := location.Exact("/upstreams")
		l.Use(parapet.Handler(lbUpstream.upstreamsHandler))
		s.Use(l)
	}

	// ws upstreams
	{
		l := location.Exact("/wsupstreams")
		l.Use(parapet.Handler(lbWs.upstreamsHandler))
		s.Use(l)
	}

	{
		l := location.Exact("/ws")
		l.Use(stripprefix.New("/ws"))
		l.Use(upstream.New(&lbWs))
		s.Use(l)
	}

	// http
	s.Use(upstream.New(&lbUpstream))

	var wg sync.WaitGroup

	if *addr != "" {
		wg.Add(1)
		srv := parapet.NewBackend()
		srv.Addr = *addr
		srv.GraceTimeout = 3 * time.Second
		srv.WaitBeforeShutdown = 0
		srv.Use(s)
		prom.Connections(srv)
		prom.Networks(srv)
		go func() {
			defer wg.Done()

			err := srv.ListenAndServe()
			if err != nil {
				log.Fatalf("can not start server; %v", err)
			}
		}()
	}

	if *tlsAddr != "" {
		wg.Add(1)
		srv := parapet.NewBackend()
		srv.Addr = *tlsAddr
		srv.GraceTimeout = 3 * time.Second
		srv.WaitBeforeShutdown = 0
		srv.TLSConfig = &tls.Config{}

		if *tlsKey == "" || *tlsCert == "" {
			cert, err := parapet.GenerateSelfSignCertificate(parapet.SelfSign{
				CommonName: "geth-gateway",
				Hosts:      []string{"geth-gateway"},
				NotBefore:  time.Now().Add(-5 * time.Minute),
				NotAfter:   time.Now().AddDate(10, 0, 0),
			})
			if err != nil {
				log.Fatalf("can not generate self signed cert; %v", err)
			}
			srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)
		} else {
			cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
			if err != nil {
				log.Fatalf("can not load x509 key pair; %v", err)
			}
			srv.TLSConfig.Certificates = append(srv.TLSConfig.Certificates, cert)
		}

		srv.Use(s)
		prom.Connections(srv)
		prom.Networks(srv)
		go func() {
			defer wg.Done()

			err := srv.ListenAndServe()
			if err != nil {
				log.Fatalf("can not start server; %v", err)
			}
		}()
	}

	wg.Wait()
}

func isReady() bool {
	lbUpstream.mu.RLock()
	block := lbUpstream.block
	lbUpstream.mu.RUnlock()

	t := time.Unix(int64(block.Time), 0)
	return time.Since(t) <= healthyDuration
}

func isLive() bool {
	lbUpstream.mu.RLock()
	ok := len(lbUpstream.bestURLs) > 0
	updated := lbUpstream.updated
	lbUpstream.mu.RUnlock()

	return ok && time.Since(updated) <= 10*time.Second
}

func healthz(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("ready") == "1" {
		if !isReady() {
			http.Error(w, "not ready", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ready"))
		return
	}

	if !isLive() {
		http.Error(w, "not ok", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

type lb struct {
	mu       sync.RWMutex
	addr     []string
	urls     []*url.URL
	bestURLs []*url.URL
	block    *types.Header
	updated  time.Time
	blocks   []lastBlock

	fallback *lb

	// tr
	i uint32
}

type lastBlock struct {
	mu        sync.Mutex
	Block     *types.Header
	UpdatedAt time.Time
}

func getLastBlock(ctx context.Context, client *ethclient.Client, b *lastBlock, force bool) (*types.Header, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if !force && time.Since(b.UpdatedAt) < time.Second {
		return b.Block, nil
	}

	block, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		return b.Block, err
	}
	b.Block = block
	b.UpdatedAt = time.Now()
	return b.Block, nil
}

const promNamespace = "geth_gateway"

var headDuration = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: promNamespace,
	Name:      "head_duration_seconds",
}, []string{"upstream"})

var headNumber = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: promNamespace,
	Name:      "head_number",
}, []string{"upstream"})

func (lb *lb) updateLastBlock(ctx context.Context) {
	blockNumbers := make([]uint64, len(lb.blocks))
	blockData := make([]*types.Header, len(lb.blocks))

	var wg sync.WaitGroup
	for i := range lb.blocks {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()

			client, err := getClient(ctx, lb.addr[i])
			if err != nil {
				return
			}

			block, _ := getLastBlock(ctx, client, &lb.blocks[i], true)
			if block == nil {
				return
			}

			blockNumbers[i] = block.Number.Uint64()
			blockData[i] = block

			t := time.Unix(int64(block.Time), 0)
			diff := time.Since(t)

			g, err := headDuration.GetMetricWith(prometheus.Labels{
				"upstream": lb.addr[i],
			})
			if err == nil {
				g.Set(float64(diff) / float64(time.Second))
			}

			g, err = headNumber.GetMetricWith(prometheus.Labels{
				"upstream": lb.addr[i],
			})
			if err == nil {
				g.Set(float64(block.Number.Uint64()))
			}
		}()
	}
	wg.Wait()

	h := highestBlock(blockNumbers)

	// collect all best block rpc
	best := make([]*url.URL, 0, len(blockNumbers))
	var bestIndex int
	for i, b := range blockNumbers {
		if b == h {
			best = append(best, lb.urls[i])
			bestIndex = i
		}
	}

	lb.mu.Lock()
	lb.bestURLs = best
	lb.block = blockData[bestIndex]
	lb.updated = time.Now()
	lb.mu.Unlock()
}

func (lb *lb) runUpdateLoop(interval, deadline time.Duration) {
	if len(lb.addr) == 0 {
		return
	}

	for {
		ctx, cancel := context.WithTimeout(context.Background(), deadline)
		lb.updateLastBlock(ctx)
		cancel()

		time.Sleep(interval)
	}
}

func highestBlock(blockNumbers []uint64) uint64 {
	max := blockNumbers[0]
	for _, x := range blockNumbers {
		if x > max {
			max = x
		}
	}
	return max
}

var (
	trs = map[string]http.RoundTripper{
		"http": &upstream.HTTPTransport{
			MaxIdleConns: 500,
		},
		"https": &upstream.HTTPSTransport{
			MaxIdleConns:    500,
			MaxConn:         2000,
			IdleConnTimeout: 30 * time.Second,
		},
	}
)

func (lb *lb) roundTrip(r *http.Request) (*http.Response, error) {
	lb.mu.RLock()
	targets := lb.bestURLs
	lb.mu.RUnlock()

	if len(targets) == 0 {
		return nil, upstream.ErrUnavailable
	}

	i := atomic.AddUint32(&lb.i, 1) - 1
	i %= uint32(len(targets))
	t := targets[i]

	r.URL.Scheme = t.Scheme
	r.URL.Host = t.Host
	r.URL.Path = path.Join(t.Path, r.URL.Path)
	r.Host = t.Host
	return trs[t.Scheme].RoundTrip(r)
}

func (lb *lb) roundTripWithFallback(r *http.Request) (*http.Response, error) {
	if lb.isFallback() {
		return lb.fallback.RoundTrip(r)
	}
	return lb.roundTrip(r)
}

func isResponseRetryable(resp *http.Response) bool {
	if resp == nil {
		return true
	}
	if resp.StatusCode >= 500 {
		return true
	}
	return false
}

func (lb *lb) RoundTrip(r *http.Request) (resp *http.Response, err error) {
	for i := 0; i < 3; i++ {
		resp, err = lb.roundTripWithFallback(r)
		if err == nil && !isResponseRetryable(resp) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}
	return
}

func (lb *lb) isFallback() bool {
	if lb.fallback == nil {
		return false
	}

	lb.mu.RLock()
	block := lb.block
	lb.mu.RUnlock()

	if block == nil {
		// lb don't have any block
		return true
	}

	t := time.Unix(int64(block.Time), 0)
	ready := time.Since(t) <= healthyDuration

	// our lb is not ready but let check is the fallback is faster than us
	if !ready {
		lb.fallback.mu.RLock()
		fallbackBlock := lb.fallback.block
		lb.fallback.mu.RUnlock()

		if fallbackBlock == nil {
			// fallback not available
			return false
		}
		ready = block.Time >= fallbackBlock.Time
	}

	return !ready
}

type upstreamsResult struct {
	Upstreams []string `json:"upstreams"`
	Block     struct {
		Number   uint64 `json:"number"`
		Duration string `json:"duration"`
	} `json:"block"`
	Fallback *upstreamsResult `json:"fallback"`
	Forward  string           `json:"forward"`
}

func (lb *lb) upstreams() upstreamsResult {
	lb.mu.RLock()
	block := lb.block
	list := lb.bestURLs
	lb.mu.RUnlock()

	var resp upstreamsResult
	resp.Forward = "upstream"
	for _, x := range list {
		resp.Upstreams = append(resp.Upstreams, x.String())
	}
	if block != nil {
		resp.Block.Number = block.Number.Uint64()
		resp.Block.Duration = time.Since(time.Unix(int64(block.Time), 0)).String()
	}
	if lb.fallback != nil {
		fb := lb.fallback.upstreams()
		resp.Fallback = &fb
	}
	if lb.isFallback() {
		resp.Forward = "fallback"
	}
	return resp
}

func (lb *lb) upstreamsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(lb.upstreams())
}

var (
	clients   = map[string]*ethclient.Client{}
	muClients sync.RWMutex
)

func getClient(ctx context.Context, addr string) (*ethclient.Client, error) {
	muClients.RLock()
	c := clients[addr]
	muClients.RUnlock()

	if c != nil {
		return c, nil
	}

	var err error
	c, err = ethclient.DialContext(ctx, addr)
	if err != nil {
		return nil, err
	}

	muClients.Lock()
	defer muClients.Unlock()

	if clients[addr] == nil {
		clients[addr] = c
	} else {
		c.Close()
	}

	return clients[addr], nil
}
