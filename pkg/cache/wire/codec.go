package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/kuasar-sandbox/accelerator/pkg/cache"
)

var (
	ErrFrameTooLarge = errors.New("wire: frame exceeds MaxFrameSize")
	ErrFrameTooSmall = errors.New("wire: frame smaller than minimum header")
)

// ---------------------------------------------------------------------------
// Request codec
// ---------------------------------------------------------------------------

// ReadRequest reads one complete request frame from r.
//
// For Put operations the Value slice is allocated from the internal
// request-buffer pool; the caller MUST call Request.Release when done
// to return the buffer. Request.Release is a no-op for Get/Ping
// requests (Value is nil).
func ReadRequest(r io.Reader) (*Request, error) {
	var hdr [RequestHeaderSize]byte

	// 1. Read TotalLen (first 4 bytes).
	if _, err := io.ReadFull(r, hdr[:4]); err != nil {
		return nil, err
	}
	totalLen := binary.LittleEndian.Uint32(hdr[:4])

	// 2. Validate bounds.
	if totalLen < RequestHeaderSize {
		return nil, ErrFrameTooSmall
	}
	if totalLen > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	// 3. Read the rest of the fixed header.
	if _, err := io.ReadFull(r, hdr[4:]); err != nil {
		return nil, fmt.Errorf("wire: read request header: %w", err)
	}

	req := &Request{
		Opcode:    hdr[4],
		Namespace: hdr[5],
		Flags:     hdr[6],
	}
	copy(req.Hash[:], hdr[7:39])

	// 4. Read value payload (Put ops).
	valueLen := int(totalLen) - RequestHeaderSize
	if valueLen > 0 {
		req.Value = GetReqBuf(valueLen)
		if _, err := io.ReadFull(r, req.Value); err != nil {
			putReqBuf(req.Value)
			req.Value = nil
			return nil, fmt.Errorf("wire: read request value: %w", err)
		}
	}

	return req, nil
}

// WriteRequest writes a request frame to w using writev when possible.
func WriteRequest(w io.Writer, req *Request) error {
	var hdr [RequestHeaderSize]byte

	totalLen := uint32(RequestHeaderSize + len(req.Value))
	binary.LittleEndian.PutUint32(hdr[:4], totalLen)
	hdr[4] = req.Opcode
	hdr[5] = req.Namespace
	hdr[6] = req.Flags
	copy(hdr[7:39], req.Hash[:])

	if len(req.Value) == 0 {
		_, err := w.Write(hdr[:])
		return err
	}

	bufs := net.Buffers{hdr[:], req.Value}
	_, err := bufs.WriteTo(w)
	return err
}

// ---------------------------------------------------------------------------
// Response codec
// ---------------------------------------------------------------------------

// ReadResponse reads one complete response frame from r, allocating
// the value payload (on HIT) from pool. Callers MUST call
// resp.Value.Release() when done with the blob to return the buffer
// to the pool.
//
// Pass cache.DefaultPool (or nil — treated identically) if you don't
// want pooling — the Blob will be a GC-managed memBlob with a no-op
// Release, exactly matching the pre-pool baseline.
//
// This is the slow/generic path (any io.Reader). Production callers
// on wire.Conn go through (*Conn).ReadResponse which has a large-
// payload fast path that bypasses bufio for the value body.
func ReadResponse(r io.Reader, pool cache.BlobPool) (*Response, error) {
	if pool == nil {
		pool = cache.DefaultPool
	}
	var hdr [ResponseHeaderSize]byte

	// 1. Read the fixed header.
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	totalLen := binary.LittleEndian.Uint32(hdr[:4])

	// 2. Validate bounds.
	if totalLen < ResponseHeaderSize {
		return nil, ErrFrameTooSmall
	}
	if totalLen > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	resp := &Response{
		Status: hdr[4],
		Flags:  hdr[5],
	}
	errLen := int(binary.LittleEndian.Uint16(hdr[6:8]))

	remaining := int(totalLen) - ResponseHeaderSize

	// 3. Read error message if present.
	if errLen > 0 {
		if errLen > remaining {
			return nil, fmt.Errorf("wire: errLen %d exceeds remaining %d", errLen, remaining)
		}
		errBuf := make([]byte, errLen)
		if _, err := io.ReadFull(r, errBuf); err != nil {
			return nil, fmt.Errorf("wire: read response errmsg: %w", err)
		}
		resp.ErrMsg = string(errBuf)
		remaining -= errLen
	}

	// 4. Read value payload (Get HIT).
	if remaining > 0 {
		buf, blob := pool.Alloc(remaining)
		if _, err := io.ReadFull(r, buf); err != nil {
			blob.Release()
			return nil, fmt.Errorf("wire: read response value: %w", err)
		}
		resp.Value = blob
	}

	return resp, nil
}

// readResponseFast reads a response frame using `br` for the header
// (small, amortised via bufio) and, for large value payloads, reads
// the body directly from `raw` after draining any bufio-prefetched
// bytes. This saves the kernel→bufio→pool memcpy that dominates
// ReadResponse CPU for multi-KB payloads (e.g., 131 KiB EC shards or
// 512 KiB objects).
//
// Correctness hinges on bufio.Reader exposing both its buffered bytes
// (Buffered(), Peek, Discard) and never prefetching past the header
// call. Since we Peek/Discard exactly the buffered bytes into the
// value buf before switching to raw, ordering is preserved.
func readResponseFast(br *bufio.Reader, raw io.Reader, pool cache.BlobPool) (*Response, error) {
	if pool == nil {
		pool = cache.DefaultPool
	}
	var hdr [ResponseHeaderSize]byte

	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, err
	}
	totalLen := binary.LittleEndian.Uint32(hdr[:4])
	if totalLen < ResponseHeaderSize {
		return nil, ErrFrameTooSmall
	}
	if totalLen > MaxFrameSize {
		return nil, ErrFrameTooLarge
	}

	resp := &Response{Status: hdr[4], Flags: hdr[5]}
	errLen := int(binary.LittleEndian.Uint16(hdr[6:8]))
	remaining := int(totalLen) - ResponseHeaderSize

	if errLen > 0 {
		if errLen > remaining {
			return nil, fmt.Errorf("wire: errLen %d exceeds remaining %d", errLen, remaining)
		}
		errBuf := make([]byte, errLen)
		if _, err := io.ReadFull(br, errBuf); err != nil {
			return nil, fmt.Errorf("wire: read response errmsg: %w", err)
		}
		resp.ErrMsg = string(errBuf)
		remaining -= errLen
	}

	if remaining > 0 {
		buf, blob := pool.Alloc(remaining)

		// Drain any bytes bufio already prefetched into the value buf,
		// then read the rest straight from raw. This avoids the extra
		// kernel→bufio copy for the bulk of a large payload.
		if buffered := br.Buffered(); buffered > 0 {
			n := buffered
			if n > remaining {
				n = remaining
			}
			if _, err := io.ReadFull(br, buf[:n]); err != nil {
				blob.Release()
				return nil, fmt.Errorf("wire: drain bufio: %w", err)
			}
			if n == remaining {
				resp.Value = blob
				return resp, nil
			}
			if _, err := io.ReadFull(raw, buf[n:]); err != nil {
				blob.Release()
				return nil, fmt.Errorf("wire: read response value: %w", err)
			}
			resp.Value = blob
			return resp, nil
		}

		// bufio empty: read straight from raw into the pool buffer.
		if _, err := io.ReadFull(raw, buf); err != nil {
			blob.Release()
			return nil, fmt.Errorf("wire: read response value: %w", err)
		}
		resp.Value = blob
	}

	return resp, nil
}

// WriteResponse writes a response frame to w using writev when possible.
// The caller is responsible for calling resp.Value.Release() after this
// returns (on both success and failure) when resp.Value is non-nil.
//
// Fast path: if resp.Value implements cache.Segmenter (e.g. EC's
// SegmentedBlob), the frame is assembled from the segments directly
// without a join. Saves the 50–180 µs memcpy that EC's Decode would
// otherwise perform to produce a single contiguous slice.
func WriteResponse(w io.Writer, resp *Response) error {
	var hdr [ResponseHeaderSize]byte

	errBytes := []byte(resp.ErrMsg)

	var valueSegments [][]byte
	var valueLen int
	if resp.Value != nil {
		if seg, ok := resp.Value.(cache.Segmenter); ok {
			valueSegments = seg.Segments()
			for _, s := range valueSegments {
				valueLen += len(s)
			}
		} else {
			valueBytes := resp.Value.Bytes()
			if len(valueBytes) > 0 {
				valueSegments = [][]byte{valueBytes}
				valueLen = len(valueBytes)
			}
		}
	}

	totalLen := uint32(ResponseHeaderSize + len(errBytes) + valueLen)
	binary.LittleEndian.PutUint32(hdr[:4], totalLen)
	hdr[4] = resp.Status
	hdr[5] = resp.Flags
	binary.LittleEndian.PutUint16(hdr[6:8], uint16(len(errBytes)))

	// Use writev to avoid copying the (potentially large) value. For
	// SegmentedBlob this sends hdr + errMsg + N shard slices in one
	// syscall-scheduled writev without any join.
	bufs := net.Buffers{hdr[:]}
	if len(errBytes) > 0 {
		bufs = append(bufs, errBytes)
	}
	for _, s := range valueSegments {
		if len(s) > 0 {
			bufs = append(bufs, s)
		}
	}
	_, err := bufs.WriteTo(w)
	return err
}
