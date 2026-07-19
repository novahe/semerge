package client

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
	"github.com/kuasar-sandbox/accelerator/pkg/cache/wire"
	"github.com/kuasar-sandbox/accelerator/pkg/store"
)

var clientSocketSeq atomic.Uint32

type clientWireServer struct {
	lis       net.Listener
	warm      bool
	warmCalls atomic.Int32
	wg        sync.WaitGroup
}

func runClientTestServer(t *testing.T, warm bool) (string, *clientWireServer) {
	t.Helper()
	path := fmt.Sprintf("/tmp/cache-warm-%d-%d.sock", os.Getpid(), clientSocketSeq.Add(1))
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	srv := &clientWireServer{lis: lis, warm: warm}
	srv.wg.Add(1)
	go func() {
		defer srv.wg.Done()
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			srv.wg.Add(1)
			go srv.serveConn(conn)
		}
	}()
	return "unix://" + path, srv
}

func (s *clientWireServer) serveConn(conn net.Conn) {
	defer s.wg.Done()
	defer conn.Close()
	wc := wire.NewConn(conn)
	for {
		req, err := wc.ReadRequest()
		if err != nil {
			return
		}
		resp := &wire.Response{Status: wire.StatusHit}
		switch req.Opcode {
		case wire.OpcodePing:
			if s.warm {
				resp.Flags = wire.CapabilityWarmOnly
			}
		case wire.OpcodeObjectGet:
			if req.Flags&wire.FlagWarmOnly != 0 {
				s.warmCalls.Add(1)
			}
		case wire.OpcodeCancelRequest:
			resp.Status = wire.StatusCancelled
		}
		req.Release()
		if err := wc.WriteResponse(resp); err != nil {
			return
		}
	}
}

func (s *clientWireServer) Close() {
	_ = s.lis.Close()
	s.wg.Wait()
}

func TestObjectClientNegotiatesWarmAndUsesNoPayloadRPC(t *testing.T) {
	endpoint, srv := runClientTestServer(t, true)
	defer srv.Close()

	c, err := NewGetter(endpoint, Options{Pool: 1, WarmPool: 2, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewGetter: %v", err)
	}
	defer c.Close()
	if !c.WarmSupported() {
		t.Fatal("warm capability was not negotiated")
	}
	result, err := c.Warm(context.Background(), store.PartitionChunk, store.ContentKey{1})
	if err != nil || result != cache.CacheHit {
		t.Fatalf("Warm result=%v err=%v", result, err)
	}
	if srv.warmCalls.Load() != 1 {
		t.Fatalf("server warm calls=%d, want 1", srv.warmCalls.Load())
	}
}

func TestObjectClientDisablesWarmWithoutCapability(t *testing.T) {
	endpoint, srv := runClientTestServer(t, false)
	defer srv.Close()

	c, err := NewGetter(endpoint, Options{Pool: 1, WarmPool: 2, Timeout: time.Second})
	if err != nil {
		t.Fatalf("NewGetter: %v", err)
	}
	defer c.Close()
	if c.WarmSupported() {
		t.Fatal("plain backend unexpectedly advertised warm support")
	}
	_, err = c.Warm(context.Background(), store.PartitionChunk, store.ContentKey{1})
	if !errors.Is(err, cache.ErrWarmUnsupported) {
		t.Fatalf("Warm error=%v, want ErrWarmUnsupported", err)
	}
}
