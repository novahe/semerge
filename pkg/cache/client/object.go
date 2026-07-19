package client

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

// Options configures a cache-ctl wire client.
type Options struct {
	Pool     int            // number of parallel Get connections (default 4)
	WarmPool int            // independent warm-only connections (default 4)
	Timeout  time.Duration  // per-op timeout (default 2s)
	BlobPool cache.BlobPool // payload allocator; nil → cache.DefaultPool
}

// GetCloser = cache.Getter + io.Closer. The client package's exported
// read-side contract — Close is a first-class part of the contract so
// callers never need to runtime-assert io.Closer on the returned value.
type GetCloser interface {
	cache.Getter
	cache.Warmer
	cache.WarmCapability
	io.Closer
}

// TierCloser = cache.Tier + io.Closer. The client package's exported
// read+write contract.
type TierCloser interface {
	cache.Tier
	cache.Warmer
	cache.WarmCapability
	io.Closer
}

// impl is the private wire-client that underlies both NewGetter and New.
// getPool and warmPool are independent, pre-established pools so speculative
// warming cannot occupy demand-side client connections. fillPool dials
// on-demand (capped at 1) so backfill writes cannot starve reads.
type impl struct {
	getPool  *ConnPool
	warmPool *ConnPool
	fillPool *ConnPool
	timeout  time.Duration
	blobPool cache.BlobPool
	warmOK   bool
}

func newImpl(endpoint string, opts Options) (*impl, error) {
	if opts.Pool <= 0 {
		opts.Pool = 4
	}
	if opts.WarmPool <= 0 {
		opts.WarmPool = 4
	}
	// opts.Timeout <= 0 stays 0 = no per-call deadline: bounded only by
	// the caller's context, never an arbitrary number.
	if opts.BlobPool == nil {
		opts.BlobPool = cache.DefaultPool
	}
	gp, err := DialConnPool(endpoint, ConnPoolConfig{
		PoolSize: opts.Pool,
		MaxSize:  opts.Pool,
		Timeout:  opts.Timeout,
	})
	if err != nil {
		return nil, err
	}
	fp, err := DialConnPool(endpoint, ConnPoolConfig{
		PoolSize: 0,
		MaxSize:  1,
		Timeout:  opts.Timeout,
		IdleMax:  30 * time.Second,
	})
	if err != nil {
		gp.Close()
		return nil, err
	}

	// Capability negotiation is deliberately best-effort: a transient ping
	// failure or an old server must not break the existing Get path.
	var capabilities byte
	probeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	if pc, acquireErr := gp.Acquire(probeCtx); acquireErr == nil {
		flags, pingErr := pc.PingCapabilities(2 * time.Second)
		gp.Release(pc, pingErr == nil)
		if pingErr == nil {
			capabilities = flags
		}
	}
	cancel()

	var wp *ConnPool
	if capabilities&wire.CapabilityWarmOnly != 0 {
		wp, err = DialConnPool(endpoint, ConnPoolConfig{
			PoolSize: opts.WarmPool,
			MaxSize:  opts.WarmPool,
			Timeout:  opts.Timeout,
		})
		if err != nil {
			// Warming is optional. Preserve a healthy demand client and expose
			// the unavailable capability through WarmSupported.
			wp = nil
		}
	}
	return &impl{
		getPool: gp, warmPool: wp, fillPool: fp,
		timeout: opts.Timeout, blobPool: opts.BlobPool, warmOK: wp != nil,
	}, nil
}

// NewGetter dials endpoint and returns a read-only handle.
func NewGetter(endpoint string, opts Options) (GetCloser, error) {
	return newImpl(endpoint, opts)
}

// New dials endpoint and returns a full read+write tier.
func New(endpoint string, opts Options) (TierCloser, error) {
	return newImpl(endpoint, opts)
}

// Get implements cache.Getter.
//
// Wire-level failures (connection, timeout, EOF) are reported as errors
// rather than silently converted to CacheMiss. Returning MISS on a
// transport failure would be semantically wrong: MISS means "the peer
// confirmed it does not have this key", whereas a transport error means
// "we don't know". In a TieredCache chain, a false MISS at origin is
// indistinguishable from a genuine absence — the cascade terminates and
// the caller is told the key doesn't exist, even though it may.
func (c *impl) Get(ctx context.Context, p store.Partition, key store.ContentKey) (cache.CacheResult, cache.Blob, error) {
	pc, err := c.getPool.Acquire(ctx)
	if err != nil {
		return cache.CacheMiss, nil, fmt.Errorf("cache: client.Get acquire: %w", err)
	}

	req := &wire.Request{
		Opcode:    wire.OpcodeObjectGet,
		Namespace: partitionToWireNS(p),
		Hash:      key,
	}

	pc.SetOpDeadline(ctx, c.timeout)
	if err := pc.WriteRequest(req); err != nil {
		c.getPool.Release(pc, false)
		return cache.CacheMiss, nil, fmt.Errorf("cache: client.Get write: %w", err)
	}

	resp, err := pc.ReadResponse(c.blobPool)
	if err != nil {
		c.getPool.Release(pc, false)
		return cache.CacheMiss, nil, fmt.Errorf("cache: client.Get read: %w", err)
	}
	c.getPool.Release(pc, true)

	switch resp.Status {
	case wire.StatusHit:
		return cache.CacheHit, resp.Value, nil
	case wire.StatusMiss:
		return cache.CacheMiss, nil, nil
	case wire.StatusError:
		return cache.CacheMiss, nil, fmt.Errorf("cache: server error: %s", resp.ErrMsg)
	default:
		return cache.CacheMiss, nil, fmt.Errorf("cache: unexpected status %d", resp.Status)
	}
}

// WarmSupported reports whether the server advertised warm-only support and
// the independent warm connection pool was established successfully.
func (c *impl) WarmSupported() bool { return c.warmOK }

// Warm fetches key into a ready server-side cache tier without transferring
// ciphertext back to this process.
func (c *impl) Warm(ctx context.Context, p store.Partition, key store.ContentKey) (cache.CacheResult, error) {
	if !c.warmOK || c.warmPool == nil {
		return cache.CacheMiss, cache.ErrWarmUnsupported
	}
	pc, err := c.warmPool.Acquire(ctx)
	if err != nil {
		return cache.CacheMiss, fmt.Errorf("cache: client.Warm acquire: %w", err)
	}

	req := &wire.Request{
		Opcode:    wire.OpcodeObjectGet,
		Namespace: partitionToWireNS(p),
		Flags:     wire.FlagWarmOnly,
		Hash:      key,
	}
	pc.SetOpDeadline(ctx, c.timeout)
	if err := pc.WriteRequest(req); err != nil {
		c.warmPool.Release(pc, false)
		return cache.CacheMiss, fmt.Errorf("cache: client.Warm write: %w", err)
	}

	watchDone := make(chan struct{})
	watchExited := make(chan struct{})
	go func() {
		defer close(watchExited)
		select {
		case <-ctx.Done():
			_ = pc.SendCancel()
		case <-watchDone:
		}
	}()
	resp, err := pc.ReadResponse(nil)
	close(watchDone)
	<-watchExited
	if err != nil {
		c.warmPool.Release(pc, false)
		return cache.CacheMiss, fmt.Errorf("cache: client.Warm read: %w", err)
	}
	c.warmPool.Release(pc, true)
	if resp.Value != nil {
		// A capable server must not send a payload for warm-only. Release it
		// defensively, then reject the protocol violation.
		resp.Value.Release()
		return cache.CacheMiss, fmt.Errorf("cache: client.Warm unexpected payload")
	}
	switch resp.Status {
	case wire.StatusHit:
		return cache.CacheHit, nil
	case wire.StatusMiss:
		return cache.CacheMiss, nil
	case wire.StatusCancelled:
		return cache.CacheMiss, ErrCancelled
	case wire.StatusError:
		return cache.CacheMiss, fmt.Errorf("cache: server error: %s", resp.ErrMsg)
	default:
		return cache.CacheMiss, fmt.Errorf("cache: unexpected status %d", resp.Status)
	}
}

// Fill implements cache.Filler.
func (c *impl) Fill(ctx context.Context, p store.Partition, key store.ContentKey, value []byte) error {
	pc, err := c.fillPool.Acquire(ctx)
	if err != nil {
		return err
	}

	req := &wire.Request{
		Opcode:    wire.OpcodeObjectPut,
		Namespace: partitionToWireNS(p),
		Hash:      key,
		Value:     value,
	}

	pc.SetOpDeadline(ctx, c.timeout)
	if err := pc.WriteRequest(req); err != nil {
		c.fillPool.Release(pc, false)
		return err
	}

	resp, err := pc.ReadResponse(c.blobPool)
	if err != nil {
		c.fillPool.Release(pc, false)
		return err
	}
	if resp.Value != nil {
		resp.Value.Release()
	}
	c.fillPool.Release(pc, true)

	if resp.Status == wire.StatusError {
		return fmt.Errorf("cache: fill: %s", resp.ErrMsg)
	}
	return nil
}

// Close closes both underlying connection pools.
func (c *impl) Close() error {
	err1 := c.getPool.Close()
	err2 := c.fillPool.Close()
	var err3 error
	if c.warmPool != nil {
		err3 = c.warmPool.Close()
	}
	if err1 != nil {
		return err1
	}
	if err2 != nil {
		return err2
	}
	return err3
}

var _ cache.Getter = (*impl)(nil)
var _ cache.Warmer = (*impl)(nil)
var _ cache.WarmCapability = (*impl)(nil)
var _ cache.Tier = (*impl)(nil)
var _ GetCloser = (*impl)(nil)
var _ TierCloser = (*impl)(nil)

// ── helpers ──

func partitionToWireNS(p store.Partition) byte {
	switch p {
	case store.PartitionChunk:
		return wire.NSChunk
	case store.PartitionManifest:
		return wire.NSManifest
	case store.PartitionBlob:
		return wire.NSBlob
	default:
		return wire.NSChunk
	}
}
