//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

func readLoadAverageScaled() (uint64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0, errors.New("unexpected /proc/loadavg format")
	}
	val, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse loadavg: %w", err)
	}
	if val < 0 {
		val = 0
	}
	return uint64((val * 100.0) + 0.5), nil
}

func readCPUSnapshot() (cpuSnapshot, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSnapshot{}, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return cpuSnapshot{}, errors.New("unexpected /proc/stat format")
		}
		var total uint64
		for i := 1; i < len(fields); i++ {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return cpuSnapshot{}, fmt.Errorf("parse cpu field %q: %w", fields[i], err)
			}
			total += v
		}

		user, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return cpuSnapshot{}, fmt.Errorf("parse user: %w", err)
		}
		nice, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			return cpuSnapshot{}, fmt.Errorf("parse nice: %w", err)
		}
		system, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			return cpuSnapshot{}, fmt.Errorf("parse system: %w", err)
		}
		iowait, err := strconv.ParseUint(fields[5], 10, 64)
		if err != nil {
			return cpuSnapshot{}, fmt.Errorf("parse iowait: %w", err)
		}
		var steal uint64
		if len(fields) > 8 {
			steal, err = strconv.ParseUint(fields[8], 10, 64)
			if err != nil {
				return cpuSnapshot{}, fmt.Errorf("parse steal: %w", err)
			}
		}

		return cpuSnapshot{
			user:   user + nice,
			system: system,
			iowait: iowait,
			steal:  steal,
			total:  total,
		}, nil
	}

	if err := scanner.Err(); err != nil {
		return cpuSnapshot{}, err
	}
	return cpuSnapshot{}, errors.New("cpu line not found in /proc/stat")
}

func cpuBreakdownPercent(prev, current cpuSnapshot) (cpuBreakdown, error) {
	if current.total <= prev.total {
		return cpuBreakdown{}, errors.New("invalid cpu snapshot progression")
	}
	totalDelta := current.total - prev.total

	user := pctDelta(prev.user, current.user, totalDelta)
	system := pctDelta(prev.system, current.system, totalDelta)
	iowait := pctDelta(prev.iowait, current.iowait, totalDelta)
	steal := pctDelta(prev.steal, current.steal, totalDelta)

	return cpuBreakdown{
		user:   clampPct(user),
		system: clampPct(system),
		iowait: clampPct(iowait),
		steal:  clampPct(steal),
	}, nil
}

type ramBreakdown struct {
	used   float64
	free   float64
	shared float64
	buff   float64
}

func readRAMBreakdownPercent() (ramBreakdown, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return ramBreakdown{}, err
	}
	defer f.Close()

	var totalKB uint64
	var freeKB uint64
	var availableKB uint64
	var sharedKB uint64
	var buffersKB uint64
	var cachedKB uint64
	var sreclaimableKB uint64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "MemTotal:") {
			totalKB, err = parseMeminfoValue(line)
			if err != nil {
				return ramBreakdown{}, err
			}
		}
		if strings.HasPrefix(line, "MemFree:") {
			freeKB, err = parseMeminfoValue(line)
			if err != nil {
				return ramBreakdown{}, err
			}
		}
		if strings.HasPrefix(line, "MemAvailable:") {
			availableKB, err = parseMeminfoValue(line)
			if err != nil {
				return ramBreakdown{}, err
			}
		}
		if strings.HasPrefix(line, "Shmem:") {
			sharedKB, err = parseMeminfoValue(line)
			if err != nil {
				return ramBreakdown{}, err
			}
		}
		if strings.HasPrefix(line, "Buffers:") {
			buffersKB, err = parseMeminfoValue(line)
			if err != nil {
				return ramBreakdown{}, err
			}
		}
		if strings.HasPrefix(line, "Cached:") {
			cachedKB, err = parseMeminfoValue(line)
			if err != nil {
				return ramBreakdown{}, err
			}
		}
		if strings.HasPrefix(line, "SReclaimable:") {
			sreclaimableKB, err = parseMeminfoValue(line)
			if err != nil {
				return ramBreakdown{}, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ramBreakdown{}, err
	}
	if totalKB == 0 {
		return ramBreakdown{}, errors.New("MemTotal missing from /proc/meminfo")
	}
	if freeKB > totalKB {
		freeKB = totalKB
	}

	// Match Linux `free` semantics more closely:
	// used ~= MemTotal - MemAvailable
	// buff/cache ~= Buffers + Cached + SReclaimable - Shmem
	if availableKB == 0 {
		availableKB = freeKB + buffersKB + cachedKB + sreclaimableKB
	}
	if availableKB > totalKB {
		availableKB = totalKB
	}

	usedKB := totalKB - availableKB

	buffCacheKB := buffersKB + cachedKB + sreclaimableKB
	if buffCacheKB > sharedKB {
		buffCacheKB -= sharedKB
	} else {
		buffCacheKB = 0
	}
	if buffCacheKB > totalKB {
		buffCacheKB = totalKB
	}

	return ramBreakdown{
		used:   clampPct((float64(usedKB) / float64(totalKB)) * 100.0),
		free:   clampPct((float64(freeKB) / float64(totalKB)) * 100.0),
		shared: clampPct((float64(sharedKB) / float64(totalKB)) * 100.0),
		buff:   clampPct((float64(buffCacheKB) / float64(totalKB)) * 100.0),
	}, nil
}

func pctDelta(prev, cur, totalDelta uint64) float64 {
	if cur <= prev || totalDelta == 0 {
		return 0
	}
	return (float64(cur-prev) / float64(totalDelta)) * 100.0
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func parseMeminfoValue(line string) (uint64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("unexpected meminfo line %q", line)
	}
	return strconv.ParseUint(fields[1], 10, 64)
}

func readDiskUsedPercent(path string) (float64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 0, errors.New("filesystem reports zero blocks")
	}
	usedBlocks := stat.Blocks - stat.Bavail
	return (float64(usedBlocks) / float64(stat.Blocks)) * 100.0, nil
}

func discoverDiskTargets(paths []string, hasBtrfsBinary bool) []diskTarget {
	mounts := readMountInfo()
	targets := make([]diskTarget, 0, len(paths))
	for _, path := range paths {
		mount := lookupMount(path, mounts)
		fsType := "unknown"
		majorMinor := ""
		deviceName := ""
		if mount != nil {
			fsType = mount.fsType
			majorMinor = mount.majorMinor
			deviceName = mount.deviceName
		}
		useBtrfs := hasBtrfsBinary && fsType == "btrfs"
		targets = append(targets, diskTarget{
			path:                 path,
			fsType:               fsType,
			majorMinor:           majorMinor,
			deviceName:           deviceName,
			useBtrfsInodeCommand: useBtrfs,
		})
	}
	return targets
}

func autoDiscoverDiskPaths() []string {
	mounts := readMountInfo()
	if len(mounts) == 0 {
		return []string{"/"}
	}

	ignoreFSTypes := map[string]struct{}{
		"proc":        {},
		"sysfs":       {},
		"tmpfs":       {},
		"devtmpfs":    {},
		"devpts":      {},
		"cgroup":      {},
		"cgroup2":     {},
		"mqueue":      {},
		"hugetlbfs":   {},
		"pstore":      {},
		"securityfs":  {},
		"tracefs":     {},
		"configfs":    {},
		"autofs":      {},
		"overlay":     {},
		"squashfs":    {},
		"nsfs":        {},
		"rpc_pipefs":  {},
		"fusectl":     {},
		"binfmt_misc": {},
		"debugfs":     {},
		"selinuxfs":   {},
		"ramfs":       {},
		"efivarfs":    {},
	}

	skipPrefix := []string{
		"/proc",
		"/sys",
		"/dev",
		"/run",
		"/snap",
		"/var/lib/docker",
		"/var/lib/containers",
	}

	seen := make(map[string]struct{})
	paths := make([]string, 0, len(mounts))
	for _, m := range mounts {
		p := strings.TrimSpace(m.mountPoint)
		if p == "" || !strings.HasPrefix(p, "/") {
			continue
		}
		if _, skip := ignoreFSTypes[m.fsType]; skip && p != "/tmp" {
			continue
		}
		skip := false
		for _, prefix := range skipPrefix {
			if p == prefix || strings.HasPrefix(p, prefix+"/") {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	if _, ok := seen["/"]; !ok {
		paths = append(paths, "/")
	}

	sort.Slice(paths, func(i, j int) bool {
		if paths[i] == "/" {
			return true
		}
		if paths[j] == "/" {
			return false
		}
		return paths[i] < paths[j]
	})
	return paths
}

func readInodeUsagePercent(target diskTarget, btrfsBinary string) (float64, error) {
	if target.useBtrfsInodeCommand {
		percent, err := readBtrfsInodeUsagePercent(target.path, btrfsBinary)
		if err == nil {
			return percent, nil
		}
		// Fall through to statfs as a best-effort fallback.
	}

	var stat syscall.Statfs_t
	if err := syscall.Statfs(target.path, &stat); err != nil {
		return 0, err
	}
	if stat.Files == 0 {
		return 0, errors.New("filesystem reports zero inodes")
	}
	used := stat.Files - stat.Ffree
	return clampPct((float64(used) / float64(stat.Files)) * 100.0), nil
}

func readBtrfsInodeUsed(path, btrfsBinary string) (uint64, error) {
	if strings.TrimSpace(btrfsBinary) == "" {
		return 0, errors.New("btrfs binary not available")
	}

	output, err := exec.Command(btrfsBinary, "filesystem", "usage", "--raw", "-T", path).CombinedOutput()
	if err != nil {
		output, err = exec.Command(btrfsBinary, "filesystem", "usage", "-T", path).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("btrfs usage command failed: %w (%s)", err, strings.TrimSpace(string(output)))
		}
	}

	count, err := parseBtrfsInodeUsed(string(output))
	if err != nil {
		return 0, fmt.Errorf("parse btrfs inode used: %w", err)
	}
	return count, nil
}

func readBtrfsInodeUsagePercent(path, btrfsBinary string) (float64, error) {
	if strings.TrimSpace(btrfsBinary) == "" {
		return 0, errors.New("btrfs binary not available")
	}

	output, err := exec.Command(btrfsBinary, "filesystem", "usage", "--raw", "-T", path).CombinedOutput()
	if err != nil {
		output, err = exec.Command(btrfsBinary, "filesystem", "usage", "-T", path).CombinedOutput()
		if err != nil {
			return 0, fmt.Errorf("btrfs usage command failed: %w (%s)", err, strings.TrimSpace(string(output)))
		}
	}

	percent, err := parseBtrfsInodeUsagePercent(string(output))
	if err != nil {
		return 0, fmt.Errorf("parse btrfs inode usage percent: %w", err)
	}
	return percent, nil
}

func parseBtrfsInodeUsagePercent(output string) (float64, error) {
	lines := strings.Split(output, "\n")
	reUsed := regexp.MustCompile(`(?i)used[= :]+([0-9][0-9,._A-Za-z]*)`)
	reTotal := regexp.MustCompile(`(?i)total[= :]+([0-9][0-9,._A-Za-z]*)`)

	for _, line := range lines {
		lineLower := strings.ToLower(line)
		if !strings.Contains(lineLower, "inode") {
			continue
		}

		usedMatch := reUsed.FindStringSubmatch(line)
		totalMatch := reTotal.FindStringSubmatch(line)
		if len(usedMatch) < 2 || len(totalMatch) < 2 {
			continue
		}

		used, err := parseCountToken(usedMatch[1])
		if err != nil {
			continue
		}
		total, err := parseCountToken(totalMatch[1])
		if err != nil || total == 0 {
			continue
		}
		return clampPct((float64(used) / float64(total)) * 100.0), nil
	}

	return 0, errors.New("inode used/total values not found")
}

func parseBtrfsInodeUsed(output string) (uint64, error) {
	lines := strings.Split(output, "\n")
	re := regexp.MustCompile(`(?i)used[= :]+([0-9][0-9,._A-Za-z]*)`)
	for _, line := range lines {
		lineLower := strings.ToLower(line)
		if !strings.Contains(lineLower, "inode") {
			continue
		}
		m := re.FindStringSubmatch(line)
		if len(m) < 2 {
			continue
		}
		value, err := parseCountToken(m[1])
		if err == nil {
			return value, nil
		}
	}
	return 0, errors.New("inode used value not found")
}

func parseCountToken(token string) (uint64, error) {
	token = strings.TrimSpace(token)
	token = strings.TrimSuffix(token, ",")
	token = strings.ReplaceAll(token, ",", "")
	token = strings.ReplaceAll(token, "_", "")

	multiplier := float64(1)
	last := token[len(token)-1]
	switch last {
	case 'k', 'K':
		multiplier = 1_000
		token = token[:len(token)-1]
	case 'm', 'M':
		multiplier = 1_000_000
		token = token[:len(token)-1]
	case 'g', 'G':
		multiplier = 1_000_000_000
		token = token[:len(token)-1]
	case 't', 'T':
		multiplier = 1_000_000_000_000
		token = token[:len(token)-1]
	}

	value, err := strconv.ParseFloat(token, 64)
	if err != nil {
		return 0, err
	}
	if value < 0 {
		return 0, errors.New("negative value")
	}
	return uint64(value * multiplier), nil
}

type mountInfo struct {
	mountPoint string
	fsType     string
	majorMinor string
	deviceName string
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

func readMountInfo() []mountInfo {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil
	}
	defer f.Close()

	var mounts []mountInfo
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, " - ")
		if len(parts) != 2 {
			continue
		}
		left := strings.Fields(parts[0])
		right := strings.Fields(parts[1])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		deviceName := ""
		if len(right) > 1 {
			deviceName = right[1]
		}
		mounts = append(mounts, mountInfo{
			mountPoint: unescapeMountField(left[4]),
			fsType:     right[0],
			majorMinor: left[2],
			deviceName: deviceName,
		})
	}

	sort.Slice(mounts, func(i, j int) bool {
		return len(mounts[i].mountPoint) > len(mounts[j].mountPoint)
	})
	return mounts
}

func lookupFsType(path string, mounts []mountInfo) string {
	m := lookupMount(path, mounts)
	if m == nil {
		return "unknown"
	}
	return m.fsType
}

func lookupMount(path string, mounts []mountInfo) *mountInfo {
	cleanPath := path
	if cleanPath == "" {
		return nil
	}
	for i := range mounts {
		m := &mounts[i]
		if cleanPath == m.mountPoint || strings.HasPrefix(cleanPath, m.mountPoint+"/") {
			return m
		}
	}
	return nil
}

func unescapeMountField(v string) string {
	replacer := strings.NewReplacer(
		`\\`, `\`,
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(v)
}

func readNetSnapshot() (netSnapshot, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(netSnapshot)
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())
		if lineNum <= 2 || line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "" || iface == "lo" {
			continue
		}

		fields := strings.Fields(parts[1])
		if len(fields) < 16 {
			continue
		}
		rxBytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			continue
		}
		rxPackets, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		txBytes, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			continue
		}
		txPackets, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}

		out[iface] = netCounters{
			rxBytes:   rxBytes,
			txBytes:   txBytes,
			rxPackets: rxPackets,
			txPackets: txPackets,
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func readDiskSnapshot() (diskSnapshot, error) {
	f, err := os.Open("/proc/diskstats")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(diskSnapshot)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 14 {
			continue
		}

		key := fields[0] + ":" + fields[1]
		readIOs, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			continue
		}
		readSectors, err := strconv.ParseUint(fields[5], 10, 64)
		if err != nil {
			continue
		}
		writeIOs, err := strconv.ParseUint(fields[7], 10, 64)
		if err != nil {
			continue
		}
		writeSectors, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}

		out[key] = diskCounters{
			readIOs:      readIOs,
			writeIOs:     writeIOs,
			readSectors:  readSectors,
			writeSectors: writeSectors,
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func diskRatesForTarget(
	prev diskSnapshot,
	current diskSnapshot,
	target diskTarget,
	elapsedSeconds float64,
) (uint64, uint64, uint64, uint64, error) {
	if elapsedSeconds <= 0 {
		return 0, 0, 0, 0, errors.New("elapsed time must be > 0")
	}
	if strings.TrimSpace(target.majorMinor) == "" {
		return 0, 0, 0, 0, errors.New("target has no major:minor mapping")
	}

	prevCounters, ok := prev[target.majorMinor]
	if !ok {
		return 0, 0, 0, 0, errors.New("previous disk counters not found")
	}
	currentCounters, ok := current[target.majorMinor]
	if !ok {
		return 0, 0, 0, 0, errors.New("current disk counters not found")
	}

	readIOPSDelta := counterDelta(currentCounters.readIOs, prevCounters.readIOs)
	writeIOPSDelta := counterDelta(currentCounters.writeIOs, prevCounters.writeIOs)
	const sectorSizeBytes = 512
	readBytesDelta := counterDelta(currentCounters.readSectors, prevCounters.readSectors) * sectorSizeBytes
	writeBytesDelta := counterDelta(currentCounters.writeSectors, prevCounters.writeSectors) * sectorSizeBytes

	readIOPS := uint64((float64(readIOPSDelta) / elapsedSeconds) + 0.5)
	writeIOPS := uint64((float64(writeIOPSDelta) / elapsedSeconds) + 0.5)
	throughputRead := uint64((float64(readBytesDelta) / elapsedSeconds) + 0.5)
	throughputWrite := uint64((float64(writeBytesDelta) / elapsedSeconds) + 0.5)
	return readIOPS, writeIOPS, throughputRead, throughputWrite, nil
}

func readLVMThinUsage() (map[string]uint64, error) {
	output, err := exec.Command(
		"lvs",
		"--reportformat", "json",
		"--units", "p",
		"--nosuffix",
		"-o", "vg_name,lv_name,lv_attr,data_percent,metadata_percent",
	).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("lvs command failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}

	type lvsRow struct {
		VGName          string `json:"vg_name"`
		LVName          string `json:"lv_name"`
		LVAttr          string `json:"lv_attr"`
		DataPercent     string `json:"data_percent"`
		MetadataPercent string `json:"metadata_percent"`
	}
	type lvsReport struct {
		Report []struct {
			LV []lvsRow `json:"lv"`
		} `json:"report"`
	}

	var parsed lvsReport
	if err := json.Unmarshal(output, &parsed); err != nil {
		return nil, fmt.Errorf("parse lvs json: %w", err)
	}

	out := make(map[string]uint64)
	for _, report := range parsed.Report {
		for _, row := range report.LV {
			vg := strings.TrimSpace(row.VGName)
			lv := strings.TrimSpace(row.LVName)
			attr := strings.TrimSpace(row.LVAttr)
			if vg == "" || lv == "" || attr == "" {
				continue
			}
			if strings.HasPrefix(lv, "tpl_") {
				continue
			}
			kind := attr[0]
			if kind != 't' && kind != 'V' {
				continue
			}

			dataPct, dataOK := parseLVMPercentToken(row.DataPercent)
			metaPct, metaOK := parseLVMPercentToken(row.MetadataPercent)
			if dataOK || metaOK {
				out[lvmPackedMetricKey(vg, lv)] = packU32x2(dataPct, metaPct)
			}
		}
	}

	return out, nil
}

func parseLVMPercentToken(raw string) (uint64, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || value == "-" {
		return 0, false
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return percentToScaled100Uint64(f), true
}
