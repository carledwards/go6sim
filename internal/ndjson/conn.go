// Package ndjson adapts a net.Conn into a bridge.Conn that frames
// messages as newline-delimited JSON — one JSON object per `\n`, both
// directions. Used by every TCP-attached bridge listener in this
// repo (cmd/6502-sim-serve, cmd/6502-sim --serve). The bridge package
// itself serialises writes via its own writeMu, so the embedded
// bufio.Writer doesn't need its own lock.
package ndjson

import (
	"bufio"
	"net"
)

// Conn wraps a net.Conn to satisfy bridge.Conn. One JSON frame per
// line on the wire; trailing CR/LF stripped from reads.
type Conn struct {
	raw net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
}

// New wraps the given net.Conn. The caller still owns it for any
// further configuration (deadlines, KeepAlive, etc.) but should let
// Conn.Close be the one to actually close it.
func New(c net.Conn) *Conn {
	return &Conn{
		raw: c,
		r:   bufio.NewReader(c),
		w:   bufio.NewWriter(c),
	}
}

// Read returns one whole JSON frame (trailing newline stripped). EOF
// surfaces as io.EOF, which the bridge Serve loop treats as a clean
// shutdown.
func (c *Conn) Read() ([]byte, error) {
	line, err := c.r.ReadBytes('\n')
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, err
}

// Write sends one JSON frame followed by a single '\n', flushing
// per frame so notifications are visible immediately on the wire.
func (c *Conn) Write(b []byte) error {
	if _, err := c.w.Write(b); err != nil {
		return err
	}
	if err := c.w.WriteByte('\n'); err != nil {
		return err
	}
	return c.w.Flush()
}

// Close closes the underlying socket. Idempotent (net.Conn.Close is).
func (c *Conn) Close() error { return c.raw.Close() }
