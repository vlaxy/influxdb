package durablequeue

import (
	"fmt"
	"io"
)

type Scanner interface {
	// Next returns the current block and advances the scanner to the next block.
	Next() bool

	// Err returns any non io.EOF error as a result of calling the Next function.
	Err() error

	// Bytes returns the most recent block generated by a call to Next. A new buffer
	// is generated with each call to Next, so the buffer may be retained by the caller.
	Bytes() []byte

	// Advance moves the head pointer to the next byte slice in the queue.
	// Advance is guaranteed to make forward progress and is idempotent.
	Advance() (int64, error)
}

type queueScanner struct {
	q  *Queue
	ss *segmentScanner
}

func (qs *queueScanner) Next() bool {
	return qs.ss.Next()
}

func (qs *queueScanner) Err() error {
	return qs.ss.Err()
}

func (qs *queueScanner) Bytes() []byte {
	return qs.ss.Bytes()
}

func (qs *queueScanner) Advance() (n int64, err error) {
	n, err = qs.ss.Advance()
	// always advance to the next segment if the current segment presents any error
	// condition, which either indicates success (io.EOF) or corruption of some kind.
	if err != nil {
		qs.q.mu.Lock()
		defer qs.q.mu.Unlock()

		// retry under lock - otherwise it is possible a write happened between getting the EOF
		// and taking the queue lock.
		if err == io.EOF {
			n, err = qs.ss.Advance()
			if err == nil {
				return n, nil
			}
		}

		// If the error was not EOF, force the segment to be trimmed
		force := err != io.EOF
		if trimErr := qs.q.trimHead(force); trimErr != nil {
			return 0, trimErr
		}
		if err != io.EOF {
			// We are dropping writes due to this error, so we should report it
			return n, fmt.Errorf("dropped bad disk queue segment: %w", err)
		}
	}
	return n, nil
}

type segmentScanner struct {
	s   *segment
	pos int64
	n   int64
	buf []byte
	err error
	eof bool

	//TODO(SGC): consider adding backing buffer once we send writes to remote node as single array
}

var _ Scanner = (*segmentScanner)(nil)

func (l *segment) newScanner() (*segmentScanner, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// If we're at the end of the file, can't advance
	if int64(l.pos) == l.size-footerSize {
		return nil, io.EOF
	}

	if err := l.seekToCurrent(); err != nil {
		return nil, err
	}

	return &segmentScanner{s: l, pos: l.pos}, nil
}

func (ss *segmentScanner) Next() bool {
	ss.s.mu.Lock()
	defer ss.s.mu.Unlock()

	if ss.eof || ss.err != nil {
		return false
	}

	if err := ss.s.seek(ss.pos); err != nil {
		ss.setErr(err)
		return false
	}

	for {
		if int64(ss.pos) == ss.s.size-footerSize {
			ss.eof = true
			return false
		}

		ss.n++
		// read the record size
		sz, err := ss.s.readUint64()
		if err == io.EOF {
			return false
		} else if err != nil {
			ss.setErr(err)
			return false
		}

		ss.pos += 8 + int64(sz)
		if sz == 0 {
			continue
		}

		if sz > uint64(ss.s.maxSize) {
			ss.setErr(fmt.Errorf("record size out of range: max %d: got %d", ss.s.maxSize, sz))
			return false
		}

		// The node processor will hold a reference to ss.buf via the Bytes method,
		// so it's important to create a new slice here,
		// even though it looks like we could reslice ss.buf.
		ss.buf = make([]byte, sz)

		if err := ss.s.readBytes(ss.buf); err != nil {
			ss.setErr(err)
			return false
		}

		return true
	}
}

func (ss *segmentScanner) setErr(err error) {
	ss.err = err
	ss.buf = nil
}

func (ss *segmentScanner) Err() error {
	return ss.err
}

func (ss *segmentScanner) Bytes() []byte {
	return ss.buf
}

func (ss *segmentScanner) Advance() (int64, error) {
	if ss.err != nil {
		return ss.n, ss.err
	}
	return ss.n, ss.s.advanceTo(ss.pos)
}