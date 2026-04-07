# Security & Update Policy

## Automatic Updates Are Intentionally Avoided

For security reasons, `mon-agent` is intentionally designed without self-update behavior and without any automatic update mechanism.

- No background updater
- No remote code download
- No auto-install of new versions
- No hot-patching behavior

All upgrades must be performed manually by an operator using a trusted release artifact and an explicit deployment step.

## Scope of Function

`mon-agent` is designed to function only as a metrics sender:

- collect local system telemetry
- send metrics payloads to the configured NetQuirk push endpoint

The agent does not provide remote command execution or general-purpose control-plane behavior.
