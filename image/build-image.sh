#!/usr/bin/env bash
# build-image.sh — KelviOS Debian-based image builder
# Idempotent, set -euo pipefail, clear per-stage logging.
# Output: image/output/kelvios-<version>-amd64.img
# Layout A/B: EFI (512M) + root-A (2560M) + root-B (2560M) + data (remainder)

set -euo pipefail

# ─── Configuration ──────────────────────────────────────────────
# REPO_ROOT derived from script location; can be overridden by KELVIOS_REPO_ROOT
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEFAULT_REPO_ROOT="$(dirname "${SCRIPT_DIR}")"
REPO_ROOT="${KELVIOS_REPO_ROOT:-${DEFAULT_REPO_ROOT}}"

IMAGE_DIR="${REPO_ROOT}/image"
WORK_DIR="${IMAGE_DIR}/work"
ROOTFS_DIR="${WORK_DIR}/rootfs"
OUTPUT_DIR="${IMAGE_DIR}/output"

# Image sizing (can be overridden via env)
KELVIOS_IMAGE_SIZE_GB="${KELVIOS_IMAGE_SIZE_GB:-8}"
KELVIOS_EFI_SIZE_MB="${KELVIOS_EFI_SIZE_MB:-512}"
KELVIOS_ROOT_SIZE_MB="${KELVIOS_ROOT_SIZE_MB:-2560}"
KELVIOS_DATA_SIZE_MB="${KELVIOS_DATA_SIZE_MB:-0}"  # 0 = remainder

IMAGE_SIZE_GB="${KELVIOS_IMAGE_SIZE_GB}"
IMAGE_SIZE_BYTES=$((IMAGE_SIZE_GB * 1024 * 1024 * 1024))
EFI_SIZE_MB="${KELVIOS_EFI_SIZE_MB}"
ROOT_SIZE_MB="${KELVIOS_ROOT_SIZE_MB}"
DATA_SIZE_MB="${KELVIOS_DATA_SIZE_MB}"

# Calculate partition sizes in bytes
EFI_SIZE_BYTES=$((EFI_SIZE_MB * 1024 * 1024))
ROOT_SIZE_BYTES=$((ROOT_SIZE_MB * 1024 * 1024))
if [[ ${DATA_SIZE_MB} -gt 0 ]]; then
    DATA_SIZE_BYTES=$((DATA_SIZE_MB * 1024 * 1024))
else
    DATA_SIZE_BYTES=$((IMAGE_SIZE_BYTES - EFI_SIZE_BYTES - 2 * ROOT_SIZE_BYTES))
fi

DEBIAN_RELEASE="${DEBIAN_RELEASE:-trixie}"
DEBIAN_MIRROR="${DEBIAN_MIRROR:-http://deb.debian.org/debian}"

BINARY_PATH="${REPO_ROOT}/bin/kelvios-core"
OUTPUT_IMG="${OUTPUT_DIR}/kelvios-${DEBIAN_RELEASE}-amd64.img"

# ─── Logging helpers ────────────────────────────────────────────
log() { echo "[$(date '+%H:%M:%S')] $*"; }
stage() { log "=== STAGE: $* ==="; }
warn() { log "WARN: $*"; }
die() { log "FATAL: $*"; exit 1; }

# ─── Print configuration ────────────────────────────────────────
stage "Configuration"
log "REPO_ROOT: ${REPO_ROOT}"
log "IMAGE_SIZE_GB: ${IMAGE_SIZE_GB} (EFI: ${EFI_SIZE_MB}MiB, root-A: ${ROOT_SIZE_MB}MiB, root-B: ${ROOT_SIZE_MB}MiB, data: ${DATA_SIZE_MB}MiB)"
log "DEBIAN_RELEASE: ${DEBIAN_RELEASE}"
log "DEBIAN_MIRROR: ${DEBIAN_MIRROR}"
log "BINARY_PATH: ${BINARY_PATH}"
log "OUTPUT_IMG: ${OUTPUT_IMG}"

# ─── Pre-flight checks ──────────────────────────────────────────
stage "Pre-flight checks"
[[ -f "${BINARY_PATH}" ]] || die "Binary not found at ${BINARY_PATH}. Run 'make build' first."
[[ $EUID -eq 0 ]] || die "Must run as root (sudo)."
command -v mmdebstrap >/dev/null || die "mmdebstrap not installed."
command -v grub-install >/dev/null || die "grub-install not installed."
command -v sgdisk >/dev/null || die "sgdisk (gdisk) not installed."
command -v mkfs.fat >/dev/null || die "mkfs.fat (dosfstools) not installed."
command -v mkfs.ext4 >/dev/null || die "mkfs.ext4 (e2fsprogs) not installed."
command -v grub-editenv >/dev/null || die "grub-editenv not installed."
command -v systemd-nspawn >/dev/null || warn "systemd-nspawn not available; verification will be limited."

log "Binary: ${BINARY_PATH} ($(stat -c%s "${BINARY_PATH}") bytes)"
log "Work dir: ${WORK_DIR}"
log "Output: ${OUTPUT_IMG}"

# ─── Cleanup stale mounts from previous runs ──────────────────────
stage "Cleanup stale mounts"
# Unmount any mounts under WORK_DIR (reverse order: deepest first)
if [[ -d "${WORK_DIR}" ]]; then
    # Find and unmount all mounts under WORK_DIR
    awk -v wd="${WORK_DIR}" '$2 ~ "^" wd {print $2}' /proc/mounts | sort -r | while read -r mnt; do
        umount "${mnt}" 2>/dev/null || umount -l "${mnt}" 2>/dev/null || true
    done
    # Detach any loop devices associated with output image
    losetup -j "${OUTPUT_IMG}" 2>/dev/null | cut -d: -f1 | while read -r loop; do
        losetup -d "${loop}" 2>/dev/null || true
    done
fi

# ─── Clean previous work dir (idempotent) ───────────────────────
stage "Clean work directory"
rm -rf "${WORK_DIR}"
mkdir -p "${ROOTFS_DIR}" "${OUTPUT_DIR}"

# ─── Stage 1: mmdebstrap minimal rootfs ─────────────────────────
stage "Mmdebstrap ${DEBIAN_RELEASE} (minbase + packages)"
PACKAGES="systemd,systemd-sysv,linux-image-amd64,grub-efi-amd64-bin,grub2-common,openssh-server,ca-certificates,curl"
mmdebstrap --variant=minbase --include="${PACKAGES}" "${DEBIAN_RELEASE}" "${ROOTFS_DIR}" "${DEBIAN_MIRROR}"
log "Mmdebstrap complete."

# ─── Stage 2: Install kelvios-core binary ───────────────────────
stage "Install kelvios-core binary"
install -Dm755 "${BINARY_PATH}" "${ROOTFS_DIR}/usr/local/bin/kelvios-core"
log "Binary installed to /usr/local/bin/kelvios-core"

# ─── Stage 3: Write config.toml ─────────────────────────────────
stage "Write /etc/kelvios/config.toml"
mkdir -p "${ROOTFS_DIR}/etc/kelvios"
cat > "${ROOTFS_DIR}/etc/kelvios/config.toml" <<'EOF'
[http]
addr = ":8080"
read_timeout = "10s"
write_timeout = "10s"
idle_timeout = "60s"

[database]
path = "/var/lib/kelvios/catalog.db"
EOF
mkdir -p "${ROOTFS_DIR}/var/lib/kelvios"
log "Config written."

# ─── Stage 4: systemd unit ──────────────────────────────────────
stage "Install systemd unit"
mkdir -p "${ROOTFS_DIR}/etc/systemd/system"
cat > "${ROOTFS_DIR}/etc/systemd/system/kelvios-core.service" <<'EOF'
[Unit]
Description=KelviOS Core API
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/kelvios-core
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

# Security hardening
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/kelvios
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF

# Enable via chroot
chroot "${ROOTFS_DIR}" systemctl enable kelvios-core.service
log "Systemd unit installed and enabled."

# ─── Stage 5: boot-good marker service (FR-5.8) ─────────────────
stage "Install boot-good marker service"
cat > "${ROOTFS_DIR}/etc/systemd/system/kelvios-boot-good.service" <<'EOF'
[Unit]
Description=KelviOS Boot Good Marker (FR-5.8)
After=kelvios-core.service
Requires=kelvios-core.service

[Service]
Type=oneshot
ExecStart=/usr/local/bin/kelvios-boot-good
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
EOF

# Create the boot-good marker script
cat > "${ROOTFS_DIR}/usr/local/bin/kelvios-boot-good" <<'EOF'
#!/usr/bin/env bash
# kelvios-boot-good — mark current boot as successful in grubenv
# Reads current root PARTLABEL to determine active slot (a/b)
# Writes kelvios_boot_ok=1 and kelvios_active=<slot> to EFI grubenv

set -euo pipefail

EFI_PART="/dev/disk/by-partlabel/kelvios-efi"
ROOT_PART="$(findmnt -n -o SOURCE /)"
SLOT=""

# Determine slot from root partition PARTLABEL
if [[ "${ROOT_PART}" == *"kelvios-root-a"* ]]; then
    SLOT="a"
elif [[ "${ROOT_PART}" == *"kelvios-root-b"* ]]; then
    SLOT="b"
else
    echo "ERROR: Cannot determine slot from root partition: ${ROOT_PART}" >&2
    exit 1
fi

# Mount EFI partition
MOUNT_POINT=$(mktemp -d)
mount "${EFI_PART}" "${MOUNT_POINT}"

# Write grubenv
grub-editenv "${MOUNT_POINT}/grub/grubenv" set kelvios_boot_ok=1
grub-editenv "${MOUNT_POINT}/grub/grubenv" set kelvios_active="${SLOT}"

# Verify
grub-editenv "${MOUNT_POINT}/grub/grubenv" list

umount "${MOUNT_POINT}"
rmdir "${MOUNT_POINT}"

echo "Boot marked good for slot ${SLOT}"
EOF
chmod +x "${ROOTFS_DIR}/usr/local/bin/kelvios-boot-good"

# Enable boot-good service
chroot "${ROOTFS_DIR}" systemctl enable kelvios-boot-good.service
log "Boot-good service installed and enabled."

# ─── Stage 6: Hostname & root lock ──────────────────────────────
stage "Set hostname and lock root"
echo "kelvios" > "${ROOTFS_DIR}/etc/hostname"
cat > "${ROOTFS_DIR}/etc/hosts" <<'EOF'
127.0.0.1   localhost
127.0.1.1   kelvios
::1         localhost ip6-localhost ip6-loopback
EOF

# Lock root password (disable console login for PoC)
chroot "${ROOTFS_DIR}" passwd -l root
log "Hostname set to 'kelvios', root locked."

# ─── Stage 7: Network config (DHCP via systemd-networkd) ────────
stage "Configure network (DHCP via systemd-networkd)"
mkdir -p "${ROOTFS_DIR}/etc/systemd/network"
cat > "${ROOTFS_DIR}/etc/systemd/network/20-wired.network" <<'EOF'
[Match]
Name=en* eth*

[Network]
DHCP=yes
EOF

# Enable systemd-networkd
chroot "${ROOTFS_DIR}" systemctl enable systemd-networkd.service

# Enable systemd-resolved if available (may not be in minbase)
chroot "${ROOTFS_DIR}" systemctl enable systemd-resolved.service 2>/dev/null || warn "systemd-resolved not available, skipping"

ln -sf /run/systemd/resolve/stub-resolv.conf "${ROOTFS_DIR}/etc/resolv.conf"
log "Network configuration (DHCP) installed."

# ─── Stage 8: Serial console (GRUB + kernel cmdline) ────────────
stage "Configure serial console (ttyS0)"
# GRUB serial config
mkdir -p "${ROOTFS_DIR}/etc/default/grub.d"
cat > "${ROOTFS_DIR}/etc/default/grub.d/99-serial.cfg" <<'EOF'
GRUB_TERMINAL="serial console"
GRUB_SERIAL_COMMAND="serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1"
GRUB_CMDLINE_LINUX_DEFAULT="console=ttyS0,115200n8 console=tty0"
GRUB_DEFAULT="KelviOS (A/B fallback)"
EOF
log "Serial console configured."

# ─── Stage 8b: Install A/B fallback GRUB menuentry ────────────────
stage "Install A/B fallback GRUB menuentry (07_kelvios_fallback)"
install -Dm755 "${REPO_ROOT}/40_kelvios_fallback" "${ROOTFS_DIR}/etc/grub.d/07_kelvios_fallback"
log "Fallback menuentry installed to /etc/grub.d/07_kelvios_fallback"

# ─── Stage 9: fstab template (will be finalized per-slot) ───────
stage "Prepare fstab template"
cat > "${ROOTFS_DIR}/etc/fstab.template" <<'EOF'
# <file system> <mount point> <type> <options> <dump> <pass>
PARTLABEL=kelvios-root-__SLOT__  /  ext4  defaults,noatime  0  1
PARTLABEL=kelvios-efi   /boot/efi  vfat  defaults  0  2
PARTLABEL=kelvios-data  /var/lib/kelvios  ext4  defaults,noatime  0  2
EOF
log "Fstab template prepared."

# ─── Stage 10: SSH config ───────────────────────────────────────
stage "Configure SSH"
mkdir -p "${ROOTFS_DIR}/etc/ssh/sshd_config.d"
cat > "${ROOTFS_DIR}/etc/ssh/sshd_config.d/99-kelvios.conf" <<'EOF'
PasswordAuthentication no
PubkeyAuthentication yes
PermitRootLogin prohibit-password
EOF
log "SSH configured."

# ─── Stage 11: Create disk image ────────────────────────────────
stage "Create sparse disk image (${IMAGE_SIZE_GB}GB)"
truncate -s "${IMAGE_SIZE_BYTES}" "${OUTPUT_IMG}"
log "Sparse image created: ${OUTPUT_IMG}"

stage "Partition image (GPT: EFI + root-A + root-B + data)"
sgdisk -Z "${OUTPUT_IMG}"  # zap
sgdisk -n 1:0:+${EFI_SIZE_MB}M -t 1:ef00 -c 1:"kelvios-efi" "${OUTPUT_IMG}"
sgdisk -n 2:0:+${ROOT_SIZE_MB}M -t 2:8300 -c 2:"kelvios-root-a" "${OUTPUT_IMG}"
sgdisk -n 3:0:+${ROOT_SIZE_MB}M -t 3:8300 -c 3:"kelvios-root-b" "${OUTPUT_IMG}"
if [[ ${DATA_SIZE_MB} -gt 0 ]]; then
    sgdisk -n 4:0:+${DATA_SIZE_MB}M -t 4:8300 -c 4:"kelvios-data" "${OUTPUT_IMG}"
else
    sgdisk -n 4:0:0 -t 4:8300 -c 4:"kelvios-data" "${OUTPUT_IMG}"
fi
sgdisk -p "${OUTPUT_IMG}"

# ─── Stage 12: Setup loop devices & format ──────────────────────
stage "Setup loop devices and format partitions"
LOOP_DEV=$(losetup --find --show --partscan "${OUTPUT_IMG}")
log "Loop device: ${LOOP_DEV}"
EFI_PART="${LOOP_DEV}p1"
ROOT_A_PART="${LOOP_DEV}p2"
ROOT_B_PART="${LOOP_DEV}p3"
DATA_PART="${LOOP_DEV}p4"

# Wait for partitions to appear
for i in {1..10}; do
    [[ -b "${EFI_PART}" && -b "${ROOT_A_PART}" && -b "${ROOT_B_PART}" && -b "${DATA_PART}" ]] && break
    sleep 0.5
done
[[ -b "${EFI_PART}" ]] || die "EFI partition ${EFI_PART} not found"
[[ -b "${ROOT_A_PART}" ]] || die "Root-A partition ${ROOT_A_PART} not found"
[[ -b "${ROOT_B_PART}" ]] || die "Root-B partition ${ROOT_B_PART} not found"
[[ -b "${DATA_PART}" ]] || die "Data partition ${DATA_PART} not found"

mkfs.fat -F32 -n "KELVIOS-EFI" "${EFI_PART}"
mkfs.ext4 -L "kelvios-root-a" "${ROOT_A_PART}"
mkfs.ext4 -L "kelvios-root-b" "${ROOT_B_PART}"
mkfs.ext4 -L "kelvios-data" "${DATA_PART}"

# ─── Stage 13: Mount and copy rootfs to both slots ──────────────
stage "Mount partitions and copy rootfs to slot A"
MOUNT_DIR="${WORK_DIR}/mnt"
mkdir -p "${MOUNT_DIR}"
mount "${ROOT_A_PART}" "${MOUNT_DIR}"
mkdir -p "${MOUNT_DIR}/boot/efi"
mount "${EFI_PART}" "${MOUNT_DIR}/boot/efi"
mkdir -p "${MOUNT_DIR}/var/lib/kelvios"
mount "${DATA_PART}" "${MOUNT_DIR}/var/lib/kelvios"

# Copy rootfs to slot A
rsync -aHAX --exclude=/proc --exclude=/sys --exclude=/dev --exclude=/run --exclude=/tmp \
    "${ROOTFS_DIR}/" "${MOUNT_DIR}/"

# Generate fstab for slot A (after rsync, so it doesn't get overwritten)
    mkdir -p "${MOUNT_DIR}/etc"
    sed 's/__SLOT__/a/g' "${ROOTFS_DIR}/etc/fstab.template" > "${MOUNT_DIR}/etc/fstab"

    # Regenerate initramfs for slot A (needs bind mounts)
    stage "Regenerate initramfs for slot A"
    for d in dev proc sys run; do
        mkdir -p "${MOUNT_DIR}/${d}"
        mount --bind "/${d}" "${MOUNT_DIR}/${d}"
    done
    chroot "${MOUNT_DIR}" update-initramfs -u -k all
    for d in run sys proc dev; do
        umount -l "${MOUNT_DIR}/${d}" 2>/dev/null || true
    done

    # Unmount slot A
    umount -l "${MOUNT_DIR}/var/lib/kelvios" 2>/dev/null || true
    umount -l "${MOUNT_DIR}/boot/efi" 2>/dev/null || true
    umount -l "${MOUNT_DIR}" 2>/dev/null || true

    stage "Mount partitions and copy rootfs to slot B"
mkdir -p "${MOUNT_DIR}"
mount "${ROOT_B_PART}" "${MOUNT_DIR}"
mkdir -p "${MOUNT_DIR}/boot/efi"
mount "${EFI_PART}" "${MOUNT_DIR}/boot/efi"
mkdir -p "${MOUNT_DIR}/var/lib/kelvios"
mount "${DATA_PART}" "${MOUNT_DIR}/var/lib/kelvios"

# Copy rootfs to slot B
rsync -aHAX --exclude=/proc --exclude=/sys --exclude=/dev --exclude=/run --exclude=/tmp \
    "${ROOTFS_DIR}/" "${MOUNT_DIR}/"

# Generate fstab for slot B (after rsync, so it doesn't get overwritten)
    mkdir -p "${MOUNT_DIR}/etc"
    sed 's/__SLOT__/b/g' "${ROOTFS_DIR}/etc/fstab.template" > "${MOUNT_DIR}/etc/fstab"

    # Regenerate initramfs for slot B (needs bind mounts)
    stage "Regenerate initramfs for slot B"
    for d in dev proc sys run; do
        mkdir -p "${MOUNT_DIR}/${d}"
        mount --bind "/${d}" "${MOUNT_DIR}/${d}"
    done
    chroot "${MOUNT_DIR}" update-initramfs -u -k all
    for d in run sys proc dev; do
        umount -l "${MOUNT_DIR}/${d}" 2>/dev/null || true
    done

    # ─── Stage 14: Install GRUB (BIOS + UEFI) ───────────────────────
stage "Install GRUB (BIOS + UEFI)"
# Ensure mount points for bind mounts exist in the target rootfs
for d in dev proc sys run; do
    mkdir -p "${MOUNT_DIR}/${d}"
done

# Bind mounts for chroot
for d in dev proc sys run; do
    mount --bind "/${d}" "${MOUNT_DIR}/${d}"
done

# GRUB install for UEFI (use full path)
chroot "${MOUNT_DIR}" /usr/sbin/grub-install --target=x86_64-efi --efi-directory=/boot/efi \
    --bootloader-id=kelvios --recheck --no-floppy

# Also install fallback bootloader (EFI/BOOT/BOOTX64.EFI) for UEFI firmware without NVRAM
chroot "${MOUNT_DIR}" /usr/sbin/grub-install --target=x86_64-efi --efi-directory=/boot/efi \
    --bootloader-id=BOOT --removable --recheck --no-floppy || warn "Fallback bootloader install failed"

# GRUB install for BIOS (on the loop device) - non-fatal, only if grub-pc available
if chroot "${MOUNT_DIR}" /usr/sbin/grub-install --target=i386-pc "${LOOP_DEV}" --recheck --no-floppy 2>/dev/null; then
    log "BIOS GRUB installed"
else
    warn "grub-pc not available or BIOS GRUB install failed (non-fatal)"
fi

# Generate grub config with both slots
chroot "${MOUNT_DIR}" /usr/sbin/update-grub

# ─── Stage 15: Initialize grubenv with defaults ─────────────────
stage "Initialize grubenv (kelvios_active=a, kelvios_boot_ok=0)"
# Create initial grubenv - try both possible locations
GRUBENV_PATHS=(
    "${MOUNT_DIR}/boot/efi/grub/grubenv"
    "${MOUNT_DIR}/boot/efi/EFI/kelvios/grubenv"
)
for grubenv_path in "${GRUBENV_PATHS[@]}"; do
    grubenv_dir=$(dirname "${grubenv_path}")
    mkdir -p "${grubenv_dir}"
    if grub-editenv "${grubenv_path}" create 2>/dev/null; then
        grub-editenv "${grubenv_path}" set kelvios_active=a
        grub-editenv "${grubenv_path}" set kelvios_boot_ok=0
        grub-editenv "${grubenv_path}" list
        log "grubenv initialized at ${grubenv_path}"
        break
    fi
done

# ─── Stage 16: Cleanup mounts ───────────────────────────────────
stage "Unmount and cleanup"
for d in run sys proc dev; do
    umount -l "${MOUNT_DIR}/${d}" 2>/dev/null || true
done
umount -l "${MOUNT_DIR}/var/lib/kelvios" 2>/dev/null || true
umount -l "${MOUNT_DIR}/boot/efi" 2>/dev/null || true
umount -l "${MOUNT_DIR}" 2>/dev/null || true
losetup -d "${LOOP_DEV}" 2>/dev/null || true

# ─── Stage 17: Final verification ───────────────────────────────
stage "Final verification"
log "Image: ${OUTPUT_IMG}"
log "Size: $(stat -c%s "${OUTPUT_IMG}") bytes ($(du -h "${OUTPUT_IMG}" | cut -f1))"
sgdisk -p "${OUTPUT_IMG}"

# Quick content check via loop mount (read-only)
VERIFY_LOOP=$(losetup --find --show --partscan --read-only "${OUTPUT_IMG}")
VERIFY_MOUNT="${WORK_DIR}/verify"
mkdir -p "${VERIFY_MOUNT}"

# Check slot A
mount -o ro "${VERIFY_LOOP}p2" "${VERIFY_MOUNT}"
log "Verification checks (slot A):"
[[ -f "${VERIFY_MOUNT}/usr/local/bin/kelvios-core" ]] && log "  ✓ Binary present" || log "  ✗ Binary MISSING"
[[ -f "${VERIFY_MOUNT}/etc/kelvios/config.toml" ]] && log "  ✓ Config present" || log "  ✗ Config MISSING"
[[ -f "${VERIFY_MOUNT}/etc/systemd/system/kelvios-core.service" ]] && log "  ✓ Systemd unit present" || log "  ✗ Systemd unit MISSING"
[[ -f "${VERIFY_MOUNT}/etc/systemd/system/kelvios-boot-good.service" ]] && log "  ✓ Boot-good unit present" || log "  ✗ Boot-good unit MISSING"
chroot "${VERIFY_MOUNT}" systemctl is-enabled kelvios-core.service 2>/dev/null && log "  ✓ Service enabled" || log "  ✗ Service NOT enabled"
chroot "${VERIFY_MOUNT}" systemctl is-enabled kelvios-boot-good.service 2>/dev/null && log "  ✓ Boot-good service enabled" || log "  ✗ Boot-good service NOT enabled"
grep -q "kelvios" "${VERIFY_MOUNT}/etc/hostname" && log "  ✓ Hostname set" || log "  ✗ Hostname MISSING"
grep -q "kelvios-root-a" "${VERIFY_MOUNT}/etc/fstab" && log "  ✓ fstab points to root-a" || log "  ✗ fstab WRONG"
grep -q "kelvios-data" "${VERIFY_MOUNT}/etc/fstab" && log "  ✓ fstab includes data partition" || log "  ✗ fstab missing data"
umount -l "${VERIFY_MOUNT}" 2>/dev/null || true

# Check slot B
mount -o ro "${VERIFY_LOOP}p3" "${VERIFY_MOUNT}"
log "Verification checks (slot B):"
grep -q "kelvios-root-b" "${VERIFY_MOUNT}/etc/fstab" && log "  ✓ fstab points to root-b" || log "  ✗ fstab WRONG"
umount -l "${VERIFY_MOUNT}" 2>/dev/null || true

# Check EFI grubenv
mount -o ro "${VERIFY_LOOP}p1" "${VERIFY_MOUNT}"
log "Verification checks (EFI):"
grub-editenv "${VERIFY_MOUNT}/grub/grubenv" list 2>/dev/null && log "  ✓ grubenv present" || log "  ✗ grubenv MISSING"
umount -l "${VERIFY_MOUNT}" 2>/dev/null || true

losetup -d "${VERIFY_LOOP}" 2>/dev/null || true

stage "BUILD COMPLETE"
log "Output image: ${OUTPUT_IMG}"