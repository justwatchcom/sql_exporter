// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cloudsqlconn

import (
	"bytes"
	"net"
	"sync"
	"time"
)

type fallbackState int

const (
	stateBeforeFirstRead fallbackState = iota
	stateDuringFirstRead
	stateAfterFirstRead
)

// fallbackConn delegates to a GRPC stream connection, but if it fails with a specific error
// it will retry connecting to a secondary connection.
type fallbackConn struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	cond     *sync.Cond
	state    fallbackState
	closed   bool
	conn     net.Conn
	writebuf *bytes.Buffer

	isFallbackError func(error) bool
	connectFallback func() (net.Conn, error)
}

func newFallbackConn(conn net.Conn, isFallbackError func(error) bool, connectFallback func() (net.Conn, error)) *fallbackConn {
	fc := &fallbackConn{
		conn:            conn,
		isFallbackError: isFallbackError,
		connectFallback: connectFallback,
	}
	fc.cond = sync.NewCond(&fc.mu)
	return fc
}

func (c *fallbackConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}

	if c.state == stateAfterFirstRead {
		conn := c.conn
		c.mu.Unlock()
		return conn.Read(b)
	}

	if c.state == stateDuringFirstRead {
		for c.state == stateDuringFirstRead {
			c.cond.Wait()
		}
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		conn := c.conn
		c.mu.Unlock()
		return conn.Read(b)
	}

	// stateBeforeFirstRead
	c.state = stateDuringFirstRead
	conn := c.conn
	c.mu.Unlock()

	n, err := conn.Read(b)

	c.mu.Lock()
	// Check if closed while we were reading
	if c.closed {
		c.state = stateAfterFirstRead
		c.cond.Broadcast()
		c.mu.Unlock()
		return 0, net.ErrClosed
	}

	if err != nil && c.isFallbackError(err) {
		c.mu.Unlock()

		newConn, dialErr := c.connectFallback()

		if dialErr != nil {
			c.mu.Lock()
			c.state = stateAfterFirstRead
			c.cond.Broadcast()
			c.mu.Unlock()
			return 0, dialErr
		}

		c.writeMu.Lock()

		c.mu.Lock()
		if c.closed {
			c.state = stateAfterFirstRead
			c.cond.Broadcast()
			c.mu.Unlock()
			c.writeMu.Unlock()
			newConn.Close()
			return 0, net.ErrClosed
		}

		// Write cached bytes
		var writeData []byte
		if c.writebuf != nil {
			writeData = c.writebuf.Bytes()
		}

		c.mu.Unlock()

		if len(writeData) > 0 {
			_, writeErr := newConn.Write(writeData)
			if writeErr != nil {
				newConn.Close()
				c.mu.Lock()
				c.state = stateAfterFirstRead
				c.cond.Broadcast()
				c.mu.Unlock()
				c.writeMu.Unlock()
				return 0, writeErr
			}
		}

		c.mu.Lock()
		if c.closed {
			c.state = stateAfterFirstRead
			c.cond.Broadcast()
			c.mu.Unlock()
			c.writeMu.Unlock()
			newConn.Close()
			return 0, net.ErrClosed
		}

		// The stream connection failed; release it before switching over.
		_ = c.conn.Close()
		c.conn = newConn
		c.state = stateAfterFirstRead
		c.cond.Broadcast()
		c.mu.Unlock()

		c.writeMu.Unlock()

		return newConn.Read(b)
	}

	// First read succeeded or failed with non-fallback error
	c.state = stateAfterFirstRead
	c.cond.Broadcast()
	c.mu.Unlock()
	return n, err
}

func (c *fallbackConn) Write(b []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return 0, net.ErrClosed
	}

	if c.state == stateAfterFirstRead {
		conn := c.conn
		c.mu.Unlock()
		return conn.Write(b)
	}

	// For both stateBeforeFirstRead and stateDuringFirstRead:
	// We write to the current connection and cache the bytes.
	conn := c.conn
	c.mu.Unlock()

	n, err := conn.Write(b)
	if err != nil {
		return n, err
	}

	c.mu.Lock()
	// Only cache if we are still not in stateAfterFirstRead
	if c.state != stateAfterFirstRead {
		if c.writebuf == nil {
			c.writebuf = bytes.NewBuffer(nil)
		}
		c.writebuf.Write(b[:n])
	}
	c.mu.Unlock()
	return n, err
}

func (c *fallbackConn) Close() error {
	c.mu.Lock()
	c.closed = true

	if c.state == stateBeforeFirstRead {
		c.state = stateAfterFirstRead
		conn := c.conn
		c.mu.Unlock()
		return conn.Close()
	}

	if c.state == stateDuringFirstRead {
		// Close the underlying connection to unblock the ongoing read
		conn := c.conn
		c.mu.Unlock()
		return conn.Close()
	}

	// stateAfterFirstRead
	conn := c.conn
	c.mu.Unlock()
	return conn.Close()
}

func (c *fallbackConn) LocalAddr() net.Addr {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn.LocalAddr()
}

func (c *fallbackConn) RemoteAddr() net.Addr {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn.RemoteAddr()
}

func (c *fallbackConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn.SetDeadline(t)
}

func (c *fallbackConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	return conn.SetReadDeadline(t)
}

func (c *fallbackConn) SetWriteDeadline(_ time.Time) error {
	return nil
}
