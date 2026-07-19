// Package wire implements a binary framing protocol for cache data-plane RPCs.
//
// The wire protocol replaces gRPC+protobuf on the hot data path.  gRPC is
// retained only for the health-check control plane.
//
// Frame layouts are fixed-width (no varints) for branch-free parsing.
//
//	Request  (39 B header + optional value):
//	  [4 TotalLen LE][1 Opcode][1 NS][1 Flags][32 Hash][N Value]
//
//	Response (8 B header + optional payload):
//	  [4 TotalLen LE][1 Status][1 Flags][2 ErrLen LE][ErrLen ErrMsg][N Value]
//
// For shard opcodes (OpcodeShardGet / OpcodeShardPut) the Value bytes
// follow the on-disk shard format: [1 idx][1 total][N shard_data]
// (see pkg/cache/shard_value.go). The wire layer does not interpret
// the prefix; only the EC encoder/decoder reads it.
package wire

import (
	"sync"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
)

// Opcodes identify the operation in a request frame.
const (
	OpcodeObjectGet     byte = 0x01
	OpcodeObjectPut     byte = 0x02
	OpcodeShardGet      byte = 0x03
	OpcodeShardPut      byte = 0x04
	OpcodePing          byte = 0x05
	OpcodeCancelRequest byte = 0x06
)

// Request flags. Unknown flags are ignored for forward compatibility.
const (
	FlagWarmOnly byte = 1 << 0
)

// Capability bits returned in Response.Flags for OpcodePing.
const (
	CapabilityWarmOnly byte = 1 << 0
)

// Status codes in a response frame.
const (
	StatusHit       byte = 0x00
	StatusMiss      byte = 0x01
	StatusError     byte = 0x02
	StatusCancelled byte = 0x03
)

// Namespace codes (mirrors store.Partition).
const (
	NSChunk    byte = 0x01
	NSManifest byte = 0x02
	NSBlob     byte = 0x03
)

// Frame size constants.
const (
	RequestHeaderSize  = 39
	ResponseHeaderSize = 8
	MaxFrameSize       = 4 << 20 // 4 MiB
)

// Request is a decoded request frame.
type Request struct {
	Opcode    byte
	Namespace byte
	Flags     byte
	Hash      [32]byte
	Value     []byte // nil for Get/Ping; from reqBufPool for Put (server side)
}

// Release returns the request's value buffer (if any) to the wire
// package's internal request-buffer pool. Wire server calls this
// after the handler has finished processing the request. No-op for
// Get/Ping requests (Value is nil).
//
// Encapsulating pool return inside the wire package keeps wire_server
// free of any pool-specific knowledge; the server just calls
// Request.Release without caring where the buffer came from.
func (r *Request) Release() {
	if r.Value != nil {
		putReqBuf(r.Value)
		r.Value = nil
	}
}

// Response is a decoded response frame.
//
// Value carries the object payload on HIT as a cache.Blob. The Blob
// contract unifies what used to be split between a raw []byte and a
// parallel OnRelease callback: callers simply call Value.Release()
// after the frame has been written (server side) or consumed (client
// side). On client reads, the blob comes from a caller-provided
// BlobPool; on server writes, the blob comes from wherever the handler
// sourced it (rocks pool, ec memory, etc.) — Release dispatches to
// the right pool via the blob's own interface.
type Response struct {
	Status byte
	Flags  byte       // capabilities for Ping; zero for ordinary responses
	ErrMsg string     // non-empty only when Status == StatusError
	Value  cache.Blob // nil for MISS/ERROR; carries the payload on HIT
}

// ---------------------------------------------------------------------------
// reqBufPool — used only by server-side ReadRequest for Put value
// buffers. Lifetime is short: read frame → handler → Store.Put →
// Request.Release (internally putReqBuf).
//
// Client-side ReadResponse value buffers are NOT pooled (make + GC)
// because the caller (e.g. TieredCache backfill) may hold them
// indefinitely.
// ---------------------------------------------------------------------------

var reqBufPool = sync.Pool{
	New: func() any { return make([]byte, 0, 64*1024) },
}

// GetReqBuf returns a byte slice of the requested size from the pool.
// Exported because wire codec reads fill this slice during decode.
func GetReqBuf(size int) []byte {
	buf := reqBufPool.Get().([]byte)
	if cap(buf) < size {
		return make([]byte, size)
	}
	return buf[:size]
}

// putReqBuf returns a buffer to the pool. Safe to call with nil.
// Package-private: external callers should use Request.Release.
func putReqBuf(b []byte) {
	if b != nil {
		reqBufPool.Put(b[:0])
	}
}
