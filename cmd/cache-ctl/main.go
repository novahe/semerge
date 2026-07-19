// cache-ctl is the CLI for the cache daemon and client operations.
//
// Subcommands:
//
//	serve       Start cache daemon (local/shard/tiered mode via YAML config)
//	object get  Read a complete object from cache
//	object put  Write a complete object to cache
//	shard get   Read an EC shard from cache
//	shard put   Write an EC shard to cache
//	ping        Health probe via gRPC health protocol
//	info        Inspect daemon and backend state
package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/kuasar-sandbox/accelerator/internal/util"
	"github.com/kuasar-sandbox/accelerator/internal/util/obstat"
	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/client"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/ec"
	cachepb "github.com/kuasar-sandbox/accelerator/pkg/cache/pb"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/redisstore"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/rocks"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/runtime"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/server"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
	storeclient "github.com/kuasar-sandbox/accelerator/pkg/store/client"

	"google.golang.org/grpc"

	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
)

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "serve":
		cmdServe(os.Args[2:])
	case "object":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: cache-ctl object {get|put}")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "get":
			cmdObjectGet(os.Args[3:])
		case "put":
			cmdObjectPut(os.Args[3:])
		default:
			fmt.Fprintf(os.Stderr, "unknown object subcommand: %s\n", os.Args[2])
			os.Exit(1)
		}
	case "shard":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: cache-ctl shard {get|put}")
			os.Exit(1)
		}
		switch os.Args[2] {
		case "get":
			cmdShardGet(os.Args[3:])
		case "put":
			cmdShardPut(os.Args[3:])
		default:
			fmt.Fprintf(os.Stderr, "unknown shard subcommand: %s\n", os.Args[2])
			os.Exit(1)
		}
	case "ping":
		cmdPing(os.Args[2:])
	case "info":
		cmdInfo(os.Args[2:])
	case "bench":
		cmdBench(os.Args[2:])
	case "config":
		cmdConfig(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: cache-ctl <command> [args]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve       Start cache daemon")
	fmt.Fprintln(os.Stderr, "  object get  Read a complete object")
	fmt.Fprintln(os.Stderr, "  object put  Write a complete object")
	fmt.Fprintln(os.Stderr, "  shard get   Read an EC shard")
	fmt.Fprintln(os.Stderr, "  shard put   Write an EC shard")
	fmt.Fprintln(os.Stderr, "  ping        Health probe")
	fmt.Fprintln(os.Stderr, "  info        Inspect daemon and backend state")
	fmt.Fprintln(os.Stderr, "  bench       Run performance benchmark")
	fmt.Fprintln(os.Stderr, "  config      Inspect or generate the cache-ctl config")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Environment:")
	fmt.Fprintln(os.Stderr, "  CACHE_CONFIG    fallback for serve --config")
	fmt.Fprintln(os.Stderr, "  CACHE_ENDPOINT  fallback for object/shard/bench --endpoint")
}

// ── serve ──

// cacheConfigEnv overrides --config for cache-ctl serve when both are absent.
const cacheConfigEnv = "CACHE_CONFIG"

// cacheEndpointEnv supplies --endpoint for client commands (data port:
// object/shard/bench) when the flag is absent. ping/info/info --rocks-path
// are deliberately not affected: they speak the health endpoint or no
// network at all.
const cacheEndpointEnv = "CACHE_ENDPOINT"

type directStore interface {
	cache.Tier
	cache.ShardTier
	Close() error
}

// resolveDataEndpoint returns the cache data endpoint chosen from, in
// priority order, the --endpoint flag value then $CACHE_ENDPOINT.
// Empty result -> caller should fatal.
func resolveDataEndpoint(flagValue string) string {
	if flagValue != "" {
		return flagValue
	}
	return os.Getenv(cacheEndpointEnv)
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	configPath := fs.String("config", "", "YAML config file (overrides CACHE_CONFIG env)")
	fs.Parse(args)

	resolved := *configPath
	if resolved == "" {
		resolved = os.Getenv(cacheConfigEnv)
	}
	if resolved == "" {
		fatal("--config or %s required", cacheConfigEnv)
	}

	cfg, err := runtime.LoadConfig(resolved)
	if err != nil {
		fatal("%v", err)
	}
	if err := cfg.Validate(); err != nil {
		fatal("%v", err)
	}

	// One process-level pool backs all payload-returning clients/stores. Redis
	// reads use it directly; RocksDB retains its namespace-sized internal pools.
	blobPool := cache.NewPool(512 << 10)

	// local/shard use one concrete store for both object and shard opcodes.
	// tiered constructs its stores in YAML order inside buildTieredChain.
	var (
		direct     directStore
		rocksStore rocks.Interface
		redisStore *redisstore.Store
	)
	switch cfg.Mode {
	case "local", "shard":
		switch cfg.Type {
		case "embedded":
			rocksStore, err = rocks.Open(cfg.Rocks, cfg.Freq)
			if err != nil {
				fatal("open rocks: %v", err)
			}
			direct = rocksStore
		case "redis":
			redisStore, err = redisstore.Open(*cfg.Redis, blobPool)
			if err != nil {
				fatal("open redis: %v", err)
			}
			direct = redisStore
		}
		defer direct.Close()
	}

	// Per-mode backend wiring.
	//
	//   local / shard mode : one direct store serves both interfaces
	//   tiered mode        : object tier chain only
	//
	// primaryRocks is the rocks.Interface exposed via the Info gRPC
	// service for top-level rocks CF stats (local/shard mode). nil in
	// tiered mode — per-tier rocks appears under TieredStats instead.
	var (
		tierBackend  cache.Tier
		shardBackend cache.ShardTier
		primaryRocks rocks.Interface
		primaryRedis *redisstore.Store
	)

	// tieredComps holds the tier chain in tiered mode so we can defer a
	// cascaded Close after signal handling wiring is in place.
	var tieredComps *tieredComponents

	// tiered state captured during mode == "tiered" construction, consumed
	// below when wiring the Info gRPC service.
	var tieredCache *cache.TieredCache
	var originSpec *OriginSpec

	switch cfg.Mode {
	case "local", "shard":
		// Unified handler: both stores satisfy cache.Tier and
		// cache.ShardTier, so the same backend serves object and
		// shard opcodes. Mode naming reflects the *primary* workload
		// rather than an exclusive opcode family.
		tierBackend = direct
		shardBackend = direct
		primaryRocks = rocksStore
		primaryRedis = redisStore
	case "tiered":
		// The process-global BlobPool is allocated once and threaded
		// into every wire client (upstream origin, ec peers, etc.) for
		// coordinated payload pooling. rocks manages its own internal
		// pools and does NOT share this one.
		//
		// Must be constructed BEFORE buildTieredChain so the EC peer
		// shard clients can use it — without pooling here the EC path
		// accounts for ~50% of target-side heap allocations (see
		// perf-baseline.md).
		//
		// Seed size 512KB: the upstream ObjectGet response is a full
		// value (≈ 256–512KB in production). EC per-shard reads are
		// smaller (~1/4 value) so they consume a partial slice of a
		// pooled buffer without forcing an oversized realloc. Sizing
		// the pool to the largest expected response avoids the
		// "cap < size → fresh make()" branch in syncPool.Alloc.
		comps, buildErr := buildTieredChain(cfg, blobPool)
		if buildErr != nil {
			fatal("%v", buildErr)
		}
		tieredComps = comps
		defer tieredComps.Close()

		// Origin. Two flavours, dispatched on cfg.Origin.Type:
		//   - store: gRPC to store-ctl
		//   - upstream: wire-client to another cache-ctl endpoint

		var origin cache.Getter
		maxInflight := cfg.Origin.MaxInflight
		if maxInflight < 0 {
			maxInflight = 0
		}
		switch cfg.Origin.Type {
		case "store":
			sc := cfg.Origin.Store
			pool := sc.Pool
			if pool <= 0 {
				pool = 4
			}
			var timeout time.Duration // 0 = no per-op deadline (caller ctx only)
			if sc.Timeout != "" {
				if d, err := time.ParseDuration(sc.Timeout); err == nil && d > 0 {
					timeout = d
				}
			}
			originStore, dialErr := storeclient.New(sc.Endpoint, pool, timeout)
			if dialErr != nil {
				fatal("create origin client: %v", dialErr)
			}
			defer originStore.Close()
			origin = cache.NewOriginAdapter(cache.NewStoreOrigin(originStore), maxInflight)
			originSpec = &OriginSpec{Type: "store", Endpoint: sc.Endpoint}
		case "upstream":
			uc := cfg.Origin.Upstream
			pool := uc.Pool
			if pool <= 0 {
				pool = 4
			}
			var timeout time.Duration // 0 = no per-op deadline (caller ctx only)
			if uc.Timeout != "" {
				if d, err := time.ParseDuration(uc.Timeout); err == nil && d > 0 {
					timeout = d
				}
			}
			originClient, dialErr := client.NewGetter(uc.Endpoint, client.Options{
				Pool:     pool,
				Timeout:  timeout,
				BlobPool: blobPool,
			})
			if dialErr != nil {
				fatal("create upstream origin client: %v", dialErr)
			}
			defer originClient.Close()
			origin = cache.NewOriginAdapter(originClient, maxInflight)
			originSpec = &OriginSpec{Type: "upstream", Endpoint: uc.Endpoint}
		default:
			fatal("unknown origin.type: %q", cfg.Origin.Type)
		}
		tc := cache.NewTieredCache(origin, comps.Tiers...)
		tieredCache = tc
		tierBackend = &tieredBackend{tc: tc}
		// tiered mode: per-tier rocks surfaces under TieredStats; no
		// top-level rocks to expose via Info.
	}

	// Build unified CacheHandler. Tier handles object ops; shard handles
	// shard ops. Either may be nil — wire_handler returns StatusError on
	// opcodes the current mode doesn't support.
	handler := server.NewCacheHandler(tierBackend, shardBackend)

	// Wire data server.
	//
	// idleTimeout is hardcoded to 120s (silent-connection retirement).
	// rpcTimeout is the user-configurable rpc_timeout YAML field —
	// forwards a per-request deadline into HandleFrame so tiered-mode
	// remote hops can be cancelled when a request exceeds the budget.
	ws := server.NewWireServer(handler, 120*time.Second, cfg.ParseRPCTimeout())
	dataLis, err := util.Listen(cfg.Listen)
	if err != nil {
		fatal("listen %s: %v", cfg.Listen, err)
	}
	go ws.Serve(dataLis)

	// gRPC health + Info server (optional, on the HealthListen port).
	// Both services are registered on the same *grpc.Server — Health
	// for probes, Info for structured counter snapshots consumed by
	// bench scripts and cache-ctl info --endpoint.
	var grpcServer *grpc.Server
	var healthSrv interface {
		SetServingStatus(string, healthgrpc.HealthCheckResponse_ServingStatus)
	}
	healthCtx, healthCancel := context.WithCancel(context.Background())
	defer healthCancel()
	var healthMonitorDone <-chan struct{}
	if cfg.HealthListen != "" {
		grpcServer = grpc.NewServer()
		hsrv := server.RegisterHealth(grpcServer)
		healthSrv = hsrv

		infoSrv := NewInfoServer(cfg.Mode, handler)
		if tieredCache != nil {
			infoSrv.SetTiered(tieredCache, tieredComps.TierSpecs, originSpec)
		}
		if primaryRocks != nil {
			infoSrv.SetTopLevelRocks(primaryRocks)
		}
		if primaryRedis != nil {
			infoSrv.SetTopLevelRedis(primaryRedis)
		}
		cachepb.RegisterInfoServer(grpcServer, infoSrv)

		healthLis, err := util.Listen(cfg.HealthListen)
		if err != nil {
			fatal("listen health %s: %v", cfg.HealthListen, err)
		}
		redisStores := collectRedisStores(primaryRedis, tieredComps)
		if len(redisStores) > 0 {
			// RegisterHealth defaults to SERVING. Redis-backed daemons must not
			// expose that state until the dedicated end-to-end probe succeeds.
			healthSrv.SetServingStatus("", healthgrpc.HealthCheckResponse_NOT_SERVING)
		}
		go grpcServer.Serve(healthLis)
		if len(redisStores) > 0 {
			healthMonitorDone = startRedisHealthMonitor(healthCtx, healthSrv, redisStores)
		}
		fmt.Fprintf(os.Stderr, "cache-ctl serve mode=%s listen=%s health=%s\n", cfg.Mode, cfg.Listen, cfg.HealthListen)
	} else {
		fmt.Fprintf(os.Stderr, "cache-ctl serve mode=%s listen=%s\n", cfg.Mode, cfg.Listen)
	}

	// Optional pprof HTTP listener — imported for side-effect registration
	// of /debug/pprof/* on http.DefaultServeMux. Leave pprof_listen empty
	// in production. Use for perf investigation only.
	if cfg.PprofListen != "" {
		go func() {
			if err := http.ListenAndServe(cfg.PprofListen, nil); err != nil {
				fmt.Fprintf(os.Stderr, "pprof listen %s: %v\n", cfg.PprofListen, err)
			}
		}()
		fmt.Fprintf(os.Stderr, "cache-ctl pprof=%s (/debug/pprof/)\n", cfg.PprofListen)
	}

	// Runtime stats are served on-demand via the Info gRPC service (registered
	// on the Health listener in Step 3). On top of that, an adaptive stats line
	// is printed to stderr each period that saw traffic (silent when idle) —
	// concurrency, bandwidth, latency p50/p99/max, hit cascade, and backend
	// gauges. stats_interval=0/off disables it. Stopped on shutdown.
	var statsRocks rocks.Interface = primaryRocks
	if tieredComps != nil {
		statsRocks = tieredComps.EmbeddedStore
	}
	statsCtx, statsCancel := context.WithCancel(context.Background())
	defer statsCancel()
	go obstat.RunAdaptive(statsCtx, cfg.StatsIntervalDur(),
		cacheSampler(cfg.Mode, ws, tieredCache, statsRocks, collectRedisGaugeSources(primaryRedis, tieredComps)), log.Printf)

	// Signal handling.
	//
	// SIGINT/SIGTERM → graceful shutdown (single-shot).
	// SIGHUP        → re-read YAML config and, for every EC tier,
	//                 apply the updated peer list as a new membership
	//                 (epoch bumped). Reload happens in-place; the
	//                 daemon keeps running.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			reloadMembership(resolved, tieredComps)
			continue
		default:
			fmt.Fprintf(os.Stderr, "\ncache-ctl: received %v, shutting down...\n", sig)
			healthCancel()
			if healthMonitorDone != nil {
				<-healthMonitorDone
			}
			if healthSrv != nil {
				healthSrv.SetServingStatus("", healthgrpc.HealthCheckResponse_NOT_SERVING)
			}
			ws.GracefulStop()
			if grpcServer != nil {
				grpcServer.GracefulStop()
			}
			if tieredCache != nil {
				// Cancel any in-flight async fills so a fill blocked on
				// a wedged origin can't hang shutdown (fills now have no
				// deadline by default; baseCtx cancellation reaps them).
				tieredCache.Close()
			}
			return
		}
	}
}

func startRedisHealthMonitor(ctx context.Context, healthSrv interface {
	SetServingStatus(string, healthgrpc.HealthCheckResponse_ServingStatus)
}, stores []*redisstore.Store) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		monitorRedisHealth(ctx, healthSrv, stores)
	}()
	return done
}

func collectRedisStores(primary *redisstore.Store, comps *tieredComponents) []*redisstore.Store {
	var stores []*redisstore.Store
	if primary != nil {
		stores = append(stores, primary)
	}
	if comps != nil {
		for _, spec := range comps.TierSpecs {
			if spec.RedisStore != nil {
				stores = append(stores, spec.RedisStore)
			}
		}
	}
	return stores
}

func collectRedisGaugeSources(primary *redisstore.Store, comps *tieredComponents) []redisGaugeSource {
	var sources []redisGaugeSource
	if primary != nil {
		sources = append(sources, redisGaugeSource{label: "redis", store: primary})
	}
	if comps != nil {
		for i, spec := range comps.TierSpecs {
			if spec.RedisStore != nil {
				sources = append(sources, redisGaugeSource{
					label: fmt.Sprintf("redis[L%d]", i),
					store: spec.RedisStore,
				})
			}
		}
	}
	return sources
}

func monitorRedisHealth(ctx context.Context, healthSrv interface {
	SetServingStatus(string, healthgrpc.HealthCheckResponse_ServingStatus)
}, stores []*redisstore.Store) {
	probe := func() {
		status := healthgrpc.HealthCheckResponse_SERVING
		for _, store := range stores {
			probeCtx, cancel := context.WithTimeout(ctx, time.Second)
			err := store.Probe(probeCtx)
			cancel()
			if err != nil {
				status = healthgrpc.HealthCheckResponse_NOT_SERVING
				break
			}
		}
		healthSrv.SetServingStatus("", status)
	}
	probe()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			probe()
		case <-ctx.Done():
			return
		}
	}
}

// reloadMembership re-parses the YAML config at path and, for each EC
// tier in tieredComps, probes the candidate peers and applies a new
// membership. Errors are logged and do not crash the daemon; a failed
// reload leaves the existing membership in place.
func reloadMembership(configPath string, comps *tieredComponents) {
	if comps == nil {
		log.Printf("SIGHUP: not in tiered mode; nothing to reload")
		return
	}
	newCfg, err := runtime.LoadConfig(configPath)
	if err != nil {
		log.Printf("SIGHUP: reload config: %v", err)
		return
	}
	if err := newCfg.Validate(); err != nil {
		log.Printf("SIGHUP: new config invalid: %v", err)
		return
	}
	if len(newCfg.Tiers) != len(comps.TierSpecs) {
		log.Printf("SIGHUP: tier count changed (was %d, now %d); skipping reload",
			len(comps.TierSpecs), len(newCfg.Tiers))
		return
	}
	for i, spec := range comps.TierSpecs {
		if spec.Type != "ec" || spec.EC == nil {
			continue
		}
		newTier := newCfg.Tiers[i]
		if newTier.Type != "ec" {
			log.Printf("SIGHUP: tier[%d] was ec, now %q; skipping", i, newTier.Type)
			continue
		}
		if newTier.Cluster == nil {
			log.Printf("SIGHUP: tier[%d] new ec config missing cluster; skipping", i)
			continue
		}
		candidate := make([]ec.Peer, len(newTier.Cluster.Peers))
		for j, p := range newTier.Cluster.Peers {
			candidate[j] = ec.Peer{ID: p.ID, Endpoint: p.Endpoint}
		}
		live := probeAndFilter(candidate)
		if len(live) == 0 {
			log.Printf("SIGHUP: tier[%d] no peers live; skipping (keeping old membership)", i)
			continue
		}
		m := ec.Membership{
			Epoch: spec.EC.Epoch() + 1,
			Peers: live,
		}
		if err := spec.EC.ApplyMembership(m); err != nil {
			log.Printf("SIGHUP: tier[%d] apply membership: %v", i, err)
		}
	}
}

// probeAndFilter runs ProbePeer in parallel on all candidates with a
// short timeout budget, returning only peers that accepted a
// connection. Failed probes are logged at warn level so an operator
// doing a rolling replacement can see which nodes are holding up.
func probeAndFilter(candidates []ec.Peer) []ec.Peer {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	type result struct {
		p  ec.Peer
		ok bool
	}
	resCh := make(chan result, len(candidates))
	var wg sync.WaitGroup
	for _, p := range candidates {
		wg.Add(1)
		go func(p ec.Peer) {
			defer wg.Done()
			err := ec.ProbePeer(ctx, p.Endpoint)
			if err != nil {
				log.Printf("SIGHUP: probe peer %s (%s) failed: %v", p.ID, p.Endpoint, err)
			}
			resCh <- result{p: p, ok: err == nil}
		}(p)
	}
	wg.Wait()
	close(resCh)

	out := make([]ec.Peer, 0, len(candidates))
	for r := range resCh {
		if r.ok {
			out = append(out, r.p)
		}
	}
	return out
}

// ── object get ──

func cmdObjectGet(args []string) {
	fs := flag.NewFlagSet("object get", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "cache-ctl data endpoint (overrides CACHE_ENDPOINT env)")
	namespace := fs.String("namespace", "chunk", "namespace (chunk|manifest)")
	hashHex := fs.String("hash", "", "32-byte hash (hex, required)")
	fs.Parse(args)

	if *hashHex == "" {
		fatal("--hash is required")
	}
	hash, err := hex.DecodeString(*hashHex)
	if err != nil || len(hash) != 32 {
		fatal("--hash must be 64 hex chars (32 bytes)")
	}

	resolved := resolveDataEndpoint(*endpoint)
	if resolved == "" {
		fatal("--endpoint or CACHE_ENDPOINT required")
	}

	c, err := client.NewGetter(resolved, client.Options{Pool: 1, Timeout: 5 * time.Second})
	if err != nil {
		fatal("dial: %v", err)
	}
	defer c.Close()

	var key store.ContentKey
	copy(key[:], hash)
	result, blob, err := c.Get(context.Background(), store.Partition(*namespace), key)
	if err != nil {
		fatal("get: %v", err)
	}
	if result != cache.CacheHit {
		fmt.Fprintln(os.Stderr, "MISS")
		os.Exit(1)
	}
	defer blob.Release()
	os.Stdout.Write(blob.Bytes())
}

// ── object put ──

func cmdObjectPut(args []string) {
	fs := flag.NewFlagSet("object put", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "cache-ctl data endpoint (overrides CACHE_ENDPOINT env)")
	namespace := fs.String("namespace", "chunk", "namespace")
	hashHex := fs.String("hash", "", "32-byte hash (hex, required)")
	valuePath := fs.String("value", "-", "value file (- for stdin)")
	fs.Parse(args)

	if *hashHex == "" {
		fatal("--hash is required")
	}
	hash, err := hex.DecodeString(*hashHex)
	if err != nil || len(hash) != 32 {
		fatal("--hash must be 64 hex chars")
	}

	var r io.Reader = os.Stdin
	if *valuePath != "-" {
		f, err := os.Open(*valuePath)
		if err != nil {
			fatal("open value: %v", err)
		}
		defer f.Close()
		r = f
	}
	value, err := io.ReadAll(r)
	if err != nil {
		fatal("read value: %v", err)
	}

	resolved := resolveDataEndpoint(*endpoint)
	if resolved == "" {
		fatal("--endpoint or CACHE_ENDPOINT required")
	}

	c, err := client.New(resolved, client.Options{Pool: 1, Timeout: 5 * time.Second})
	if err != nil {
		fatal("dial: %v", err)
	}
	defer c.Close()

	var key store.ContentKey
	copy(key[:], hash)
	if err := c.Fill(context.Background(), store.Partition(*namespace), key, value); err != nil {
		fatal("put: %v", err)
	}
	fmt.Fprintln(os.Stderr, "OK")
}

// ── shard get ──

func cmdShardGet(args []string) {
	fs := flag.NewFlagSet("shard get", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "cache-ctl data endpoint (overrides CACHE_ENDPOINT env)")
	namespace := fs.String("namespace", "chunk", "namespace")
	hashHex := fs.String("hash", "", "32-byte hash (hex, required)")
	fs.Parse(args)

	if *hashHex == "" {
		fatal("--hash is required")
	}
	hash, err := hex.DecodeString(*hashHex)
	if err != nil || len(hash) != 32 {
		fatal("--hash must be 64 hex chars")
	}

	resolved := resolveDataEndpoint(*endpoint)
	if resolved == "" {
		fatal("--endpoint or CACHE_ENDPOINT required")
	}

	c, err := client.NewShard(resolved, client.Options{Pool: 1, Timeout: 5 * time.Second})
	if err != nil {
		fatal("dial: %v", err)
	}
	defer c.Close()

	var key store.ContentKey
	copy(key[:], hash)
	result, blob, err := c.GetShard(context.Background(), store.Partition(*namespace), key)
	if err != nil {
		fatal("get shard: %v", err)
	}
	if result != cache.CacheHit {
		fmt.Fprintln(os.Stderr, "MISS")
		os.Exit(1)
	}
	defer blob.Release()
	// The peer returns [idx][total][data]. Report idx/total on stderr
	// for debugging; raw shard bytes go to stdout.
	idx, total, data, ok := cache.ParseShardPrefix(blob.Bytes())
	if !ok {
		fatal("shard value too short (%d bytes, need >= %d)", len(blob.Bytes()), cache.ShardPrefixSize)
	}
	fmt.Fprintf(os.Stderr, "HIT idx=%d total=%d size=%d\n", idx, total, len(data))
	os.Stdout.Write(data)
}

// ── shard put ──

func cmdShardPut(args []string) {
	fs := flag.NewFlagSet("shard put", flag.ExitOnError)
	endpoint := fs.String("endpoint", "", "cache-ctl data endpoint (overrides CACHE_ENDPOINT env)")
	namespace := fs.String("namespace", "chunk", "namespace")
	hashHex := fs.String("hash", "", "32-byte hash (hex, required)")
	idx := fs.Uint("idx", 0, "shard index (0..total-1)")
	total := fs.Uint("total", 5, "total shard count (K+P)")
	valuePath := fs.String("value", "-", "shard data file (- for stdin)")
	fs.Parse(args)

	if *hashHex == "" {
		fatal("--hash is required")
	}
	hash, err := hex.DecodeString(*hashHex)
	if err != nil || len(hash) != 32 {
		fatal("--hash must be 64 hex chars")
	}
	if *idx >= *total {
		fatal("--idx must be < --total")
	}

	var r io.Reader = os.Stdin
	if *valuePath != "-" {
		f, err := os.Open(*valuePath)
		if err != nil {
			fatal("open value: %v", err)
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		fatal("read value: %v", err)
	}
	// Wrap bare data with [idx][total] prefix to match what EC peers
	// expect. This mirrors what the real tier.Fill path does via
	// EncodePrefixed; the CLI is just the single-peer debug surface.
	value := make([]byte, cache.ShardPrefixSize+len(data))
	cache.EncodeShardPrefix(value, byte(*idx), byte(*total))
	copy(value[cache.ShardPrefixSize:], data)

	resolved := resolveDataEndpoint(*endpoint)
	if resolved == "" {
		fatal("--endpoint or CACHE_ENDPOINT required")
	}

	c, err := client.NewShard(resolved, client.Options{Pool: 1, Timeout: 5 * time.Second})
	if err != nil {
		fatal("dial: %v", err)
	}
	defer c.Close()

	var key store.ContentKey
	copy(key[:], hash)
	if err := c.FillShard(context.Background(), store.Partition(*namespace), key, value); err != nil {
		fatal("put shard: %v", err)
	}
	fmt.Fprintln(os.Stderr, "OK")
}

// ── ping ──

func cmdPing(args []string) {
	fs := flag.NewFlagSet("ping", flag.ExitOnError)
	endpoint := fs.String("endpoint", "127.0.0.1:7071", "cache-ctl health endpoint")
	fs.Parse(args)

	p, err := client.DialPool(*endpoint, 1)
	if err != nil {
		fatal("dial: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	hc := healthgrpc.NewHealthClient(p.Next())
	resp, err := hc.Check(ctx, &healthgrpc.HealthCheckRequest{})
	if err != nil {
		fatal("health check: %v", err)
	}
	fmt.Println(resp.Status)
}

// ── info ── (see info.go for cmdInfo; supports remote gRPC + offline rocks)

// ── helpers ──

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cache-ctl: "+format+"\n", args...)
	os.Exit(1)
}

// tieredBackend adapts *cache.TieredCache to cache.Tier for the wire
// handler. TieredCache.Get returns (bool, Blob, error); this adapter
// translates to the Tier Get signature (CacheResult, Blob, error) and
// preserves the Blob abstraction end-to-end so the wire server's
// OnRelease callback releases any pool buffer in one well-defined place.
//
// Fill is unsupported in tiered mode — the bench target is intended
// as a read cache; writes go to origin. Return an explicit error so
// the wire layer surfaces StatusError to the client.
type tieredBackend struct {
	tc *cache.TieredCache
}

var errTieredFill = fmt.Errorf("writes are not supported in tiered mode")

func (b *tieredBackend) Get(ctx context.Context, p store.Partition, key store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	found, blob, err := b.tc.Get(ctx, p, key)
	if err != nil {
		return cache.CacheMiss, nil, err
	}
	if !found {
		return cache.CacheMiss, nil, nil
	}
	return cache.CacheHit, blob, nil
}

func (b *tieredBackend) Warm(ctx context.Context, p store.Partition, key store.ContentKey) (cache.CacheResult, error) {
	return b.tc.Warm(ctx, p, key)
}

func (b *tieredBackend) Fill(ctx context.Context, p store.Partition, key store.ContentKey, data []byte) error {
	return errTieredFill
}

// RejectsWrites signals the wire handler that this tier cannot serve
// object writes. Surfaced via an interface type-assertion so the
// handler can return "writes not supported" before the generic
// empty-value check.
func (b *tieredBackend) RejectsWrites() bool { return true }
