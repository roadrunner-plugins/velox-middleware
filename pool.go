package veloxmiddleware

import (
	"sync"
)

// bufferPool is a sync.Pool for reusing byte slices during file streaming.
// This reduces GC pressure and improves performance for large file transfers.
//
// Buffer size is set to 10MB to match RoadRunner's sendfile plugin pattern.
// Larger buffers reduce the number of read/write syscalls for large binaries.
var bufferPool = sync.Pool{
	New: func() interface{} {
		// 10MB buffer size - same as sendfile plugin (bufSize = 10 * 1024 * 1024)
		buf := make([]byte, 10*1024*1024)
		return buf
	},
}
