package proxy

import (
	"bytes"
	"sync"
)

// syncBuffer is a bytes.Buffer that tolerates being written by the code under
// test while a test goroutine reads it. Handing an unsynchronised buffer to a
// logger the proxy writes from another goroutine is a data race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
