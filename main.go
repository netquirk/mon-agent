package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type config struct {
	monitorID   string
	pushBaseURL string
	pushURL     string
	interval    time.Duration
	timeout     time.Duration
	diskPaths   []string
	location    string
	oneshot     bool
	install     bool
	serviceName string
	installUser string
	binaryPath  string
	envFilePath string
	servicePath string
	showVersion bool
	includeRAM  bool
	includeCPU  bool
	includeNet  bool
	insecureTLS bool
}

type diskTarget struct {
	path                 string
	fsType               string
	majorMinor           string
	deviceName           string
	useBtrfsInodeCommand bool
}

type cpuSnapshot struct {
	user   uint64
	system uint64
	iowait uint64
	steal  uint64
	total  uint64
}

type netSnapshot map[string]netCounters
type diskSnapshot map[string]diskCounters

type cpuBreakdown struct {
	user   float64
	system float64
	iowait float64
	steal  float64
}

type pushPayload struct {
	AgentVersion int               `json:"agent_version"`
	Timestamp    int64             `json:"ts"`
	Metrics      map[string]uint64 `json:"metrics"`
}

var version = "dev"

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if cfg.showVersion {
		fmt.Println(version)
		return
	}
	if cfg.install {
		if err := installSystemdService(cfg); err != nil {
			log.Fatalf("install failed: %v", err)
		}
		log.Printf("installed and started systemd service %q", cfg.serviceName)
		return
	}

	httpClient := &http.Client{
		Timeout: cfg.timeout,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
	}
	if cfg.insecureTLS {
		transport := httpClient.Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = newInsecureTLSConfig()
		httpClient.Transport = transport
	}

	var prevCPU cpuSnapshot
	if cfg.includeCPU {
		prevCPU, err = readCPUSnapshot()
		if err != nil {
			log.Fatalf("failed to read initial CPU snapshot: %v", err)
		}
	}
	var prevNet netSnapshot
	if cfg.includeNet {
		prevNet, err = readNetSnapshot()
		if err != nil {
			log.Fatalf("failed to read initial net snapshot: %v", err)
		}
	}
	var prevDisk diskSnapshot
	prevDiskAt := time.Now()
	if snapshot, snapErr := readDiskSnapshot(); snapErr == nil {
		prevDisk = snapshot
		prevDiskAt = time.Now()
	} else {
		log.Printf("disk iops/throughput disabled: failed to read initial disk snapshot: %v", snapErr)
	}

	btrfsBinary := ""
	if p, err := exec.LookPath("btrfs"); err == nil {
		btrfsBinary = p
	}
	hasLVS := false
	if _, err := exec.LookPath("lvs"); err == nil {
		hasLVS = true
	}
	diskTargets := discoverDiskTargets(cfg.diskPaths, btrfsBinary != "")
	for _, target := range diskTargets {
		log.Printf(
			"disk target path=%s fs=%s dev=%s major_minor=%s inode_source=%s",
			target.path,
			target.fsType,
			target.deviceName,
			target.majorMinor,
			map[bool]string{true: "btrfs-cli", false: "statfs"}[target.useBtrfsInodeCommand],
		)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runOnce := func() error {
		metrics := make(map[string]uint64)

		if cfg.includeCPU {
			current, err := readCPUSnapshot()
			if err != nil {
				return fmt.Errorf("read cpu snapshot: %w", err)
			}
			cpu, err := cpuBreakdownPercent(prevCPU, current)
			if err != nil {
				return fmt.Errorf("compute cpu breakdown: %w", err)
			}
			metrics[cpuMetricKey("user")] = percentToUint64(cpu.user)
			metrics[cpuMetricKey("system")] = percentToUint64(cpu.system)
			metrics[cpuMetricKey("iowait")] = percentToUint64(cpu.iowait)
			metrics[cpuMetricKey("steal")] = percentToUint64(cpu.steal)
			prevCPU = current
		}

		if cfg.includeRAM {
			ram, err := readRAMBreakdownPercent()
			if err != nil {
				return fmt.Errorf("read ram usage: %w", err)
			}
			metrics[ramMetricKey("used")] = percentToUint64(ram.used)
			metrics[ramMetricKey("free")] = percentToUint64(ram.free)
			metrics[ramMetricKey("shared")] = percentToUint64(ram.shared)
			metrics[ramMetricKey("buff")] = percentToUint64(ram.buff)
		}
		if cfg.includeNet {
			currentNet, err := readNetSnapshot()
			if err != nil {
				return fmt.Errorf("read net snapshot: %w", err)
			}
			for iface, cur := range currentNet {
				prev, ok := prevNet[iface]
				if !ok {
					continue
				}
				bytesDelta := counterDelta(cur.bytes, prev.bytes)
				packetsDelta := counterDelta(cur.packets, prev.packets)
				metrics[netBytesMetricKey(iface)] = bytesDelta
				metrics[netPacketsMetricKey(iface)] = packetsDelta
			}
			prevNet = currentNet
		}

		for _, target := range diskTargets {
			usedPercent, err := readDiskUsedPercent(target.path)
			if err != nil {
				log.Printf("disk metric failed for %q: %v", target.path, err)
				continue
			}
			metrics[diskMetricKey(target.path)] = percentToUint64(usedPercent)

			inodePercent, err := readInodeUsagePercent(target, btrfsBinary)
			if err != nil {
				log.Printf("inode metric failed for %q: %v", target.path, err)
				continue
			}
			metrics[inodeMetricKey(target.path)] = percentToUint64(inodePercent)
		}
		if prevDisk != nil {
			currentDisk, err := readDiskSnapshot()
			if err != nil {
				log.Printf("disk iops/throughput read failed: %v", err)
			} else {
				now := time.Now()
				elapsed := now.Sub(prevDiskAt).Seconds()
				if elapsed >= 1 {
					for _, target := range diskTargets {
						iops, throughput, ioErr := diskRatesForTarget(prevDisk, currentDisk, target, elapsed)
						if ioErr != nil {
							continue
						}
						metrics[iopsMetricKey(target.path)] = iops
						metrics[throughputMetricKey(target.path)] = throughput
					}
				}
				prevDisk = currentDisk
				prevDiskAt = now
			}
		}
		if hasLVS {
			lvmMetrics, err := readLVMThinUsage()
			if err != nil {
				log.Printf("lvm thin metrics failed: %v", err)
			} else {
				for k, v := range lvmMetrics {
					metrics[k] = v
				}
			}
		}

		if len(metrics) == 0 {
			return errors.New("no metrics collected")
		}

		payload := pushPayload{
			AgentVersion: 1,
			Timestamp:    time.Now().Unix(),
			Metrics:      metrics,
		}
		return pushMetrics(ctx, httpClient, cfg, payload)
	}

	if cfg.oneshot {
		if err := runOnce(); err != nil {
			log.Fatalf("push failed: %v", err)
		}
		log.Printf("metrics pushed (oneshot)")
		return
	}

	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	log.Printf("starting monitoring agent; interval=%s push_url=%s disks=%s",
		cfg.interval, cfg.pushURL, strings.Join(cfg.diskPaths, ","))

	if err := runOnce(); err != nil {
		log.Printf("initial push failed: %v", err)
	}

	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down")
			return
		case <-ticker.C:
			if err := runOnce(); err != nil {
				log.Printf("push failed: %v", err)
			}
		}
	}
}

func loadConfig() (config, error) {
	var cfg config
	var diskCSV string
	var intervalSeconds int
	var timeoutSeconds int
	flag.StringVar(&cfg.monitorID, "id", envOrDefault("NQ_MONITOR_ID", ""), "Monitor UUID")
	flag.IntVar(&intervalSeconds, "interval", envIntOrDefault("NQ_INTERVAL_SECONDS", 60), "Collection interval in seconds")
	flag.IntVar(&timeoutSeconds, "timeout", envIntOrDefault("NQ_TIMEOUT_SECONDS", 10), "HTTP timeout in seconds")
	flag.StringVar(&diskCSV, "disk-paths", envOrDefault("NQ_DISK_PATHS", "/,/tmp"), "Comma-separated disk paths, e.g. /,/tmp,/var")
	flag.StringVar(&cfg.location, "location", envOrDefault("NQ_LOCATION", "agent"), "Location label sent as x-monitor-location")
	flag.BoolVar(&cfg.oneshot, "oneshot", envBoolOrDefault("NQ_ONESHOT", false), "Collect and push once, then exit")
	flag.BoolVar(&cfg.install, "install", false, "Install binary + systemd service and exit (Linux)")
	flag.StringVar(&cfg.serviceName, "service-name", envOrDefault("NQ_SERVICE_NAME", "monitoring-agent"), "Systemd service name")
	flag.StringVar(&cfg.installUser, "install-user", envOrDefault("NQ_INSTALL_USER", "root"), "User for systemd service")
	flag.StringVar(&cfg.binaryPath, "install-binary-path", envOrDefault("NQ_INSTALL_BINARY_PATH", ""), "Install path for agent binary")
	flag.StringVar(&cfg.envFilePath, "install-env-path", envOrDefault("NQ_INSTALL_ENV_PATH", ""), "Path for environment file")
	flag.StringVar(&cfg.servicePath, "install-service-path", envOrDefault("NQ_INSTALL_SERVICE_PATH", ""), "Path for systemd unit file")
	flag.BoolVar(&cfg.showVersion, "version", false, "Print agent version and exit")
	flag.BoolVar(&cfg.includeRAM, "ram", envBoolOrDefault("NQ_INCLUDE_RAM", true), "Include RAM usage metric")
	flag.BoolVar(&cfg.includeCPU, "cpu", envBoolOrDefault("NQ_INCLUDE_CPU", true), "Include CPU usage metric")
	flag.BoolVar(&cfg.includeNet, "net", envBoolOrDefault("NQ_INCLUDE_NET", true), "Include network byte/packet metrics")
	flag.BoolVar(&cfg.insecureTLS, "insecure-tls", envBoolOrDefault("NQ_INSECURE_TLS", false), "Disable TLS certificate verification")
	flag.Parse()

	if cfg.showVersion {
		return cfg, nil
	}

	if cfg.install && !intervalWasExplicitlyProvided() {
		return cfg, errors.New("interval is required with -install (use -interval <seconds> or set NQ_INTERVAL_SECONDS)")
	}

	cfg.monitorID = strings.TrimSpace(cfg.monitorID)
	if cfg.monitorID == "" {
		return cfg, errors.New("id is required (or set NQ_MONITOR_ID)")
	}
	cfg.pushBaseURL = envOrDefault("NQ_PUSH_BASE_URL", "https://push.netquirk.com")
	pushURL, err := normalizePushURL(cfg.monitorID, cfg.pushBaseURL)
	if err != nil {
		return cfg, err
	}
	cfg.pushURL = pushURL
	if strings.TrimSpace(cfg.serviceName) == "" {
		return cfg, errors.New("service-name must not be empty")
	}
	if strings.TrimSpace(cfg.binaryPath) == "" {
		cfg.binaryPath = "/usr/local/bin/" + cfg.serviceName
	}
	if strings.TrimSpace(cfg.envFilePath) == "" {
		cfg.envFilePath = "/etc/default/" + cfg.serviceName
	}
	if strings.TrimSpace(cfg.servicePath) == "" {
		cfg.servicePath = "/etc/systemd/system/" + cfg.serviceName + ".service"
	}
	if intervalSeconds < 5 {
		return cfg, errors.New("interval must be >= 5 seconds")
	}
	if timeoutSeconds < 1 {
		return cfg, errors.New("timeout must be >= 1 second")
	}

	cfg.interval = time.Duration(intervalSeconds) * time.Second
	cfg.timeout = time.Duration(timeoutSeconds) * time.Second
	cfg.diskPaths = parseDiskPaths(diskCSV)
	if len(cfg.diskPaths) == 0 {
		cfg.diskPaths = []string{"/"}
	}

	return cfg, nil
}

func intervalWasExplicitlyProvided() bool {
	if _, ok := os.LookupEnv("NQ_INTERVAL_SECONDS"); ok {
		return true
	}
	for _, arg := range os.Args[1:] {
		if arg == "-interval" || strings.HasPrefix(arg, "-interval=") {
			return true
		}
		if arg == "--interval" || strings.HasPrefix(arg, "--interval=") {
			return true
		}
	}
	return false
}

func normalizePushURL(monitorID, base string) (string, error) {
	id := strings.TrimSpace(strings.TrimPrefix(monitorID, "/"))
	if id == "" {
		return "", errors.New("id must not be empty")
	}
	if strings.Contains(id, "/") {
		return "", errors.New("id must not contain '/'")
	}

	base = strings.TrimSpace(base)
	if base == "" {
		return "", errors.New("push base URL must not be empty")
	}
	if !strings.HasPrefix(base, "https://") && !strings.HasPrefix(base, "http://") {
		return "", errors.New("push base URL must start with http:// or https://")
	}

	return strings.TrimRight(base, "/") + "/" + id, nil
}

func parseDiskPaths(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{})

	for _, raw := range parts {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	return out
}

func diskMetricKey(path string) string {
	return "disk:" + path
}

func inodeMetricKey(path string) string {
	return "inode:" + path
}

func iopsMetricKey(path string) string {
	return "iops:" + path
}

func throughputMetricKey(path string) string {
	return "throughput:" + path
}

func lvmThinDataMetricKey(vg, lv string) string {
	return "lvm:data:" + vg + "/" + lv
}

func lvmThinMetaMetricKey(vg, lv string) string {
	return "lvm:meta:" + vg + "/" + lv
}

func netBytesMetricKey(iface string) string {
	return "net:" + iface + ":bytes"
}

func netPacketsMetricKey(iface string) string {
	return "net:" + iface + ":packets"
}

func cpuMetricKey(part string) string {
	return "cpu:" + part
}

func ramMetricKey(part string) string {
	return "ram:" + part
}

func counterDelta(current, previous uint64) uint64 {
	if current >= previous {
		return current - previous
	}
	return current
}

func pushMetrics(ctx context.Context, client *http.Client, cfg config, payload pushPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-monitor-location", cfg.location)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post push metrics: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	log.Printf("push ok status=%d metrics=%d", resp.StatusCode, len(payload.Metrics))
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func envBoolOrDefault(key string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func round2(v float64) float64 {
	return float64(int(v*100+0.5)) / 100
}

func percentToUint64(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	if v >= 100 {
		return 100
	}
	return uint64(v + 0.5)
}
