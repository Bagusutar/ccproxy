//go:build !windows

package main

// IsElevated 仅在 Windows 上有意义（UIPI 特权隔离）。
func IsElevated() bool { return false }
