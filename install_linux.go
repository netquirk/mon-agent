//go:build linux

package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

//go:embed systemd/mon-agent.service
var bundledServiceUnit string

func installSystemdService(cfg config) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("install requires root (try sudo)")
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return fmt.Errorf("resolve executable symlink: %w", err)
	}

	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("systemctl not found: %w", err)
	}

	// Best effort: stop existing service before replacing binary.
	_ = runCommand("systemctl", "stop", cfg.serviceName+".service")

	if err := copyFile(exe, cfg.binaryPath, 0o755); err != nil {
		return fmt.Errorf("install binary: %w", err)
	}

	envContent := buildEnvFile(cfg)
	if err := writeFileWithPerms(cfg.envFilePath, []byte(envContent), 0o644); err != nil {
		return fmt.Errorf("write env file: %w", err)
	}

	unit := strings.ReplaceAll(bundledServiceUnit, "{{BINARY_PATH}}", cfg.binaryPath)
	unit = strings.ReplaceAll(unit, "{{ENV_FILE}}", cfg.envFilePath)
	unit = strings.ReplaceAll(unit, "{{USER}}", cfg.installUser)
	unit = strings.ReplaceAll(unit, "{{SERVICE_NAME}}", cfg.serviceName)
	if err := writeFileWithPerms(cfg.servicePath, []byte(unit), 0o644); err != nil {
		return fmt.Errorf("write systemd unit: %w", err)
	}

	if err := runCommand("systemctl", "daemon-reload"); err != nil {
		return err
	}
	if err := runCommand("systemctl", "enable", "--now", cfg.serviceName+".service"); err != nil {
		return err
	}

	return nil
}

func buildEnvFile(cfg config) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "NQ_MONITOR_ID=%s\n", shellQuote(cfg.monitorID))
	fmt.Fprintf(&b, "NQ_PUSH_BASE_URL=%s\n", shellQuote(cfg.pushBaseURL))
	fmt.Fprintf(&b, "NQ_INTERVAL_SECONDS=%d\n", int(cfg.interval/time.Second))
	fmt.Fprintf(&b, "NQ_TIMEOUT_SECONDS=%d\n", int(cfg.timeout/time.Second))
	fmt.Fprintf(&b, "NQ_DISK_PATHS=%s\n", shellQuote(strings.Join(cfg.diskPaths, ",")))
	fmt.Fprintf(&b, "NQ_LOCATION=%s\n", shellQuote(cfg.location))
	fmt.Fprintf(&b, "NQ_INCLUDE_CPU=%t\n", cfg.includeCPU)
	fmt.Fprintf(&b, "NQ_INCLUDE_RAM=%t\n", cfg.includeRAM)
	fmt.Fprintf(&b, "NQ_INCLUDE_NET=%t\n", cfg.includeNet)
	fmt.Fprintf(&b, "NQ_INSECURE_TLS=%t\n", cfg.insecureTLS)
	return b.String()
}

func shellQuote(v string) string {
	v = strings.ReplaceAll(v, `'`, `'\''`)
	return "'" + v + "'"
}

func writeFileWithPerms(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		return err
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".mon-agent-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	return nil
}

func runCommand(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %w (%s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
