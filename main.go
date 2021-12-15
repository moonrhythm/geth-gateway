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
	"github.com/moonrhythm/parapet/pkg/upstream"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	ethClients      []*ethclient.Client
	ethWsClients    []*ethclient.Client
	upstreamAddrs   []string
	wsUpstreamAddrs []string

	upstreamURLs    []*url.URL
	wsUpstreamURLs  []*url.URL
	healthyDuration time.Duration

	muBestUpstreams  sync.RWMutex
	bestUpstreamURLs []*url.URL
	bestBlock        *types.Header
	bestBlockUpdated time.Time

	muBestWsUpstreams  sync.RWMutex
	bestWsUpstreamURLs []*url.URL
	bestWsBlock        *types.Header
	bestWsBlockUpdated time.Time
)

func main() {
	var (
		addr                = flag.String("addr", ":80", "HTTP address")
		tlsAddr             = flag.String("tls.addr", "", "HTTPS address")
		tlsKey              = flag.String("tls.key", "", "TLS private key file")
		tlsCert             = flag.String("tls.cert", "", "TLS certificate file")
		upstreamList        = flag.String("upstream", "", "Upstream list")
		gethHealthyDuration = flag.Duration("healthy-duration", time.Minute, "duration from last block that mark as healthy")
		healthCheckDeadline = flag.Duration("health-check.deadline", 4*time.Second, "deadline when run health check")
		healthCheckInterval = flag.Duration("health-check.interval", 2*time.Second, "health check interval")
	)

	flag.Parse()

	log.Printf("geth-gateway")
	log.Printf("HTTP address: %s", *addr)
	log.Printf("HTTPS address: %s", *tlsAddr)
	log.Printf("Upstream: %s", *upstreamList)
	log.Printf("Healthy Duration: %s", *gethHealthyDuration)
	log.Printf("Health Check Deadline: %s", *healthCheckDeadline)

	healthyDuration = *gethHealthyDuration

	prom.Registry().MustRegister(headDuration, headNumber)

	for _, addr := range strings.Split(*upstreamList, ",") {
		addr = strings.TrimSpace(addr)
		if addr == "" {
			continue
		}

		u, err := url.Parse(addr)
		if err != nil {
			log.Fatalf("can not parse url; %v", err)
		}

		client, err := ethclient.Dial(addr)
		if err != nil {
			log.Fatalf("can not dial geth; %v", err)
		}

		if u.Scheme == "ws" || u.Scheme == "wss" {
			u.Scheme = "http" + strings.TrimPrefix(u.Scheme, "ws") // http or https
			wsUpstreamAddrs = append(wsUpstreamAddrs, addr)
			wsUpstreamURLs = append(wsUpstreamURLs, u)
			ethWsClients = append(ethWsClients, client)
		} else {
			upstreamAddrs = append(upstreamAddrs, addr)
			upstreamURLs = append(upstreamURLs, u)
			ethClients = append(ethClients, client)
		}
	}

	// on startup, we don't have check last block yet
	// TODO: add readiness health check to send healthy state after first upstream check
	{
		lastBlocks = make([]lastBlock, len(upstreamURLs))
		bestUpstreamURLs = upstreamURLs

		wsLastBlocks = make([]lastBlock, len(wsUpstreamURLs))
		bestWsUpstreamURLs = wsUpstreamURLs
	}

	// http update loop
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), *healthCheckDeadline)
			updateLastBlock(ctx)
			cancel()

			time.Sleep(*healthCheckInterval)
		}
	}()

	// ws update loop
	if len(wsUpstreamAddrs) > 0 {
		go func() {
			for {
				ctx, cancel := context.WithTimeout(context.Background(), *healthCheckDeadline)
				updateWsLastBlock(ctx)
				cancel()

				time.Sleep(*healthCheckInterval)
			}
		}()
	}

	var s parapet.Middlewares

	s.Use(logger.Stdout())
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
		l.Use(parapet.Handler(upstreamsHandler))
		s.Use(l)
	}

	// ws upstreams
	{
		l := location.Exact("/wsupstreams")
		l.Use(parapet.Handler(wsUpstreamsHandler))
		s.Use(l)
	}

	if len(wsUpstreamAddrs) > 0 {
		l := location.Exact("/ws")
		l.Use(upstream.New(&wsTr{}))
		s.Use(l)
	}

	// http
	s.Use(upstream.New(&tr{}))

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

type lastBlock struct {
	mu        sync.Mutex
	Block     *types.Header
	UpdatedAt time.Time
}

var (
	lastBlocks   []lastBlock
	wsLastBlocks []lastBlock
)

func getLastBlock(ctx context.Context, ethClients []*ethclient.Client, i int, force bool) (*types.Header, error) {
	b := &lastBlocks[i]
	b.mu.Lock()
	defer b.mu.Unlock()

	if !force && time.Since(b.UpdatedAt) < time.Second {
		return b.Block, nil
	}

	block, err := ethClients[i].HeaderByNumber(ctx, nil)
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

func updateLastBlock(ctx context.Context) {
	blockNumbers := make([]uint64, len(lastBlocks))
	blockData := make([]*types.Header, len(lastBlocks))

	var wg sync.WaitGroup
	for i := range lastBlocks {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()

			block, _ := getLastBlock(ctx, ethClients, i, true)
			if block == nil {
				return
			}

			blockNumbers[i] = block.Number.Uint64()
			blockData[i] = block

			t := time.Unix(int64(block.Time), 0)
			diff := time.Since(t)

			g, err := headDuration.GetMetricWith(prometheus.Labels{
				"upstream": upstreamAddrs[i],
			})
			if err == nil {
				g.Set(float64(diff) / float64(time.Second))
			}

			g, err = headNumber.GetMetricWith(prometheus.Labels{
				"upstream": upstreamAddrs[i],
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
			best = append(best, upstreamURLs[i])
			bestIndex = i
		}
	}

	muBestUpstreams.Lock()
	bestUpstreamURLs = best
	bestBlock = blockData[bestIndex]
	bestBlockUpdated = time.Now()
	muBestUpstreams.Unlock()
}

func updateWsLastBlock(ctx context.Context) {
	// TODO: refactor ?
	blockNumbers := make([]uint64, len(wsLastBlocks))
	blockData := make([]*types.Header, len(wsLastBlocks))

	var wg sync.WaitGroup
	for i := range wsLastBlocks {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()

			block, _ := getLastBlock(ctx, ethWsClients, i, true)
			if block == nil {
				return
			}

			blockNumbers[i] = block.Number.Uint64()
			blockData[i] = block

			t := time.Unix(int64(block.Time), 0)
			diff := time.Since(t)

			g, err := headDuration.GetMetricWith(prometheus.Labels{
				"upstream": wsUpstreamAddrs[i],
			})
			if err == nil {
				g.Set(float64(diff) / float64(time.Second))
			}

			g, err = headNumber.GetMetricWith(prometheus.Labels{
				"upstream": wsUpstreamAddrs[i],
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
			best = append(best, wsUpstreamURLs[i])
			bestIndex = i
		}
	}

	muBestWsUpstreams.Lock()
	bestWsUpstreamURLs = best
	bestWsBlock = blockData[bestIndex]
	bestWsBlockUpdated = time.Now()
	muBestWsUpstreams.Unlock()
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

type tr struct {
	i uint32
}

var (
	trs = map[string]http.RoundTripper{
		"http": &upstream.HTTPTransport{
			MaxIdleConns: 1000,
		},
		"https": &upstream.HTTPSTransport{
			MaxIdleConns: 1000,
		},
	}
)

// RoundTrip sends a request to upstream server
func (tr *tr) RoundTrip(r *http.Request) (*http.Response, error) {
	muBestUpstreams.RLock()
	targets := bestUpstreamURLs
	muBestUpstreams.RUnlock()

	if len(targets) == 0 {
		return nil, upstream.ErrUnavailable
	}

	i := atomic.AddUint32(&tr.i, 1) - 1
	i %= uint32(len(targets))
	t := targets[i]

	r.URL.Scheme = t.Scheme
	r.URL.Host = t.Host
	r.URL.Path = path.Join(t.Path, r.URL.Path)
	r.Host = t.Host
	return trs[t.Scheme].RoundTrip(r)
}

// TODO: implement L7 ws load balancer ?
type wsTr struct {
	i uint32
}

func (tr *wsTr) RoundTrip(r *http.Request) (*http.Response, error) {
	muBestWsUpstreams.RLock()
	targets := bestWsUpstreamURLs
	muBestWsUpstreams.RUnlock()

	if len(targets) == 0 {
		return nil, upstream.ErrUnavailable
	}

	i := atomic.AddUint32(&tr.i, 1) - 1
	i %= uint32(len(targets))
	t := targets[i]

	r.URL.Scheme = t.Scheme
	r.URL.Host = t.Host
	r.URL.Path = t.Path
	r.Host = t.Host
	return trs[t.Scheme].RoundTrip(r)
}

func isReady() bool {
	muBestUpstreams.RLock()
	block := bestBlock
	muBestUpstreams.RUnlock()

	t := time.Unix(int64(block.Time), 0)
	return time.Since(t) <= healthyDuration
}

func isLive() bool {
	muBestUpstreams.RLock()
	ok := len(bestUpstreamURLs) > 0
	updated := bestBlockUpdated
	muBestUpstreams.RUnlock()

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

type upstreamsResponse struct {
	Upstreams []string `json:"upstreams"`
	Block     struct {
		Number   uint64 `json:"number"`
		Duration string `json:"duration"`
	} `json:"block"`
}

func upstreamsHandler(w http.ResponseWriter, r *http.Request) {
	muBestUpstreams.RLock()
	block := bestBlock
	list := bestUpstreamURLs
	muBestUpstreams.RUnlock()

	var resp upstreamsResponse
	for _, x := range list {
		resp.Upstreams = append(resp.Upstreams, x.String())
	}
	if block != nil {
		resp.Block.Number = block.Number.Uint64()
		resp.Block.Duration = time.Since(time.Unix(int64(block.Time), 0)).String()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}

func wsUpstreamsHandler(w http.ResponseWriter, r *http.Request) {
	muBestWsUpstreams.RLock()
	block := bestWsBlock
	list := bestWsUpstreamURLs
	muBestWsUpstreams.RUnlock()

	var resp upstreamsResponse
	for _, x := range list {
		resp.Upstreams = append(resp.Upstreams, x.String())
	}
	if block != nil {
		resp.Block.Number = block.Number.Uint64()
		resp.Block.Duration = time.Since(time.Unix(int64(block.Time), 0)).String()
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}
