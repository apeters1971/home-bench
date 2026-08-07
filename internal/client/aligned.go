package client

import "unsafe"

const directAlign = 4096

// alignedBuffer returns a byte slice of the given size whose starting
// address is aligned for O_DIRECT / uncached I/O.
func alignedBuffer(size int) []byte {
	if size <= 0 {
		size = directAlign
	}
	raw := make([]byte, size+directAlign)
	addr := uintptr(unsafe.Pointer(&raw[0]))
	off := int(addr % uintptr(directAlign))
	if off != 0 {
		off = directAlign - off
	}
	return raw[off : off+size]
}
