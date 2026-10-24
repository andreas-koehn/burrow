// Package bridge wires two net.Conns together for a tunnel data stream.
package bridge

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// Pipe copies a→b and b→a concurrently, adding the a→b byte count to in and
// the b→a count to out. When either direction finishes (EOF or error), BOTH
// conns are closed so the other Copy unblocks; Pipe returns once both are done.
// (yamux.Stream has no CloseWrite; close-both-on-first-EOF is the correct,
// panic-free TCP-proxy semantics — see spec DP3.)
func Pipe(a, b net.Conn, in, out *atomic.Uint64) {
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = a.Close(); _ = b.Close() }) }

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		n, _ := io.Copy(b, a)
		in.Add(uint64(n))
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		n, _ := io.Copy(a, b)
		out.Add(uint64(n))
		closeBoth()
	}()
	wg.Wait()
}

// Side names a conn of PipeSides.
type Side int32

const (
	// SideA is the first conn of PipeSides, SideB the second.
	SideA Side = 1
	SideB Side = 2
)

// PipeSides is Pipe that also says which conn ended the pipe: the one whose
// read ended first (EOF, an error, a close), or the one written to when a
// copy ended on a write. Its copies go through Read, so a conn's own
// WriteTo is not used; Pipe keeps that for the other callers.
func PipeSides(a, b net.Conn, in, out *atomic.Uint64) Side {
	var once sync.Once
	closeBoth := func() { once.Do(func() { _ = a.Close(); _ = b.Close() }) }
	var first atomic.Int32
	half := func(dst, src net.Conn, dstSide, srcSide Side, count *atomic.Uint64) {
		r := &endReader{r: src}
		n, _ := io.Copy(dst, r)
		count.Add(uint64(n))
		side := dstSide
		if r.ended {
			side = srcSide
		}
		// Before the close: the other copy ends because of it.
		first.CompareAndSwap(0, int32(side))
		closeBoth()
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		half(b, a, SideB, SideA, in)
	}()
	go func() {
		defer wg.Done()
		half(a, b, SideA, SideB, out)
	}()
	wg.Wait()
	return Side(first.Load())
}

// endReader notes that its reader's Read failed (EOF included).
type endReader struct {
	r     io.Reader
	ended bool
}

func (e *endReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil {
		e.ended = true
	}
	return n, err
}
