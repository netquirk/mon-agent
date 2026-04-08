# NetQuirk Monitoring Agent (Go)

Lightweight open source system agent that collects local host metrics and pushes them to an `agent` monitor endpoint.

## Security Policy

Automatic updates are intentionally disabled for security reasons. See [SECURITY_POLICY.md](SECURITY_POLICY.md).

## Metrics Sent

- `pack4_cpu` - Packed CPU lanes `[user, system, iowait, steal]` (each lane is scaled percent x100)
- `pack4_ram` - Packed RAM lanes `[used, free, shared, buff/cache]` (each lane is scaled percent x100)
- `pack2_disk_{path}` - Packed disk/inode usage lanes `[disk_used_percent, inode_used_percent]`
- `vec_disk_{path}` - 4-lane disk IO vector `[read_bps, write_bps, read_iops, write_iops]`
- `pack2_lvm_{vg}/{lv}` - Packed LVM thin usage lanes `[data_percent, meta_percent]`
- `vec_net_{iface}` - 4-lane net vector `[rx_bytes, tx_bytes, rx_packets, tx_packets]`

## Payload Format

The agent sends:

```json
{
  "agent_version": 1,
  "ts": 1775340000,
  "metrics": {
    "pack4_cpu": 1125917086976090,
    "pack4_ram": 13258617121480734,
    "pack2_disk_/": [44, 71],
    "pack2_disk_/tmp": [8, 3],
    "vec_disk_/": [98304, 98304, 12, 11],
    "vec_net_eth0": [12488, 9312, 83, 62]
  }
}
```

All scalar metric values are emitted as `uint64` integers. CPU/RAM lanes are packed into a single `uint64` per group and stored as scaled percent (`percent x100`) per lane. Net interface metrics use a 4-lane integer vector payload.

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
