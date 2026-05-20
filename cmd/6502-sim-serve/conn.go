package main

import (
	"bufio"
	"net"
)

// ndjsonConn adapts a net.Conn into a bridge.Conn that frames messages
// as newline-delimited JSON. One JSON object per line, both
// directions. The bridge package serialises writes via its own
// writeMu, so the embedded bufio.Writer doesn't need its own lock.
type ndjsonConn struct {
	raw net.Conn
	r   *bufio.Reader
	w   *bufio.Writer
}

func newNDJSONConn(c net.Conn) *ndjsonConn {
	return &ndjsonConn{
		raw: c,
		r:   bufio.NewReader(c),
		w:   bufio.NewWriter(c),
	}
}

// Read returns one whole JSON frame (without the trailing newline /
// CR). EOF (peer disconnect) surfaces as io.EOF, which the bridge
// Serve loop treats as a clean shutdown.
func (n *ndjsonConn) Read() ([]byte, error) {
	line, err := n.r.ReadBytes('\n')
	if err != nil {
		// Return partial line + the error so the caller can distinguish
		// EOF-with-data from EOF-clean. The bridge ignores partial-frame
		// content on a read error, so this is mostly cosmetic.
		return trimLineEnd(line), err
	}
	return trimLineEnd(line), nil
}

// Write sends one JSON frame followed by a single '\n'. bufio.Writer
// is flushed per frame so the peer sees each notification / response
// as soon as the handler enqueues it.
func (n *ndjsonConn) Write(b []byte) error {
	if _, err := n.w.Write(b); err != nil {
		return err
	}
	if err := n.w.WriteByte('\n'); err != nil {
		return err
	}
	return n.w.Flush()
}

func (n *ndjsonConn) Close() error { return n.raw.Close() }

func trimLineEnd(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
