//go:build windows

package main

import "os"

func daemonSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}
