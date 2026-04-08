# NetQuirk Monitoring Agent (Go)

Lightweight open source system agent that collects local host metrics and pushes them to an `agent` monitor endpoint.

## Security Policy

Automatic updates are intentionally disabled for security reasons. See [SECURITY_POLICY.md](SECURITY_POLICY.md).

## Metrics Sent

- `pack4_cpu_v1` - Packed CPU lanes `[user, system, iowait, steal]` (each lane is scaled percent x100)
- `pack4_ram_v1` - Packed RAM lanes `[used, free, shared, buff/cache]` (each lane is scaled percent x100)
- `disk:{path}` - Disk used percent for each configured mount path
- `inode:{path}` - Inode used percent for each configured mount path
- `iops:{path}` - Disk I/O operations per second for each configured mount path
- `throughput:{path}` - Disk throughput (bytes/sec) for each configured mount path
- `pack2_lvm_v1_{vg}/{lv}` - Packed LVM thin usage lanes `[data_percent, meta_percent]` (lane3/4 reserved)
- `net:{iface}:bytes` - Bytes transferred during the interval (rx + tx)
- `net:{iface}:packets` - Packets transferred during the interval (rx + tx)

Examples:

- `disk:/`
- `disk:/tmp`
- `inode:/`
- `inode:/tmp`
- `iops:/`
- `throughput:/tmp`
- `pack2_lvm_v1_vg0/thinpool`
- `net:eth0:bytes`
- `net:eth0:packets`

## Payload Format

The agent sends:

```json
{
  "agent_version": 1,
  "ts": 1775340000,
  "metrics": {
    "pack4_cpu_v1": 1125917086976090,
    "pack4_ram_v1": 13258617121480734,
    "disk:/": 44,
    "disk:/tmp": 8,
    "inode:/": 71,
    "inode:/tmp": 3,
    "iops:/": 23,
    "throughput:/": 196608,
    "net:eth0:bytes": 12488,
    "net:eth0:packets": 83
  }
}
```

All metric values are emitted as `uint64` integers. CPU/RAM lanes are packed into a single `uint64` per group and stored as scaled percent (`percent x100`) per lane.

## Btrfs Inode Handling

At startup the agent detects filesystem type for each monitored disk path.

- Non-Btrfs filesystems: inode count is read via `statfs` (same source as `df -i`).
- Btrfs filesystems: inode count is read via the `btrfs filesystem usage` command, because `df -i`/statfs can be misleading on Btrfs.

## Build

```bash
cd agent
go build -o mon-agent .
```

Print binary version:

```bash
./mon-agent -version
```

## Run

```bash
./mon-agent \
  -id "<monitor_uuid>" \
  -interval 60
```

## Configuration

Flags (or env vars):

- `-id` (`NQ_MONITOR_ID`) preferred; monitor UUID
- `NQ_PUSH_BASE_URL` optional base for `-id` (default `https://push.netquirk.com`)
- `-interval` (`NQ_INTERVAL_SECONDS`) default `60`
- `-timeout` (`NQ_TIMEOUT_SECONDS`) default `10`
- `-disk-paths` (`NQ_DISK_PATHS`) optional. If omitted, disk paths are auto-discovered.
- `-location` (`NQ_LOCATION`) default `"agent"` (sent in `x-monitor-location`)
- `-oneshot` (`NQ_ONESHOT`) run once and exit
- `-cpu` (`NQ_INCLUDE_CPU`) default `true`
- `-ram` (`NQ_INCLUDE_RAM`) default `true`
- `-net` (`NQ_INCLUDE_NET`) default `true`
- `-insecure-tls` (`NQ_INSECURE_TLS`) default `false`
- `-install` installs binary + env file + bundled systemd unit and starts the service (Linux only)
- `-service-name` (`NQ_SERVICE_NAME`) default `mon-agent`
- `-install-user` (`NQ_INSTALL_USER`) default `root`
- `-install-binary-path` (`NQ_INSTALL_BINARY_PATH`) default `/usr/local/bin/<service-name>`
- `-install-env-path` (`NQ_INSTALL_ENV_PATH`) default `/etc/default/<service-name>`
- `-install-service-path` (`NQ_INSTALL_SERVICE_PATH`) default `/etc/systemd/system/<service-name>.service`

## systemd Install (Bundled)

The agent binary embeds a systemd service template:

- `agent/systemd/mon-agent.service`

Install and start it:

```bash
sudo ./mon-agent \
  -install \
  -id "<monitor_uuid>" \
  -interval 60 \
  -disk-paths "/,/tmp,/var" \
  -location "eu-west"
```

Note: `-install` requires an explicit interval (`-interval` or `NQ_INTERVAL_SECONDS`) so installed cadence is intentional.

This will:

- copy the current binary to `/usr/local/bin/mon-agent`
- write environment config to `/etc/default/mon-agent`
- write service unit to `/etc/systemd/system/mon-agent.service`
- run `systemctl daemon-reload`
- run `systemctl enable --now mon-agent.service`

## GitHub Releases

Automated release workflow:

- `.github/workflows/agent-release.yml`

Release notes and process:

- [RELEASING.md](RELEASING.md)
