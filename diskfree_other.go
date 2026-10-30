//go:build !(darwin || linux || freebsd || windows)

package main

import "errors"

func freeSpace(string) (uint64, error) {
	return 0, errors.New("free space unknown on this platform")
}
