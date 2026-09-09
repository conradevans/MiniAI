# MiniAI Qwen 8B CPU power lease

MiniAI remains an unprivileged service. Immediately before selected Qwen 8B work,
it invokes the fixed root-owned helper through noninteractive sudo. The helper
saves the current CPU policy values in `/run/miniai-power/lease.json`, applies
`energy_performance_preference=power` and `scaling_max_freq=3000000`, and restores
the exact snapshot after inference. Qwen 4B and deterministic responses do not
invoke the helper.

The helper accepts exactly one of `enter`, `restore`, `recover`, or `status`. Its
sysfs root, control names, low-power values, state path, and lock path are
compiled constants. It accepts no caller-provided paths or settings.

## Administrator installation

Run these only during an approved deployment window from `/srv/miniai`:

```bash
set -e

go build -trimpath -o /tmp/miniai-power-helper ./cmd/miniai-power-helper
sudo systemctl stop miniai.service
sudo /usr/local/libexec/miniai-power-helper recover
sudo install -d -o root -g root -m 0755 /usr/local/libexec
sudo install -o root -g root -m 0755 /tmp/miniai-power-helper /usr/local/libexec/miniai-power-helper
sudo visudo -cf /srv/miniai/miniai-power-helper.sudoers
sudo install -o root -g root -m 0440 /srv/miniai/miniai-power-helper.sudoers /etc/sudoers.d/miniai-power-helper
sudo visudo -cf /etc/sudoers.d/miniai-power-helper
sudo install -o root -g root -m 0644 /srv/miniai/miniai-power-tmpfiles.conf /etc/tmpfiles.d/miniai-power.conf
sudo systemd-tmpfiles --create /etc/tmpfiles.d/miniai-power.conf
sudo stat -c '%U %G %a %n' /run/miniai-power
sudo install -o root -g root -m 0644 /srv/miniai/miniai-power-lease.conf /etc/systemd/system/miniai.service.d/power-lease.conf
```

The `stat` output must be exactly:

```text
root root 700 /run/miniai-power
```

Stopping MiniAI before invoking `recover` prevents a new legacy-path lease from
being acquired between recovery and helper replacement. The recovery command
must succeed before installation continues so an active legacy lease cannot be
stranded when the new helper starts using `/run/miniai-power`.

The systemd drop-in is required because the checked-in base unit has
`NoNewPrivileges=true` and an empty capability bounding set; Linux otherwise
prevents the fixed sudo executable from entering the permitted root helper. It
restores only `CAP_SETUID`/`CAP_SETGID` for sudo and carves out only the cpufreq
subtree and `/run/miniai-power` for writes while retaining
`ProtectKernelTunables=true` for other kernel controls and keeping the rest of
`/run` read-only. The remaining unit sandbox and exact-command sudoers allowlist
stay in place.

Remove any `MINIAI_8B_PROFILE` line from the MiniAI systemd override before the
production test. Cool (`num_thread=4`, `num_batch=64`) is the code default.
Then run `systemctl daemon-reload` and restart only as part of the separately
authorized deployment.

## Recovery and status

The following commands emit only fixed status words:

```bash
sudo /usr/local/libexec/miniai-power-helper status
sudo /usr/local/libexec/miniai-power-helper recover
```

MiniAI invokes `recover` before opening its database or accepting requests. A
recovery failure blocks Qwen 8B but leaves deterministic and Qwen 4B behavior
available.

## Removal

Recover an active lease before removing privileges or the helper:

```bash
set -e

sudo systemctl stop miniai.service
sudo /usr/local/libexec/miniai-power-helper recover
sudo rm -f /etc/sudoers.d/miniai-power-helper
sudo rm -f /etc/systemd/system/miniai.service.d/power-lease.conf
sudo rm -f /etc/tmpfiles.d/miniai-power.conf
sudo rm -f /usr/local/libexec/miniai-power-helper
sudo systemctl daemon-reload
```

Restore the prior MiniAI binary before starting the service again. Do not delete
`/run/miniai-power/lease.json` manually: a retained state file means exact CPU
restoration did not complete and must be resolved with `recover`.
