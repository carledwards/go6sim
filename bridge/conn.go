package bridge

import (
	"errors"
	"io"
	"sync"
)

// Conn is the bridge's transport seam: one whole JSON object per Read /
// Write. A WebSocket frame fits naturally; tests use [Pipe] to get an
// in-memory pair without spinning up a network.
type Conn interface {
	Read() ([]byte, error) // one JSON frame
	Write([]byte) error    // one JSON frame
	Close() error
}

// Pipe returns two connected in-memory Conns: a frame written to one is
// readable from the other. Closing either side EOFs both reads — the
// symmetry matches how a real socket close behaves.
func Pipe() (Conn, Conn) {
	a := make(chan []byte, 16)
	b := make(chan []byte, 16)
	closeA := make(chan struct{})
	closeB := make(chan struct{})
	return &pipeConn{in: a, out: b, mine: closeA, peer: closeB},
		&pipeConn{in: b, out: a, mine: closeB, peer: closeA}
}

type pipeConn struct {
	in, out    chan []byte
	mine, peer chan struct{}
	once       sync.Once
}

func (p *pipeConn) Read() ([]byte, error) {
	select {
	case b := <-p.in:
		return b, nil
	case <-p.mine:
		return nil, io.EOF
	case <-p.peer:
		return nil, io.EOF
	}
}

func (p *pipeConn) Write(b []byte) error {
	select {
	case p.out <- b:
		return nil
	case <-p.mine:
		return errors.New("bridge: write on closed Conn")
	case <-p.peer:
		return io.ErrClosedPipe
	}
}

func (p *pipeConn) Close() error {
	p.once.Do(func() { close(p.mine) })
	return nil
}
