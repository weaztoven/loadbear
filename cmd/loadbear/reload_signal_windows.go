//go:build windows

package main

import "os"

func registerReloadSignal() (<-chan os.Signal, func()) {
	return nil, func() {}
}
