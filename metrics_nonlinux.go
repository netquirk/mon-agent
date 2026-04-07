//go:build !linux

package main

import (
	"fmt"
)

type ramBreakdown struct {
	used   float64
	free   float64
	shared float64
	buff   float64
}

type netCounters struct {
	rxBytes   uint64
	txBytes   uint64
	rxPackets uint64
	txPackets uint64
}

type diskCounters struct {
	readIOs      uint64
	writeIOs     uint64
	readSectors  uint64
	writeSectors uint64
}

func readCPUSnapshot() (cpuSnapshot, error) {
	return cpuSnapshot{}, nil
}

func cpuBreakdownPercent(prev, current cpuSnapshot) (cpuBreakdown, error) {
	return cpuBreakdown{}, nil
}

func readRAMBreakdownPercent() (ramBreakdown, error) {
	// Keep the agent functional on non-Linux builds until native collectors are added.
	return ramBreakdown{
		used:   0,
		free:   0,
		shared: 0,
		buff:   0,
	}, nil
}

func readDiskUsedPercent(path string) (float64, error) {
	return 0, fmt.Errorf("disk usage collection is not implemented on this platform")
}

func discoverDiskTargets(paths []string, hasBtrfsBinary bool) []diskTarget {
	targets := make([]diskTarget, 0, len(paths))
	for _, path := range paths {
		targets = append(targets, diskTarget{
			path:       path,
			fsType:     "unsupported",
			majorMinor: "",
			deviceName: "",
		})
	}
	return targets
}

func autoDiscoverDiskPaths() []string {
	return []string{"/"}
}

func readInodeUsagePercent(target diskTarget, btrfsBinary string) (float64, error) {
	return 0, fmt.Errorf("inode usage collection is not implemented on this platform")
}

func readNetSnapshot() (netSnapshot, error) {
	return make(netSnapshot), nil
}

func readDiskSnapshot() (diskSnapshot, error) {
	return nil, fmt.Errorf("disk counter snapshot is not implemented on this platform")
}

func diskRatesForTarget(
	prev diskSnapshot,
	current diskSnapshot,
	target diskTarget,
	elapsedSeconds float64,
) (uint64, uint64, uint64, error) {
	return 0, 0, 0, fmt.Errorf("disk IOPS/throughput collection is not implemented on this platform")
}

func readLVMThinUsage() (map[string]uint64, error) {
	return nil, fmt.Errorf("lvm thin metrics are not implemented on this platform")
}
