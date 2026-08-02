//go:build windows

package session

import "unsafe"

func unsafePointer[T any](value *T) unsafe.Pointer {
	return unsafe.Pointer(value)
}
