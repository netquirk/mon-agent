package main

import (
	"bufio"
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
	"path/filepath"
	"sort"
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
	queueDir    string
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
	AgentVersion uint64                     `json:"agent_version"`
	Timestamp    int64                      `json:"ts"`
	Metrics      map[string]json.RawMessage `json:"metrics"`
	IngestMode   string                     `json:"ingest_mode,omitempty"`
}

type pushBatchRequest struct {
	Batch []pushPayload `json:"batch"`
}

type queueCursor struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

type pushResponse struct {
	Success bool `json:"success"`
}

var version = "dev"

const (
	packedCPUKey = "pack4_cpu"
	packedRAMKey = "pack4_ram"
)

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
			Proxy:               http.ProxyFromEnvironment,
			ForceAttemptHTTP2:   true,
			DisableKeepAlives:   false,
			DisableCompression:  true,
			MaxIdleConns:        1,
			MaxIdleConnsPerHost: 1,
			MaxConnsPerHost:     1,
			IdleConnTimeout:     5 * time.Minute,
		},
	}

	encodedAgentVersion, err := encodeSemverVersion(version)
	if err != nil {
		log.Fatalf("invalid agent version %q: %v", version, err)
	}
	if cfg.insecureTLS {
		transport := httpClient.Transport.(*http.Transport).Clone()
		transport.TLSClientConfig = newInsecureTLSConfig()
		httpClient.Transport = transport
	}

	if err := os.MkdirAll(cfg.queueDir, 0o755); err != nil {
		log.Fatalf("failed to initialize queue dir %q: %v", cfg.queueDir, err)
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
		runStartedAt := time.Now()
		metrics := make(map[string]json.RawMessage)

		if cfg.includeCPU {
			current, err := readCPUSnapshot()
			if err != nil {
				return fmt.Errorf("read cpu snapshot: %w", err)
			}
			cpu, err := cpuBreakdownPercent(prevCPU, current)
			if err != nil {
				return fmt.Errorf("compute cpu breakdown: %w", err)
			}
			addUint64Metric(metrics, packedCPUKey, packU16x4(
				percentToScaled100Uint64(cpu.user),
				percentToScaled100Uint64(cpu.system),
				percentToScaled100Uint64(cpu.iowait),
				percentToScaled100Uint64(cpu.steal),
			))
			prevCPU = current
		}

		if cfg.includeRAM {
			ram, err := readRAMBreakdownPercent()
			if err != nil {
				return fmt.Errorf("read ram usage: %w", err)
			}
			addUint64Metric(metrics, packedRAMKey, packU16x4(
				percentToScaled100Uint64(ram.used),
				percentToScaled100Uint64(ram.free),
				percentToScaled100Uint64(ram.shared),
				percentToScaled100Uint64(ram.buff),
			))
		}
		if loadAvgScaled, err := readLoadAverageScaled(); err == nil {
			addUint64Metric(metrics, "loadavg", loadAvgScaled)
		} else {
			log.Printf("loadavg metric failed: %v", err)
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
				addUint64ArrayMetric(metrics, netVecMetricKey(iface), []uint64{
					counterDelta(cur.rxBytes, prev.rxBytes),
					counterDelta(cur.txBytes, prev.txBytes),
					counterDelta(cur.rxPackets, prev.rxPackets),
					counterDelta(cur.txPackets, prev.txPackets),
				})
			}
			prevNet = currentNet
		}

		for _, target := range diskTargets {
			usedPercent, err := readDiskUsedPercent(target.path)
			if err != nil {
				log.Printf("disk metric failed for %q: %v", target.path, err)
				continue
			}

			inodePercent, err := readInodeUsagePercent(target, btrfsBinary)
			if err != nil {
				log.Printf("inode metric failed for %q: %v", target.path, err)
				continue
			}
			addUint64ArrayMetric(metrics, diskInodePackedMetricKey(target.path), []uint64{
				percentToUint64(usedPercent),
				percentToUint64(inodePercent),
			})
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
						readIOPS, writeIOPS, throughputRead, throughputWrite, ioErr := diskRatesForTarget(prevDisk, currentDisk, target, elapsed)
						if ioErr != nil {
							continue
						}
						addUint64ArrayMetric(metrics, diskVecMetricKey(target.path), []uint64{
							throughputRead,
							throughputWrite,
							readIOPS,
							writeIOPS,
						})
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
					addUint64Metric(metrics, k, v)
				}
			}
		}

		if len(metrics) == 0 {
			return errors.New("no metrics collected")
		}

		processingMs := time.Since(runStartedAt).Milliseconds()
		if processingMs < 1 {
			processingMs = 1
		}
		addUint64Metric(metrics, "time_ms", uint64(processingMs))

		payload := pushPayload{
			AgentVersion: encodedAgentVersion,
			Timestamp:    time.Now().Unix(),
			Metrics:      metrics,
		}
		if err := queueAppend(cfg.queueDir, payload); err != nil {
			return fmt.Errorf("queue append: %w", err)
		}
		if err := queueCleanup(cfg.queueDir, time.Now().UTC()); err != nil {
			log.Printf("queue cleanup failed: %v", err)
		}
		if err := flushQueue(ctx, httpClient, cfg); err != nil {
			return fmt.Errorf("flush queue: %w", err)
		}
		return nil
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
	flag.StringVar(&diskCSV, "disk-paths", envOrDefault("NQ_DISK_PATHS", ""), "Comma-separated disk paths, e.g. /,/tmp,/var (default: auto-discover)")
	flag.StringVar(&cfg.location, "location", envOrDefault("NQ_LOCATION", "agent"), "Location label sent as x-monitor-location")
	flag.BoolVar(&cfg.oneshot, "oneshot", envBoolOrDefault("NQ_ONESHOT", false), "Collect and push once, then exit")
	flag.BoolVar(&cfg.install, "install", false, "Install binary + systemd service and exit (Linux)")
	flag.StringVar(&cfg.serviceName, "service-name", envOrDefault("NQ_SERVICE_NAME", "mon-agent"), "Systemd service name")
	flag.StringVar(&cfg.installUser, "install-user", envOrDefault("NQ_INSTALL_USER", "root"), "User for systemd service")
	flag.StringVar(&cfg.binaryPath, "install-binary-path", envOrDefault("NQ_INSTALL_BINARY_PATH", ""), "Install path for agent binary")
	flag.StringVar(&cfg.envFilePath, "install-env-path", envOrDefault("NQ_INSTALL_ENV_PATH", ""), "Path for environment file")
	flag.StringVar(&cfg.servicePath, "install-service-path", envOrDefault("NQ_INSTALL_SERVICE_PATH", ""), "Path for systemd unit file")
	flag.BoolVar(&cfg.showVersion, "version", false, "Print agent version and exit")
	flag.BoolVar(&cfg.includeRAM, "ram", envBoolOrDefault("NQ_INCLUDE_RAM", true), "Include RAM usage metric")
	flag.BoolVar(&cfg.includeCPU, "cpu", envBoolOrDefault("NQ_INCLUDE_CPU", true), "Include CPU usage metric")
	flag.BoolVar(&cfg.includeNet, "net", envBoolOrDefault("NQ_INCLUDE_NET", true), "Include network byte/packet metrics")
	flag.BoolVar(&cfg.insecureTLS, "insecure-tls", envBoolOrDefault("NQ_INSECURE_TLS", false), "Disable TLS certificate verification")
	flag.StringVar(&cfg.queueDir, "queue-dir", envOrDefault("NQ_QUEUE_DIR", "/var/lib/mon-agent/queue"), "Local queue directory for durable metric buffering")
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
		cfg.diskPaths = autoDiscoverDiskPaths()
	}
	if len(cfg.diskPaths) == 0 {
		cfg.diskPaths = []string{"/"}
	}
	cfg.queueDir = strings.TrimSpace(cfg.queueDir)
	if cfg.queueDir == "" {
		return cfg, errors.New("queue-dir must not be empty")
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

func diskVecMetricKey(path string) string {
	return "vec_disk_" + path
}

func lvmPackedMetricKey(vg, lv string) string {
	return "pack2_lvm_" + vg + "/" + lv
}

func diskInodePackedMetricKey(path string) string {
	return "pack2_disk_" + path
}

func netVecMetricKey(iface string) string {
	return "vec_net_" + iface
}

func cpuMetricKey(part string) string {
	return "cpu:" + part
}

func ramMetricKey(part string) string {
	return "ram:" + part
}

func addUint64Metric(metrics map[string]json.RawMessage, key string, value uint64) {
	metrics[key] = json.RawMessage(strconv.AppendUint(make([]byte, 0, 20), value, 10))
}

func addUint64ArrayMetric(metrics map[string]json.RawMessage, key string, values []uint64) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return
	}
	metrics[key] = encoded
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
	var parsed pushResponse
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return fmt.Errorf("push endpoint returned invalid json: %w", err)
		}
		if !parsed.Success {
			return fmt.Errorf("push endpoint returned success=false")
		}
	}

	log.Printf("push ok status=%d metrics=%d", resp.StatusCode, len(payload.Metrics))
	return nil
}

func pushMetricsBatch(ctx context.Context, client *http.Client, cfg config, batch []pushPayload) error {
	if len(batch) == 0 {
		return nil
	}

	body, err := json.Marshal(pushBatchRequest{Batch: batch})
	if err != nil {
		return fmt.Errorf("marshal batch payload: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		strings.TrimRight(cfg.pushURL, "/")+"/batch",
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("build batch request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-monitor-location", cfg.location)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post push batch metrics: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("push batch endpoint returned %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	var parsed pushResponse
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return fmt.Errorf("push batch endpoint returned invalid json: %w", err)
		}
		if !parsed.Success {
			return fmt.Errorf("push batch endpoint returned success=false")
		}
	}

	log.Printf("push batch ok status=%d points=%d", resp.StatusCode, len(batch))
	return nil
}

func flushQueue(ctx context.Context, client *http.Client, cfg config) error {
	const backfillBatchSize = 60

	files, err := queueFiles(cfg.queueDir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}

	cursor, err := readQueueCursor(cfg.queueDir)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, filePath := range files {
		startLine := 0
		if cursor.File == filePath {
			startLine = cursor.Line
		}

		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		line := 0
		batch := make([]pushPayload, 0, backfillBatchSize)
		batchLastLine := -1

		flushBatch := func() error {
			if len(batch) == 0 {
				return nil
			}
			if err := pushMetricsBatch(ctx, client, cfg, batch); err != nil {
				return err
			}
			cursor = queueCursor{File: filePath, Line: batchLastLine + 1}
			if err := writeQueueCursor(cfg.queueDir, cursor); err != nil {
				return err
			}
			batch = batch[:0]
			batchLastLine = -1
			return nil
		}

		for scanner.Scan() {
			if line < startLine {
				line++
				continue
			}

			raw := strings.TrimSpace(scanner.Text())
			if raw == "" {
				if err := flushBatch(); err != nil {
					file.Close()
					return err
				}
				cursor = queueCursor{File: filePath, Line: line + 1}
				if err := writeQueueCursor(cfg.queueDir, cursor); err != nil {
					file.Close()
					return err
				}
				line++
				continue
			}

			var payload pushPayload
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				log.Printf("dropping invalid queued payload line file=%s line=%d err=%v", filepath.Base(filePath), line+1, err)
				if err := flushBatch(); err != nil {
					file.Close()
					return err
				}
				cursor = queueCursor{File: filePath, Line: line + 1}
				if err := writeQueueCursor(cfg.queueDir, cursor); err != nil {
					file.Close()
					return err
				}
				line++
				continue
			}

			mode := strings.ToLower(strings.TrimSpace(payload.IngestMode))
			switch mode {
			case "live", "backfill":
				payload.IngestMode = mode
			default:
				payload.IngestMode = "live"
				if payload.Timestamp > 0 {
					ts := time.Unix(payload.Timestamp, 0).UTC()
					if now.Sub(ts) > (cfg.interval + cfg.interval) {
						payload.IngestMode = "backfill"
					}
				}
			}

			if payload.IngestMode == "backfill" {
				batch = append(batch, payload)
				batchLastLine = line
				if len(batch) >= backfillBatchSize {
					if err := flushBatch(); err != nil {
						file.Close()
						return err
					}
				}
			} else {
				if err := flushBatch(); err != nil {
					file.Close()
					return err
				}
				if err := pushMetrics(ctx, client, cfg, payload); err != nil {
					file.Close()
					return err
				}
				cursor = queueCursor{File: filePath, Line: line + 1}
				if err := writeQueueCursor(cfg.queueDir, cursor); err != nil {
					file.Close()
					return err
				}
			}
			line++
		}
		if err := flushBatch(); err != nil {
			file.Close()
			return err
		}
		scanErr := scanner.Err()
		if closeErr := file.Close(); closeErr != nil && scanErr == nil {
			scanErr = closeErr
		}
		if scanErr != nil {
			return scanErr
		}

		if cursor.File == filePath {
			if err := os.Remove(filePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			cursor = queueCursor{}
			if err := writeQueueCursor(cfg.queueDir, cursor); err != nil {
				return err
			}
		}
	}

	return nil
}

func queueAppend(queueDir string, payload pushPayload) error {
	ts := time.Now().UTC()
	if payload.Timestamp > 0 {
		ts = time.Unix(payload.Timestamp, 0).UTC()
	}
	filePath := filepath.Join(queueDir, ts.Format("2006-01-02")+".ndjson")

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func queueFiles(queueDir string) ([]string, error) {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".ndjson") {
			continue
		}
		out = append(out, filepath.Join(queueDir, name))
	}
	sort.Strings(out)
	return out, nil
}

func queueCleanup(queueDir string, now time.Time) error {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	cutoff := now.AddDate(0, 0, -30)
	cursor, _ := readQueueCursor(queueDir)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".ndjson") {
			continue
		}
		datePart := strings.TrimSuffix(name, ".ndjson")
		d, err := time.Parse("2006-01-02", datePart)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			path := filepath.Join(queueDir, name)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if cursor.File == path {
				cursor = queueCursor{}
				_ = writeQueueCursor(queueDir, cursor)
			}
		}
	}
	return nil
}

func queueCursorPath(queueDir string) string {
	return filepath.Join(queueDir, ".cursor.json")
}

func readQueueCursor(queueDir string) (queueCursor, error) {
	var out queueCursor
	data, err := os.ReadFile(queueCursorPath(queueDir))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return queueCursor{}, err
	}
	return out, nil
}

func writeQueueCursor(queueDir string, cursor queueCursor) error {
	data, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	return os.WriteFile(queueCursorPath(queueDir), data, 0o644)
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

func percentToScaled100Uint64(v float64) uint64 {
	if v <= 0 {
		return 0
	}
	return uint64((v * 100.0) + 0.5)
}

func packU16x4(a, b, c, d uint64) uint64 {
	clamp := func(v uint64) uint64 {
		if v > 0xffff {
			return 0xffff
		}
		return v
	}
	a = clamp(a)
	b = clamp(b)
	c = clamp(c)
	d = clamp(d)
	return a | (b << 16) | (c << 32) | (d << 48)
}

func packU32x2(a, b uint64) uint64 {
	clamp := func(v uint64) uint64 {
		if v > 0xffff_ffff {
			return 0xffff_ffff
		}
		return v
	}
	a = clamp(a)
	b = clamp(b)
	return a | (b << 32)
}

func encodeSemverVersion(raw string) (uint64, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "v")
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, errors.New("expected major.minor.patch")
	}
	parse := func(part string) (uint64, error) {
		if part == "" {
			return 0, errors.New("empty semver component")
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return 0, errors.New("non-numeric semver component")
			}
		}
		return strconv.ParseUint(part, 10, 64)
	}

	const partMask = (1 << 21) - 1
	major, err := parse(parts[0])
	if err != nil {
		return 0, err
	}
	minor, err := parse(parts[1])
	if err != nil {
		return 0, err
	}
	patch, err := parse(parts[2])
	if err != nil {
		return 0, err
	}
	if major == 0 || major > partMask || minor > partMask || patch > partMask {
		return 0, errors.New("semver component out of range for 21-bit encoding")
	}
	return (major << 42) | (minor << 21) | patch, nil
}
