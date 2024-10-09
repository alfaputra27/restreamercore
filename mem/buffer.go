package mem

import (
	"bytes"
	"sync"
)

type BufferPool struct {
	pool sync.Pool
}

func NewBufferPool() *BufferPool {
	p := &BufferPool{
		pool: sync.Pool{
			New: func() any {
				return &bytes.Buffer{}
			},
		},
	}

	return p
}

func (p *BufferPool) Get() *bytes.Buffer {
	buf := p.pool.Get().(*bytes.Buffer)
	buf.Reset()

	return buf
}

func (p *BufferPool) Put(buf *bytes.Buffer) {
	p.pool.Put(buf)
}
