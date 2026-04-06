//go:build !linux

package main

import "fmt"

func installSystemdService(cfg config) error {
	return fmt.Errorf("install is only supported on Linux")
}
