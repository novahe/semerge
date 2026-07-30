package tarstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"

	"github.com/kuasar-sandbox/accelerator/pkg/sparse"
)

// asReadSeeker returns r as an io.ReadSeeker only if it BOTH implements the
// interface AND can actually seek. An *os.File satisfies io.ReadSeeker even when
// its fd is a pipe/socket, where Seek fails at runtime with ESPIPE ("illegal
// seek") — e.g. a piped stdin (`cat foo | manifest-ctl store`). A no-op
// current-offset probe distinguishes a real seekable (regular file) from a stream,
// so the latter takes the one-pass path instead of erroring.
func asReadSeeker(r io.Reader) (io.ReadSeeker, bool) {
	rs, ok := r.(io.ReadSeeker)
	if !ok {
		return nil, false
	}
	if _, err := rs.Seek(0, io.SeekCurrent); err != nil {
		return nil, false
	}
	return rs, true
}

// ReadFrom locates the entry called name in the tar stream r (an empty
// name takes the first regular file entry) and returns its logical
// view: Read yields size bytes with holes reading as zeros, pulling
// only the packed data from r. When r is actually seekable the call
// upgrades to ReadSeekFrom — the returned Reader then also implements
// ReadSeeker (type-assert to use it). Otherwise the view is one
// sequential pass with nothing buffered beyond a block.
func ReadFrom(r io.Reader, name string) (Reader, error) {
	if rs, ok := asReadSeeker(r); ok {
		v, err := newSeekView(rs, name)
		if err != nil {
			return nil, err
		}
		return v, nil
	}
	v, err := newSeqView(r, name)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// ReadSeekFrom is ReadFrom over a seekable stream: the returned view
// supports random access by mapping logical offsets straight onto the
// packed data region inside rs — no extraction, no copies. The view
// owns rs's seek position; do not use rs elsewhere while reading.
func ReadSeekFrom(rs io.ReadSeeker, name string) (ReadSeeker, error) {
	v, err := newSeekView(rs, name)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// SourceFrom locates the entry called name (empty: first regular file
// entry) and opens it as a sparse.Source — the pipeline-facing twin
// of ReadFrom: RunAt serves the envelope's hole map (Hole/Data only,
// never Zero), ReadAt the logical bytes. Over a plain reader the
// source is one-pass (monotone ReadAt); an actually-seekable r upgrades
// to random access. The entry's name is returned alongside.
func SourceFrom(r io.Reader, name string) (sparse.Source, string, error) {
	if rs, ok := asReadSeeker(r); ok {
		v, err := newSeekView(rs, name)
		if err != nil {
			return nil, "", err
		}
		return &sourceView{m: &v.meta, sk: v}, v.meta.name, nil
	}
	v, err := newSeqView(r, name)
	if err != nil {
		return nil, "", err
	}
	return &sourceView{m: &v.meta, seq: v}, v.meta.name, nil
}

// ReadSeekFromIndex is ReadSeekFrom addressing the entry by ordinal
// instead of name: index counts the real entries of the archive in
// order (meta entries — PAX, GNU longname/longlink, global headers —
// are not counted), exactly the sequence archive/tar's Reader.Next
// yields. It exists so a consumer iterating an archive with the
// stdlib reader can re-locate the Nth member here and recover what
// the stdlib hides (the sparse hole map).
func ReadSeekFromIndex(rs io.ReadSeeker, index int) (ReadSeeker, error) {
	m, err := locateAt(rs, index, seekSkip(rs))
	if err != nil {
		return nil, err
	}
	return seekViewFrom(rs, m)
}

// SourceAt locates the entry called name (empty: first regular file entry) in
// the size-bounded archive and opens it as a CONCURRENT random-access
// sparse.Source over ra. RunAt serves the envelope's hole map; ReadAt is pure
// offset arithmetic plus ra.ReadAt. When the archive has the strict Kuasar
// payload + empty digest-marker shape, the returned source also implements
// Digester. Ordinary tar members remain valid sources without that capability.
func SourceAt(ra io.ReaderAt, size int64, name string) (sparse.Source, string, error) {
	if size < 0 || size > 1<<62 {
		return nil, "", fmt.Errorf("tarstream: invalid artifact size %d", size)
	}
	sr := io.NewSectionReader(ra, 0, size)
	m, err := locate(sr, name, seekSkip(sr))
	if err != nil {
		return nil, "", err
	}
	dataStart, err := sr.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, "", err
	}
	src := sparse.Source(&readerAtSource{
		meta:         *m,
		ra:           ra,
		dataStart:    dataStart,
		packedPrefix: packedPrefix(m.extents),
	})
	digest, ok, err := discoverDigest(ra, size, m.name)
	if err != nil {
		return nil, "", err
	}
	if ok {
		src = &digestedSource{Source: src, digest: digest}
	}
	return src, m.name, nil
}

// digestedSource adds the optional, already-parsed artifact identity without
// changing sparse.Source or performing I/O from Digest.
type digestedSource struct {
	sparse.Source
	digest string
}

func (s *digestedSource) Digest() string { return s.digest }

func (s *digestedSource) StoredRangeAt(offset, limit uint64) (fileOffset, length uint64, ok bool, err error) {
	if locator, ok := s.Source.(interface {
		StoredRangeAt(offset, limit uint64) (fileOffset, length uint64, ok bool, err error)
	}); ok {
		return locator.StoredRangeAt(offset, limit)
	}
	return 0, 0, false, nil
}

// discoverDigest recognizes the strict two-entry artifact shape. A normal tar
// without a marker is not an error; a reserved marker that is present but
// malformed is. All scans skip stored payload bytes with Seek.
func discoverDigest(ra io.ReaderAt, size int64, payloadName string) (string, bool, error) {
	firstReader := io.NewSectionReader(ra, 0, size)
	first, err := locateAt(firstReader, 0, seekSkip(firstReader))
	if err != nil || first.name != payloadName {
		return "", false, nil
	}

	markerReader := io.NewSectionReader(ra, 0, size)
	marker, err := locateAt(markerReader, 1, seekSkip(markerReader))
	if err != nil {
		return "", false, nil
	}
	if !strings.HasPrefix(marker.name, SHA256MarkerPrefix) {
		return "", false, nil
	}
	digest, ok := parseDigestMarker(marker.name)
	if !ok {
		return "", false, fmt.Errorf("tarstream: invalid digest marker %q", marker.name)
	}
	if marker.logical != 0 || marker.stored != 0 || marker.mapLen != 0 {
		return "", false, fmt.Errorf("tarstream: digest marker %q is not empty", marker.name)
	}
	markerEnd, err := markerReader.Seek(0, io.SeekCurrent)
	if err != nil {
		return "", false, err
	}
	if markerEnd+int64(len(zeroBlock2)) != size {
		return "", false, fmt.Errorf("tarstream: digest marker must be the final entry")
	}
	var trailer [1024]byte
	if _, err := ra.ReadAt(trailer[:], markerEnd); err != nil {
		return "", false, fmt.Errorf("tarstream: read end-of-archive: %w", err)
	}
	if !isZeroBlock(trailer[:512]) || !isZeroBlock(trailer[512:]) {
		return "", false, fmt.Errorf("tarstream: invalid end-of-archive after digest marker")
	}
	return digest, true, nil
}

func newSeqView(r io.Reader, name string) (*seqView, error) {
	m, err := locate(r, name, func(n int64) error {
		_, err := io.CopyN(io.Discard, r, n)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &seqView{
		meta: *m,
		src:  io.LimitReader(r, m.stored-m.mapLen),
	}, nil
}

func newSeekView(rs io.ReadSeeker, name string) (*seekView, error) {
	m, err := locate(rs, name, seekSkip(rs))
	if err != nil {
		return nil, err
	}
	return seekViewFrom(rs, m)
}

// seekSkip returns a locate skip function that advances rs in place.
func seekSkip(rs io.Seeker) func(int64) error {
	return func(n int64) error {
		_, err := rs.Seek(n, io.SeekCurrent)
		return err
	}
}

// seekViewFrom builds the random-access view for an entry locate has
// just positioned rs at (start of packed data).
func seekViewFrom(rs io.ReadSeeker, m *meta) (*seekView, error) {
	dataStart, err := rs.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	return &seekView{meta: *m, rs: rs, dataStart: dataStart, packedPrefix: packedPrefix(m.extents)}, nil
}

// packedPrefix returns prefix sums of extent sizes: packedPrefix[i] is
// the packed-region offset where extent i's bytes begin.
func packedPrefix(extents []extent) []int64 {
	p := make([]int64, len(extents)+1)
	for i, e := range extents {
		p[i+1] = p[i] + e.Size
	}
	return p
}

// meta describes the located entry.
type meta struct {
	name    string
	logical int64           // logical file size
	stored  int64           // stored entry size (map + packed data)
	mapLen  int64           // sparse map bytes consumed from the stored region
	extents []extent        // data extents in logical offsets (dense: one run)
	holes   []sparse.Extent // canonical hole map (nil = dense)
}

// locate scans entries until it finds the one named want (empty: the
// first regular file entry), leaving r positioned at the start of its
// packed data (the sparse map, when present, has been consumed). skip
// advances r across unselected content.
func locate(r io.Reader, want string, skip func(n int64) error) (*meta, error) {
	want = normalizeName(want)
	return locateMatch(r, skip, func(name string, _ int, regular bool) bool {
		return name == want || (want == "" && regular)
	})
}

// locateAt scans entries until the index-th real entry (0-based; meta
// entries are not counted — the same sequence archive/tar yields).
func locateAt(r io.Reader, index int, skip func(n int64) error) (*meta, error) {
	return locateMatch(r, skip, func(_ string, ordinal int, _ bool) bool {
		return ordinal == index
	})
}

// locateMatch is the scanning core behind locate/locateAt: match is
// consulted once per real entry with its effective name, ordinal and
// regular-ness; the first match must be a regular file.
func locateMatch(r io.Reader, skip func(n int64) error, match func(name string, ordinal int, regular bool) bool) (*meta, error) {
	var blk [512]byte
	pax := map[string]string{}
	longName := ""
	sawZero := false
	ordinal := 0
	for {
		if _, err := io.ReadFull(r, blk[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil, ErrNotFound
			}
			return nil, err
		}
		if isZeroBlock(blk[:]) {
			if sawZero {
				return nil, ErrNotFound // end of archive
			}
			sawZero = true
			continue
		}
		sawZero = false
		if string(blk[257:262]) != "ustar" {
			return nil, fmt.Errorf("tarstream: not a tar header (bad magic)")
		}

		typeflag := blk[156]
		size, err := parseNumeric(blk[124:136])
		if err != nil {
			return nil, fmt.Errorf("tarstream: bad size field: %w", err)
		}
		padded := (size + 511) &^ 511

		switch typeflag {
		case 'x': // PAX extended header for the next entry
			recs, err := readPAX(r, size, padded)
			if err != nil {
				return nil, err
			}
			maps.Copy(pax, recs)
			continue
		case 'g': // global header: not interpreted
			if err := skip(padded); err != nil {
				return nil, err
			}
			continue
		case 'L': // GNU longname: data is the next entry's name
			data := make([]byte, size)
			if _, err := io.ReadFull(r, data); err != nil {
				return nil, err
			}
			if err := skip(padded - size); err != nil {
				return nil, err
			}
			longName = strings.TrimRight(string(data), "\x00")
			continue
		case 'K': // GNU longlink: irrelevant here
			if err := skip(padded); err != nil {
				return nil, err
			}
			continue
		case 'S': // legacy GNU binary sparse: cannot even be skipped safely
			return nil, ErrUnsupportedEncoding
		}

		// A real entry: resolve its effective name and stored size.
		effName := pax["GNU.sparse.name"]
		if effName == "" {
			effName = pax["path"]
		}
		if effName == "" {
			effName = longName
		}
		if effName == "" {
			effName = ustarName(blk[:])
		}
		effName = normalizeName(effName)
		stored := size
		if s, ok := pax["size"]; ok {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("tarstream: bad pax size: %w", err)
			}
			stored = v
		}
		entryPAX := pax
		pax = map[string]string{}
		longName = ""

		regular := typeflag == '0' || typeflag == 0
		matched := match(effName, ordinal, regular)
		ordinal++
		if !matched {
			if err := skip((stored + 511) &^ 511); err != nil {
				return nil, err
			}
			continue
		}
		if !regular {
			return nil, fmt.Errorf("tarstream: entry %q is not a regular file", effName)
		}
		if _, old := entryPAX["GNU.sparse.numblocks"]; old {
			return nil, ErrUnsupportedEncoding
		}
		if _, old := entryPAX["GNU.sparse.map"]; old {
			return nil, ErrUnsupportedEncoding
		}

		if entryPAX["GNU.sparse.major"] == "1" && entryPAX["GNU.sparse.minor"] == "0" {
			realsize, err := strconv.ParseInt(entryPAX["GNU.sparse.realsize"], 10, 64)
			if err != nil {
				return nil, fmt.Errorf("tarstream: bad GNU.sparse.realsize: %w", err)
			}
			extents, mapLen, err := readSparseMap(r, realsize)
			if err != nil {
				return nil, err
			}
			return &meta{
				name:    effName,
				logical: realsize,
				stored:  stored,
				mapLen:  mapLen,
				extents: extents,
				holes:   extentsToHoles(realsize, extents),
			}, nil
		}

		m := &meta{name: effName, logical: stored, stored: stored}
		if stored > 0 {
			m.extents = []extent{{Offset: 0, Size: stored}}
		}
		return m, nil
	}
}

// readSparseMap parses the GNU PAX sparse 1.0 map that prefixes the
// stored data: decimal extent count, then offset/size per line,
// consumed in whole 512-byte blocks. Zero-length extents (GNU's
// trailing sentinel) are dropped.
func readSparseMap(r io.Reader, realsize int64) ([]extent, int64, error) {
	var (
		blk    [512]byte
		buf    []byte
		mapLen int64
	)
	next := func() (int64, error) {
		for {
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				v, err := strconv.ParseInt(string(buf[:i]), 10, 64)
				if err != nil || v < 0 {
					return 0, fmt.Errorf("tarstream: bad sparse map value %q", buf[:i])
				}
				buf = buf[i+1:]
				return v, nil
			}
			if len(buf) > 64 { // a decimal never runs this long
				return 0, fmt.Errorf("tarstream: malformed sparse map")
			}
			if _, err := io.ReadFull(r, blk[:]); err != nil {
				return 0, fmt.Errorf("tarstream: sparse map: %w", err)
			}
			mapLen += 512
			buf = append(buf, blk[:]...)
		}
	}
	count, err := next()
	if err != nil {
		return nil, 0, err
	}
	if count > 1<<20 {
		return nil, 0, fmt.Errorf("tarstream: absurd sparse extent count %d", count)
	}
	var extents []extent
	var pos int64
	for range count {
		off, err := next()
		if err != nil {
			return nil, 0, err
		}
		sz, err := next()
		if err != nil {
			return nil, 0, err
		}
		if sz == 0 {
			continue // sentinel
		}
		if off < pos || off+sz > realsize {
			return nil, 0, fmt.Errorf("tarstream: sparse extent [%d,+%d) out of order or bounds", off, sz)
		}
		extents = append(extents, extent{Offset: off, Size: sz})
		pos = off + sz
	}
	return extents, mapLen, nil
}

// readPAX reads and parses a PAX extended header's records.
func readPAX(r io.Reader, size, padded int64) (map[string]string, error) {
	if size > 1<<20 {
		return nil, fmt.Errorf("tarstream: absurd PAX header size %d", size)
	}
	data := make([]byte, padded)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, err
	}
	data = data[:size]
	recs := map[string]string{}
	for len(data) > 0 {
		sp := bytes.IndexByte(data, ' ')
		if sp <= 0 {
			return nil, fmt.Errorf("tarstream: malformed PAX record")
		}
		n, err := strconv.Atoi(string(data[:sp]))
		if err != nil || n <= sp || int64(n) > int64(len(data)) || data[n-1] != '\n' {
			return nil, fmt.Errorf("tarstream: malformed PAX record length")
		}
		kv := data[sp+1 : n-1]
		eq := bytes.IndexByte(kv, '=')
		if eq < 0 {
			return nil, fmt.Errorf("tarstream: malformed PAX record (no =)")
		}
		recs[string(kv[:eq])] = string(kv[eq+1:])
		data = data[n:]
	}
	return recs, nil
}

// ustarName joins the ustar prefix and name fields.
func ustarName(blk []byte) string {
	name := strings.TrimRight(string(blk[0:100]), "\x00")
	prefix := strings.TrimRight(string(blk[345:500]), "\x00")
	if prefix != "" {
		return prefix + "/" + name
	}
	return name
}

// parseNumeric decodes a ustar numeric field: NUL/space-terminated
// octal, or GNU base-256 when the high bit of the first byte is set.
func parseNumeric(b []byte) (int64, error) {
	if len(b) > 0 && b[0]&0x80 != 0 {
		var v int64
		v = int64(b[0] & 0x7f)
		for _, c := range b[1:] {
			v = v<<8 | int64(c)
		}
		return v, nil
	}
	s := strings.Trim(string(b), " \x00")
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 8, 64)
}

func isZeroBlock(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// normalizeName strips leading "/" and "./" so stored-name dialects
// compare equal.
func normalizeName(s string) string {
	for {
		switch {
		case strings.HasPrefix(s, "/"):
			s = strings.TrimPrefix(s, "/")
		case strings.HasPrefix(s, "./"):
			s = strings.TrimPrefix(s, "./")
		default:
			return s
		}
	}
}

// seqView is the one-pass logical view over a non-seekable source.
type seqView struct {
	meta
	src    io.Reader // packed data, already bounded to stored-mapLen
	pos    int64
	extIdx int
}

func (v *seqView) Name() string           { return v.meta.name }
func (v *seqView) Size() int64            { return v.logical }
func (v *seqView) Holes() []sparse.Extent { return append([]sparse.Extent(nil), v.holes...) }

func (v *seqView) Read(p []byte) (int, error) {
	if v.pos >= v.logical {
		return 0, io.EOF
	}
	for v.extIdx < len(v.extents) && v.pos >= v.extents[v.extIdx].Offset+v.extents[v.extIdx].Size {
		v.extIdx++
	}
	// Zero region: before the next extent, or the trailing hole.
	zeroEnd := v.logical
	if v.extIdx < len(v.extents) && v.pos < v.extents[v.extIdx].Offset {
		zeroEnd = v.extents[v.extIdx].Offset
	} else if v.extIdx < len(v.extents) {
		// Inside the current extent: read packed bytes.
		e := v.extents[v.extIdx]
		n := int64(len(p))
		if rest := e.Offset + e.Size - v.pos; rest < n {
			n = rest
		}
		read, err := v.src.Read(p[:n])
		if read > 0 {
			v.pos += int64(read)
			return read, nil
		}
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	n := int64(len(p))
	if rest := zeroEnd - v.pos; rest < n {
		n = rest
	}
	clear(p[:n])
	v.pos += n
	return int(n), nil
}

// seekView is the random-access logical view over a seekable source.
type seekView struct {
	meta
	rs           io.ReadSeeker
	dataStart    int64   // absolute offset of the packed data region
	packedPrefix []int64 // prefix sums of extent sizes
	pos          int64
}

func (v *seekView) Name() string           { return v.meta.name }
func (v *seekView) Size() int64            { return v.logical }
func (v *seekView) Holes() []sparse.Extent { return append([]sparse.Extent(nil), v.holes...) }

func (v *seekView) Seek(offset int64, whence int) (int64, error) {
	var base int64
	switch whence {
	case io.SeekStart:
		base = 0
	case io.SeekCurrent:
		base = v.pos
	case io.SeekEnd:
		base = v.logical
	default:
		return 0, fmt.Errorf("tarstream: bad whence %d", whence)
	}
	n := base + offset
	if n < 0 {
		return 0, fmt.Errorf("tarstream: negative seek position")
	}
	v.pos = n
	return n, nil
}

func (v *seekView) Read(p []byte) (int, error) {
	n, err := v.readAtPos(p, v.pos)
	v.pos += int64(n)
	return n, err
}

// readAtPos serves one partial read of the logical view at pos
// without touching v.pos: data extents map onto the packed region,
// everything else reads as zeros.
func (v *seekView) readAtPos(p []byte, pos int64) (int, error) {
	if pos >= v.logical {
		return 0, io.EOF
	}
	// Find the extent at or after pos.
	idx := 0
	for idx < len(v.extents) && pos >= v.extents[idx].Offset+v.extents[idx].Size {
		idx++
	}
	if idx < len(v.extents) && pos >= v.extents[idx].Offset {
		// Data: map the logical position into the packed region.
		e := v.extents[idx]
		packed := v.packedPrefix[idx] + (pos - e.Offset)
		n := int64(len(p))
		if rest := e.Offset + e.Size - pos; rest < n {
			n = rest
		}
		if _, err := v.rs.Seek(v.dataStart+packed, io.SeekStart); err != nil {
			return 0, err
		}
		read, err := v.rs.Read(p[:n])
		if read > 0 {
			return read, nil
		}
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	// Hole (or trailing hole): zeros until the next extent or EOF.
	zeroEnd := v.logical
	if idx < len(v.extents) {
		zeroEnd = v.extents[idx].Offset
	}
	n := int64(len(p))
	if rest := zeroEnd - pos; rest < n {
		n = rest
	}
	clear(p[:n])
	return int(n), nil
}

// readFullAt fills p from logical offset off, independent of the
// view's Read/Seek position.
func (v *seekView) readFullAt(p []byte, off int64) error {
	for len(p) > 0 {
		n, err := v.readAtPos(p, off)
		if err != nil {
			return err
		}
		p = p[n:]
		off += int64(n)
	}
	return nil
}

// sourceView adapts a located entry to sparse.Source. A separate type
// because Reader.Size returns int64 while sparse.Source.Size returns
// uint64 — one type cannot carry both methods.
type sourceView struct {
	m   *meta
	seq *seqView // exactly one of seq/sk is set
	sk  *seekView
}

func (s *sourceView) Size() uint64 { return uint64(s.m.logical) }

// RunAt classifies offsets from the envelope's map: Data inside a
// stored extent, Hole everywhere else. Zero is never produced — the
// envelope has no such state.
func (s *sourceView) RunAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	return s.m.runAt(offset, limit)
}

// runAt classifies one run of the entry's logical view from its
// extent map: Data inside a stored extent, Hole everywhere else.
func (m *meta) runAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	size := uint64(m.logical)
	if offset >= size {
		return 0, 0, io.EOF
	}
	limEnd := offset + limit
	if limEnd < offset || limEnd > size {
		limEnd = size
	}
	idx := m.extentAt(offset)
	if idx == len(m.extents) {
		return sparse.Hole, limEnd, nil // trailing hole
	}
	e := m.extents[idx]
	if offset < uint64(e.Offset) { // gap before the extent
		end := uint64(e.Offset)
		if end > limEnd {
			end = limEnd
		}
		return sparse.Hole, end, nil
	}
	end := uint64(e.Offset + e.Size)
	if end > limEnd {
		end = limEnd
	}
	return sparse.Data, end, nil
}

// extentAt returns the index of the first extent that ends past
// offset (== len(extents) when offset is in the trailing hole).
func (m *meta) extentAt(offset uint64) int {
	lo, hi := 0, len(m.extents)
	for lo < hi {
		mid := (lo + hi) / 2
		if uint64(m.extents[mid].Offset+m.extents[mid].Size) <= offset {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// readerAtSource is the concurrent random-access sparse.Source over an
// io.ReaderAt (SourceAt). It holds no mutable state: every ReadAt is
// offset arithmetic plus ra.ReadAt, so concurrency is inherited from
// ra.
type readerAtSource struct {
	meta
	ra           io.ReaderAt
	dataStart    int64   // absolute offset of the packed data region
	packedPrefix []int64 // prefix sums of extent sizes
}

func (s *readerAtSource) Size() uint64 { return uint64(s.logical) }

func (s *readerAtSource) RunAt(offset, limit uint64) (sparse.RunKind, uint64, error) {
	return s.meta.runAt(offset, limit)
}

// StoredRangeAt translates a logical Data range into its contiguous byte range
// in the tarstream envelope. The returned file offset is suitable for a
// read-only mmap of the original ReaderAt when it is the backing *os.File.
//
// ok is false for holes. The returned length never crosses a packed extent,
// even when limit extends beyond it.
func (s *readerAtSource) StoredRangeAt(offset, limit uint64) (fileOffset, length uint64, ok bool, err error) {
	if limit == 0 {
		return 0, 0, false, nil
	}
	if offset >= uint64(s.logical) {
		return 0, 0, false, io.EOF
	}
	idx := s.extentAt(offset)
	if idx == len(s.extents) || offset < uint64(s.extents[idx].Offset) {
		return 0, 0, false, nil
	}

	e := s.extents[idx]
	within := int64(offset) - e.Offset
	length = uint64(e.Size - within)
	if length > limit {
		length = limit
	}
	return uint64(s.dataStart + s.packedPrefix[idx] + within), length, true, nil
}

func (s *readerAtSource) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	size := uint64(s.logical)
	if offset >= size {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if offset+uint64(n) > size {
		n = int(size - offset)
		eof = io.EOF
	}
	p := buf[:n]
	pos := int64(offset)
	for len(p) > 0 {
		idx := s.extentAt(uint64(pos))
		if idx < len(s.extents) && pos >= s.extents[idx].Offset {
			// Data: map the logical position into the packed region.
			e := s.extents[idx]
			packed := s.packedPrefix[idx] + (pos - e.Offset)
			m := int64(len(p))
			if rest := e.Offset + e.Size - pos; rest < m {
				m = rest
			}
			read, err := s.ra.ReadAt(p[:m], s.dataStart+packed)
			if err != nil && (err != io.EOF || int64(read) < m) {
				return 0, fmt.Errorf("tarstream: packed read @ %d: %w", pos, err)
			}
			p = p[m:]
			pos += m
			continue
		}
		// Hole (or trailing hole): zeros until the next extent or EOF.
		zeroEnd := s.logical
		if idx < len(s.extents) {
			zeroEnd = s.extents[idx].Offset
		}
		m := int64(len(p))
		if rest := zeroEnd - pos; rest < m {
			m = rest
		}
		clear(p[:m])
		p = p[m:]
		pos += m
	}
	return n, eof
}

func (s *sourceView) ReadAt(ctx context.Context, buf []byte, offset uint64) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	size := uint64(s.m.logical)
	if offset >= size {
		return 0, io.EOF
	}
	n := len(buf)
	var eof error
	if offset+uint64(n) > size {
		n = int(size - offset)
		eof = io.EOF
	}
	if s.sk != nil {
		if err := s.sk.readFullAt(buf[:n], int64(offset)); err != nil {
			return 0, err
		}
		return n, eof
	}
	// One-pass: discard forward, then fill. The view synthesizes
	// zeros across holes, so skipping a hole region never touches the
	// underlying source.
	pos := uint64(s.seq.pos)
	if offset < pos {
		return 0, fmt.Errorf("tarstream: backward ReadAt @ %d (position %d) on one-pass source", offset, pos)
	}
	if offset > pos {
		if _, err := io.CopyN(io.Discard, s.seq, int64(offset-pos)); err != nil {
			return 0, err
		}
	}
	if _, err := io.ReadFull(s.seq, buf[:n]); err != nil {
		return 0, err
	}
	return n, eof
}
