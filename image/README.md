# KelviOS Image Builder

Builds a bootable Debian-based disk image with KelviOS core pre-installed.
**Layout A/B (FR-5.8):** EFI (512M) + root-A (2560M) + root-B (2560M) + data (remainder, default 8GB total).

## Prerequisites

### Host (for running guest build + boot test)
- QEMU/KVM with OVMF firmware (`/usr/share/OVMF/OVMF_CODE_4M.fd`, `/usr/share/OVMF/OVMF_VARS_4M.fd`)
- `/dev/kvm` accessible to user
- Sufficient disk space for guest qcow2 (sparse, ~24GB virtual) + output image
- Go toolchain at `~/tools/go/bin/go` (for building binary on host)

### Guest (for image build — runs inside KVM)
- Root access (sudo without password)
- Debian/Ubuntu with:
  - `mmdebstrap`
  - `grub-pc`, `grub-efi-amd64-bin`
  - `gdisk` (for `sgdisk`)
  - `dosfstools` (for `mkfs.fat`)
  - `e2fsprogs` (for `mkfs.ext4`)
  - `systemd-container` (for `systemd-nspawn` verification)
  - `rsync`
  - `grub-common` (for `grub-editenv`)

## Configuration (Environment Variables)

| Variable | Default | Description |
|----------|---------|-------------|
| `KELVIOS_REPO_ROOT` | Auto-detected from script location | Repository root path |
| `KELVIOS_IMAGE_SIZE_GB` | `8` | Total image size in GiB |
| `KELVIOS_EFI_SIZE_MB` | `512` | EFI partition size in MiB |
| `KELVIOS_ROOT_SIZE_MB` | `2560` | Each root partition (A & B) size in MiB |
| `KELVIOS_DATA_SIZE_MB` | `0` (remainder) | Data partition size in MiB; 0 = use all remaining space |
| `DEBIAN_RELEASE` | `trixie` | Debian release to bootstrap |
| `DEBIAN_MIRROR` | `http://deb.debian.org/debian` | Debian mirror URL |

## Quick Start (Guest Build + Host Boot Test)

### 1. Build the core binary (on host)
```bash
cd <path-to-your-kelvios-checkout>
make build
```

### 2. Create guest VM for image build
```bash
# On host (laptop Kubuntu)
qemu-img create -f qcow2 guest-build.qcow2 24G
# Download Debian trixie cloud image (verify checksum!)
wget https://cloud.debian.org/images/cloud/trixie/latest/debian-13-generic-amd64.qcow2 -O debian-trixie-base.qcow2
# Boot guest with cloud image + build disk
qemu-system-x86_64 -enable-kvm -m 4G -smp 2 \
  -drive file=debian-trixie-base.qcow2,format=qcow2,if=virtio \
  -drive file=guest-build.qcow2,format=qcow2,if=virtio \
  -nographic -serial mon:stdio
# Inside guest: setup, copy repo, run build-image.sh as root
```

### 3. Build image inside guest (as root)
```bash
# Inside guest, after repo is copied:
cd /path/to/repo
sudo KELVIOS_IMAGE_SIZE_GB=8 ./image/build-image.sh
# Output: image/output/kelvios-trixie-amd64.img
```

### 4. Transfer image to host (preserving sparseness)
```bash
# From guest to host (via scp/rsync over virtio-net or shared folder)
rsync --sparse -av image/output/kelvios-trixie-amd64.img host:/path/to/ws61/
```

### 5. Boot test on host (QEMU + OVMF)
```bash
# Copy OVMF_VARS for this run
cp /usr/share/OVMF/OVMF_VARS_4M.fd OVMF_VARS_4M.ws61.fd

# Boot slot A (default)
qemu-system-x86_64 -enable-kvm -m 2G -smp 2 \
  -drive file=/path/to/kelvios-trixie-amd64.img,format=raw,if=virtio \
  -drive file=/usr/share/OVMF/OVMF_CODE_4M.fd,if=pflash,format=raw,readonly=on \
  -drive file=OVMF_VARS_4M.ws61.fd,if=pflash,format=raw \
  -netdev user,id=net0,hostfwd=tcp::18080-:8080 -device virtio-net-pci,netdev=net0 \
  -nographic -serial file:boot-slot-a.log -pidfile qemu-slot-a.pid

# Test /healthz from host
curl http://localhost:18080/healthz
# Expected: {"status":"ok","version":"...","uptime_seconds":...}
```

## Image Layout (A/B Partitions)

| Partition | PARTLABEL | Size | FS | Mount Point |
|-----------|-----------|------|----|-------------|
| p1 | `kelvios-efi` | 512 MiB | FAT32 | `/boot/efi` |
| p2 | `kelvios-root-a` | 2560 MiB | ext4 | `/` (slot A) |
| p3 | `kelvios-root-b` | 2560 MiB | ext4 | `/` (slot B) |
| p4 | `kelvios-data` | remainder | ext4 | `/var/lib/kelvios` |

Both slots contain identical rootfs at build time. Slot A is default active (`kelvios_active=a`).

## GRUB Fallback + Boot-Good Marking (FR-5.8)

- **grubenv variables** (on EFI partition):
  - `kelvios_active` = `a` or `b` (default boot slot)
  - `kelvios_boot_ok` = `1` if last boot marked good, `0` otherwise
- **Boot menu**: GRUB shows entries for both slots; default follows `kelvios_active`
- **Fallback logic**: If `kelvios_boot_ok != 1` at GRUB start, boot the other slot
- **Boot-good marker**: `kelvios-boot-good.service` runs after `kelvios-core.service`,
  mounts EFI, writes `kelvios_boot_ok=1` + `kelvios_active=<current-slot>` via `grub-editenv`
- **Recovery entry**: The non-active slot entry in GRUB menu = manual recovery (FR-5.8)

## Verification

### Level 1: Partition table check
```bash
sgdisk -p image/output/kelvios-trixie-amd64.img
# Should show 4 partitions with correct PARTLABELs and sizes
```

### Level 2: Content check (read-only loop mount)
```bash
losetup --find --show --partscan --read-only image/output/kelvios-trixie-amd64.img
# Assume /dev/loopX
mount -o ro /dev/loopXp2 /mnt  # slot A
ls -l /mnt/usr/local/bin/kelvios-core
cat /mnt/etc/fstab  # should reference kelvios-root-a
umount /mnt
mount -o ro /dev/loopXp3 /mnt  # slot B
cat /mnt/etc/fstab  # should reference kelvios-root-b
umount /mnt
mount -o ro /dev/loopXp1 /mnt  # EFI
grub-editenv /mnt/grub/grubenv list
# Should show: kelvios_active=a, kelvios_boot_ok=0
umount /mnt
losetup -d /dev/loopX
```

### Level 3: Boot test (QEMU + OVMF)
See "Quick Start" section above. Required evidence:
1. Slot A boots cleanly, `/healthz` returns 200 OK
2. Slot B boots after `kelvios_active=b`, `/healthz` returns 200 OK
3. Fallback: boot with `kelvios_boot_ok=0` → GRUB boots other slot
4. Idle RSS measurement (10 samples over 10 min)
5. Disk hot-unplug / cold-unplug test

## Known TODOs

- [ ] **Root password**: Currently locked for PoC. Need a proper first-boot flow (cloud-init / ignition / custom initramfs hook) to set a random password or inject SSH keys.
- [ ] **First-run setup**: No mechanism for initial user creation, SSH key injection, or disk expansion to fill the device.
- [ ] **Secure Boot**: GRUB not signed; UEFI Secure Boot will fail. Need shim + MOK enrollment flow.
- [ ] **Image compression**: Distribute as `.img.xz` or `.img.zst` for smaller downloads.
- [ ] **Checksums**: Publish SHA256SUMS and GPG signatures.
- [ ] **Versioning**: Image version should derive from git tag / build metadata, not hardcoded.
- [ ] **ARM64 support**: Add `grub-efi-arm64-bin` and `linux-image-arm64` for Raspberry Pi / ARM SBCs.

## Build Log

The script logs each stage with timestamps. For long runs, tail the output:

```bash
tail -f image/build.log  # if run via nohup
```

## Troubleshooting

| Symptom | Likely Cause | Fix |
|---------|--------------|-----|
| `mmdebstrap` fails | Network / mirror | Check connectivity, try `--mirror=http://ftp.debian.org/debian` |
| `grub-install` fails (UEFI) | No EFI vars in container | Run on bare metal or VM with EFI vars; `--no-nvram` may help |
| `systemd-nspawn` fails | Kernel too old / no cgroup v2 | Use host with systemd 250+, cgroup v2 unified |
| Image won't boot | Partition labels mismatch fstab | Verify `PARTLABEL=kelvios-root-a/b` and `kelvios-efi` in `sgdisk -p` |
| `/healthz` not reachable | Network not configured in image | Ensure `systemd-networkd` with DHCP is enabled (added in WS61) |

## Architecture Notes

- **Why Debian trixie?** Stable, reproducible, long-term support. Pinned release avoids drift.
- **Why minbase?** Smallest base (~200MB). We add only what we need.
- **Why static Go binary?** No libc dependency, trivial to drop into any rootfs.
- **Why 8GB sparse?** Fits on 8GB+ devices, leaves room for data partition expansion.
- **Why GRUB both BIOS+UEFI?** Maximum hardware compatibility for NAS boxes.
- **Why A/B partitions?** FR-5.8 requires two OS partitions for atomic updates with fallback.
- **Why data partition?** DB and state must survive slot switches (`/var/lib/kelvios`).

## License

Part of KelviOS — see root `LICENSE`.