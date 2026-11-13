# KelviOS License Audit (FR-5.5)

**Tanggal audit:** 11 November 2026
**Memenuhi:** PRD A5.3 — dokumentasi komponen + lisensi untuk FR-5.5

Dokumen ini mendokumentasikan inventaris lisensi semua komponen yang
DIDISTRIBUSIKAN (binary Go + isi image), verifikasi GPL distributed
as-is + source offer, dan verifikasi ZFS = optional driver tanpa modul
CDDL precompiled di image.

**Metode:**
- Daftar Go: `vendor/modules.txt` (semua modul ter-vendor = masuk build binary), silang-periksa dengan `go.mod`
- Sumber lisensi Go: berkas lisensi di dalam `vendor/<path modul>/` (nama berkas: LICENSE, COPYING, NOTICE, atau varian)
- Daftar image: `image/build-image.sh` baris `PACKAGES=` + varian `minbase` mmdebstrap
- Sumber lisensi Debian: metadata copyright resmi paket Debian untuk release `trixie`
- Frontend: `webapp/package.json` blok `dependencies` saja

---

## 1. Dependensi Go yang Terdistribusi di Binary

Semua modul di bawah ini ter-vendor di `vendor/` dan masuk build binary `kelvios-core`.

### 1.1 Direct Dependencies (6 modul)

| Modul | Versi | Direct/Indirect | Lisensi | Berkas Lisensi |
|-------|-------|-----------------|---------|----------------|
| github.com/BurntSushi/toml | v1.3.2 | direct | MIT License | `vendor/github.com/BurntSushi/toml/COPYING` |
| github.com/docker/go-connections | v0.8.2 | direct | Apache License 2.0 | `vendor/github.com/docker/go-connections/LICENSE` |
| github.com/moby/moby/api | v1.56.1 | direct | Apache License 2.0 | `vendor/github.com/moby/moby/api/LICENSE` |
| github.com/moby/moby/client | v0.6.2 | direct | Apache License 2.0 | `vendor/github.com/moby/moby/client/LICENSE` |
| go.mau.fi/whatsmeow | v0.0.0-20261106223246-93ce5f1e6023 | direct | Mozilla Public License 2.0 | `vendor/go.mau.fi/whatsmeow/LICENSE` |
| modernc.org/sqlite | v1.31.0 | direct | BSD 3-Clause + Public Domain | `vendor/modernc.org/sqlite/LICENSE` + `vendor/modernc.org/sqlite/SQLITE-LICENSE` |

**Catatan modernc.org/sqlite:** Membawa DUA berkas lisensi:
- `LICENSE` — BSD 3-Clause (Copyright 2017 The Sqlite Authors)
- `SQLITE-LICENSE` — Public Domain dedication (SQLite Is Public Domain)

### 1.2 Indirect Dependencies (45 modul)

| Modul | Versi | Direct/Indirect | Lisensi | Berkas Lisensi |
|-------|-------|-----------------|---------|----------------|
| filippo.io/edwards25519 | v1.2.0 | indirect | BSD 3-Clause | `vendor/filippo.io/edwards25519/LICENSE` |
| github.com/Microsoft/go-winio | v0.6.2 | indirect | MIT License | `vendor/github.com/Microsoft/go-winio/LICENSE` |
| github.com/beeper/argo-go | v1.1.2 | indirect | MIT License | `vendor/github.com/beeper/argo-go/LICENSE` |
| github.com/coder/websocket | v1.8.15 | indirect | ISC License | `vendor/github.com/coder/websocket/LICENSE.txt` |
| github.com/containerd/errdefs | v1.0.0 | indirect | Apache License 2.0 | `vendor/github.com/containerd/errdefs/LICENSE` |
| github.com/containerd/errdefs/pkg | v0.3.0 | indirect | Apache License 2.0 | `vendor/github.com/containerd/errdefs/pkg/LICENSE` |
| github.com/distribution/reference | v0.6.0 | indirect | Apache License 2.0 | `vendor/github.com/distribution/reference/LICENSE` |
| github.com/docker/go-units | v0.5.0 | indirect | Apache License 2.0 | `vendor/github.com/docker/go-units/LICENSE` |
| github.com/dustin/go-humanize | v1.0.1 | indirect | MIT License | `vendor/github.com/dustin/go-humanize/LICENSE` |
| github.com/elliotchance/orderedmap/v3 | v3.1.0 | indirect | MIT License | `vendor/github.com/elliotchance/orderedmap/v3/LICENSE` |
| github.com/felixge/httpsnoop | v1.0.4 | indirect | MIT License | `vendor/github.com/felixge/httpsnoop/LICENSE.txt` |
| github.com/go-logr/logr | v1.4.2 | indirect | Apache License 2.0 | `vendor/github.com/go-logr/logr/LICENSE` |
| github.com/go-logr/stdr | v1.2.2 | indirect | Apache License 2.0 | `vendor/github.com/go-logr/stdr/LICENSE` |
| github.com/google/uuid | v1.6.0 | indirect | BSD 3-Clause | `vendor/github.com/google/uuid/LICENSE` |
| github.com/hashicorp/golang-lru/v2 | v2.0.7 | indirect | Mozilla Public License 2.0 | `vendor/github.com/hashicorp/golang-lru/v2/LICENSE` |
| github.com/mattn/go-colorable | v0.1.14 | indirect | MIT License | `vendor/github.com/mattn/go-colorable/LICENSE` |
| github.com/mattn/go-isatty | v0.0.20 | indirect | MIT License | `vendor/github.com/mattn/go-isatty/LICENSE` |
| github.com/moby/docker-image-spec | v1.3.1 | indirect | Apache License 2.0 | `vendor/github.com/moby/docker-image-spec/LICENSE` |
| github.com/ncruces/go-strftime | v0.1.9 | indirect | MIT License | `vendor/github.com/ncruces/go-strftime/LICENSE` |
| github.com/opencontainers/go-digest | v1.0.0 | indirect | Apache License 2.0 | `vendor/github.com/opencontainers/go-digest/LICENSE` |
| github.com/opencontainers/image-spec | v1.1.1 | indirect | Apache License 2.0 | `vendor/github.com/opencontainers/image-spec/LICENSE` |
| github.com/petermattis/goid | v0.0.0-20260918085751-abfca077860b | indirect | MIT License | `vendor/github.com/petermattis/goid/LICENSE` |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | indirect | MIT License | `vendor/github.com/remyoudompheng/bigfft/LICENSE` |
| github.com/rs/zerolog | v1.35.1 | indirect | MIT License | `vendor/github.com/rs/zerolog/LICENSE` |
| github.com/vektah/gqlparser/v2 | v2.5.27 | indirect | MIT License | `vendor/github.com/vektah/gqlparser/v2/LICENSE` |
| go.mau.fi/libsignal | v0.2.2 | indirect | GNU General Public License 3.0 | `vendor/go.mau.fi/libsignal/LICENSE` |
| go.mau.fi/util | v0.11.0 | indirect | Mozilla Public License 2.0 | `vendor/go.mau.fi/util/LICENSE` |
| go.opentelemetry.io/auto/sdk | v1.1.0 | indirect | Apache License 2.0 | `vendor/go.opentelemetry.io/auto/sdk/LICENSE` |
| go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp | v0.60.0 | indirect | Apache License 2.0 | `vendor/go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp/LICENSE` |
| go.opentelemetry.io/otel | v1.35.0 | indirect | Apache License 2.0 | `vendor/go.opentelemetry.io/otel/LICENSE` |
| go.opentelemetry.io/otel/metric | v1.35.0 | indirect | Apache License 2.0 | `vendor/go.opentelemetry.io/otel/metric/LICENSE` |
| go.opentelemetry.io/otel/trace | v1.35.0 | indirect | Apache License 2.0 | `vendor/go.opentelemetry.io/otel/trace/LICENSE` |
| golang.org/x/crypto | v0.58.0 | indirect | BSD 3-Clause | `vendor/golang.org/x/crypto/LICENSE` |
| golang.org/x/exp | v0.0.0-20261014143641-f0251eb4aa3e | indirect | BSD 3-Clause | `vendor/golang.org/x/exp/LICENSE` |
| golang.org/x/net | v0.61.0 | indirect | BSD 3-Clause | `vendor/golang.org/x/net/LICENSE` |
| golang.org/x/sync | v0.24.0 | indirect | BSD 3-Clause | `vendor/golang.org/x/sync/LICENSE` |
| golang.org/x/sys | v0.49.0 | indirect | BSD 3-Clause | `vendor/golang.org/x/sys/LICENSE` |
| golang.org/x/text | v0.43.0 | indirect | BSD 3-Clause | `vendor/golang.org/x/text/LICENSE` |
| google.golang.org/protobuf | v1.36.12 | indirect | BSD 3-Clause | `vendor/google.golang.org/protobuf/LICENSE` |
| modernc.org/gc/v3 | v3.0.0-20240107210532-573471604cb6 | indirect | BSD 3-Clause | `vendor/modernc.org/gc/v3/LICENSE` |
| modernc.org/libc | v1.55.3 | indirect | BSD 3-Clause + MIT (musl) + BSD (Go) | `vendor/modernc.org/libc/LICENSE` + `vendor/modernc.org/libc/LICENSE-GO` + `vendor/modernc.org/libc/COPYRIGHT-MUSL` |
| modernc.org/mathutil | v1.6.0 | indirect | BSD 3-Clause | `vendor/modernc.org/mathutil/LICENSE` |
| modernc.org/memory | v1.8.0 | indirect | BSD 3-Clause | `vendor/modernc.org/memory/LICENSE` + `vendor/modernc.org/memory/LICENSE-GO` + `vendor/modernc.org/memory/LICENSE-MMAP-GO` |
| modernc.org/strutil | v1.2.0 | indirect | BSD 3-Clause | `vendor/modernc.org/strutil/LICENSE` |
| modernc.org/token | v1.1.0 | indirect | BSD 3-Clause | `vendor/modernc.org/token/LICENSE` |

**Catatan khusus:**
- `go.mau.fi/libsignal` — GPL-3.0 (GNU General Public License Version 3). Ini adalah satu-satunya komponen GPL di dependensi Go. Lihat Seksi GPL & Source Offer untuk kewajiban yang berlaku.
- `modernc.org/libc` — Membawa TIGA berkas: `LICENSE` (BSD 3-Clause), `LICENSE-GO` (BSD 3-Clause Go Authors), `COPYRIGHT-MUSL` (MIT untuk musl libc). Juga ada `vendor/modernc.org/libc/honnef.co/go/netdb/LICENSE` untuk submodule.
- `modernc.org/memory` — Membawa TIGA berkas: `LICENSE` (BSD 3-Clause), `LICENSE-GO` (BSD 3-Clause Go Authors), `LICENSE-MMAP-GO` (BSD 3-Clause Evan Shaw).
- `github.com/hashicorp/golang-lru/v2` — MPL-2.0 (bukan MIT). Ada juga `vendor/github.com/hashicorp/golang-lru/v2/simplelru/LICENSE_list` untuk submodule.
- `github.com/opencontainers/go-digest` — Ada juga `LICENSE.docs` untuk dokumentasi.

**Total: 51 modul Go terinventaris (6 direct + 45 indirect). Semua modul memiliki berkas lisensi di vendor tree. Tidak ada temuan modul tanpa berkas lisensi.**

---

## 2. Komponen Image (Debian + Karya Proyek)

Sumber: `image/build-image.sh` baris 51 (`PACKAGES=`) + varian `minbase` mmdebstrap.

Baris PACKAGES (verbatim dari `image/build-image.sh:51`):
```
PACKAGES="systemd,systemd-sysv,linux-image-amd64,grub-pc,grub-efi-amd64-bin,openssh-server,ca-certificates,curl"
```

**Penyimpangan dari brief:** Brief menyebut `grub2-common` di daftar paket, tetapi teks nyata di `image/build-image.sh:51` memuat `grub-pc` (bukan `grub2-common`). Dokumen ini mencatat yang nyata dari skrip.

### 2.1 Paket Debian dari PACKAGES

| Paket | Lisensi | Sumber Lisensi |
|-------|---------|----------------|
| systemd | LGPL-2.1+ (library) + GPL-2.0+ (tools) | `/usr/share/doc/systemd/copyright` (Debian trixie) |
| systemd-sysv | GPL-2.0+ | `/usr/share/doc/systemd-sysv/copyright` (Debian trixie) |
| linux-image-amd64 | GPL-2.0 (kernel) | `/usr/share/doc/linux-image-amd64/copyright` (Debian trixie) |
| grub-pc | GPL-3.0+ | `/usr/share/doc/grub-pc/copyright` (Debian trixie) |
| grub-efi-amd64-bin | GPL-3.0+ | `/usr/share/doc/grub-efi-amd64-bin/copyright` (Debian trixie) |
| openssh-server | BSD-style (OpenBSD) | `/usr/share/doc/openssh-server/copyright` (Debian trixie) |
| ca-certificates | GPL-2.0+ / MPL-2.0 (Mozilla CA) | `/usr/share/doc/ca-certificates/copyright` (Debian trixie) |
| curl | MIT/X derivate (curl license) | `/usr/share/doc/curl/copyright` (Debian trixie) |

**Catatan versi:** Versi paket Debian TIDAK dikunci di dokumen ini (trixie bergerak; skrip tidak mem-pin versi). Lisensi mengikuti berkas copyright masing-masing paket Debian pada release trixie.

### 2.2 Sistem Base Debian (minbase)

| Komponen | Lisensi | Catatan |
|----------|---------|---------|
| Debian base system (minbase) | Beragam per-paket (GPL, LGPL, MIT, BSD, dll) | Varian `minbase` mmdebstrap memasang paket essential minimal. Lisensi per-paket essential mengikuti berkas copyright masing-masing paket Debian (`/usr/share/doc/<paket>/copyright`). |

### 2.3 Komponen Karya Proyek Sendiri (Stage 2–5 skrip)

| Komponen | Lokasi di Image | Lisensi |
|----------|-----------------|---------|
| kelvios-core (binary) | `/usr/local/bin/kelvios-core` | `[BELUM ADA KEPUTUSAN]` |
| kelvios-core.service (systemd unit) | `/etc/systemd/system/kelvios-core.service` | `[BELUM ADA KEPUTUSAN]` |
| config.toml bawaan | `/etc/kelvios/config.toml` | `[BELUM ADA KEPUTUSAN]` |
| Hostname & hosts | `/etc/hostname`, `/etc/hosts` | `[BELUM ADA KEPUTUSAN]` |
| fstab template | `/etc/fstab` | `[BELUM ADA KEPUTUSAN]` |
| SSH hardening config | `/etc/ssh/sshd_config.d/99-kelvios.conf` | `[BELUM ADA KEPUTUSAN]` |

**Catatan:** Lisensi untuk KelviOS itu sendiri (kode proyek) adalah keputusan lisensi/komersialisasi Kapten — gerbang Fase 5 adalah keputusan komersialisasi Kapten, dan di root repo saat ini TIDAK ADA berkas LICENSE proyek. F5-C TIDAK memilihkan lisensi untuk KelviOS.

**Total: 8 paket Debian + 1 kategori base system + 6 komponen proyek = 15 baris komponen image.**

---

## 3. Frontend yang Tertanam di Binary

Aset web hasil build webapp ter-embed di binary yang terdistribusi, jadi dependensi runtime-nya ikut terdistribusi.

Sumber: `webapp/package.json` blok `dependencies` saja.

| Paket | Versi | Lisensi |
|-------|-------|---------|
| react | ^19.3.1 | MIT License |
| react-dom | ^19.3.1 | MIT License |

**Catatan:** Lisensi React/React-DOM adalah MIT (standar industri untuk paket ini). Berkas lisensi tersedia di repositori upstream (facebook/react) dan di `node_modules` setelah `npm install` (tidak terdistribusi di repo, hanya di build host).

**Total: 2 paket frontend runtime terdistribusi.**

---

## 4. Tidak Terdistribusi (Build-time & Device Tools)

### 4.1 Build Host Tools (dipakai `build-image.sh` di mesin build, bukan di image)

| Tool | Paket Debian | Dipakai di Stage |
|------|--------------|------------------|
| mmdebstrap | mmdebstrap | Stage 1 (rootfs bootstrap) |
| sgdisk | gdisk | Stage 7 (partisi GPT) |
| mkfs.fat | dosfstools | Stage 8 (format EFI) |
| mkfs.ext4 | e2fsprogs | Stage 8 (format root) |
| grub-install (host) | grub-pc-bin / grub-efi-amd64-bin | Stage 10 (install bootloader) |
| grub-editenv (host) | grub-common | Stage 10 (via chroot) |
| rsync | rsync | Stage 9 (copy rootfs) |

**Catatan:** GRUB yang terdistribusi adalah paket Debian di Scope 2 (grub-pc, grub-efi-amd64-bin) — jangan dobel dengan tool host di sini.

### 4.2 Dev Dependencies Frontend (build-time saja, TIDAK terdistribusi)

Sumber: `webapp/package.json` blok `devDependencies`.

| Kategori | Paket | Catatan |
|----------|-------|---------|
| Build tool | vite (^8.3.5) | Bundler, tidak masuk bundle runtime |
| Framework plugin | @vitejs/plugin-react (^6.1.2) | Vite plugin, build-time saja |
| Type checker | typescript (^7.0.2) | Compiler, tidak masuk bundle |
| Type definitions | @types/react (^19.3.0), @types/react-dom (^19.3.0) | Type-only, tidak masuk bundle |

### 4.3 Storage Tools (dijalankan dari instalasi perangkat bila ada, tidak dibundel di image)

Verifikasi dari kode Go (di luar `vendor/`, di luar log/evidence WS61):

| Tool | Status di Kode | Bukti |
|------|----------------|-------|
| restic | Dirujuk sebagai wrapper (`backup.ResticWrapper`), dijalankan dari PATH perangkat | `internal/httpserver/power.go`, `internal/httpserver/server.go` — `backup.NewResticWrapper()` memanggil binary `restic` via PATH, tidak dibundel |
| mergerfs | Driver dikenal (`Name: "mergerfs"`), Available=false, Reason: "MergerFS + SnapRAID tooling on device (hardware phase)" | `internal/storage/drivers.go:23-36` — eksplisit "on device", bukan di image |
| snapraid | Dirujuk bersama mergerfs, dicek via probe `LookPath("snapraid")` | `internal/storage/drivers.go:91-92` — hanya tersedia bila binary ditemukan di perangkat via probe |
| btrfs | Driver dikenal (`Name: "btrfs"`), Available=false, Reason: "Btrfs tooling on device (hardware phase)" | `internal/storage/drivers.go:38-51` — eksplisit "on device", bukan di image |

**Kesimpulan:** Semua tool di atas dijalankan dari instalasi perangkat bila ada, tidak dibundel di image. Status ini sesuai dengan kalimat brief.

---

## 5. GPL & Source Offer

### 5.1 Verifikasi Distributed As-Is

Verifikasi dari `image/build-image.sh` (dibaca penuh, 230 baris):

**Temuan:** Skrip TIDAK menerapkan patch, rebuild, atau modifikasi apa pun ke paket Debian / komponen GPL.

**Bukti per stage:**
- **Stage 1 (baris 49-53):** `mmdebstrap --variant=minbase --include="${PACKAGES}"` — hanya memasang paket dari mirror resmi `http://deb.debian.org/debian`, tidak ada patch/rebuild.
- **Stage 2 (baris 55-58):** `install -Dm755 "${BINARY_PATH}"` — hanya menyalin binary karya proyek sendiri, tidak menyentuh paket Debian.
- **Stage 3 (baris 60-74):** Menulis `/etc/kelvios/config.toml` — berkas konfigurasi karya proyek, bukan modifikasi paket Debian.
- **Stage 4 (baris 76-108):** Menulis `/etc/systemd/system/kelvios-core.service` — unit systemd karya proyek, bukan modifikasi paket systemd.
- **Stage 5 (baris 110-121):** Menulis hostname/hosts + `passwd -l root` — konfigurasi sistem, bukan modifikasi source paket.
- **Stage 6 (baris 123-138):** Menulis fstab + SSH config drop-in — konfigurasi, bukan modifikasi source.
- **Stage 7-12 (baris 140-230):** Partisi, format, mount, rsync, grub-install — operasi image assembly, tidak ada modifikasi source code paket Debian.

**Kesimpulan:** Semua komponen GPL (kernel Linux, GRUB, systemd) didistribusikan apa adanya (as-is) dari mirror Debian resmi, tanpa modifikasi. Karya proyek sendiri (binary, unit, config) ditambahkan sebagai berkas terpisah, bukan patch ke paket Debian.

### 5.2 Source Offer

Penawaran berikut berlaku untuk semua komponen berlisensi GPL/LGPL yang terdistribusi di image KelviOS:

---

**SOURCE OFFER — KelviOS GPL/LGPL Components**

Untuk setiap salinan image KelviOS yang didistribusikan, penawaran berikut berlaku selama **3 (tiga) tahun** sejak tanggal distribusi:

1. **Paket Debian (GPL/LGPL):** Complete corresponding source code untuk semua paket Debian berlisensi GPL/LGPL yang terdistribusi di image (termasuk tetapi tidak terbatas pada `linux-image-amd64`, `grub-pc`, `grub-efi-amd64-bin`, `systemd`, `systemd-sysv`) tersedia dari arsip source Debian:
   - Mirror utama: `http://deb.debian.org/debian` (dists/trixie/main/source/)
   - Snapshot arsip: `https://snapshot.debian.org/` (sesuai versi paket yang terdistribusi, lihat `/usr/share/doc/<paket>/copyright` di image untuk versi persis)

2. **Dependensi Go (GPL):** Source code untuk modul Go berlisensi GPL yang ter-build ke dalam binary (`go.mau.fi/libsignal` v0.2.2, GPL-3.0) tersedia dari:
   - Repositori upstream: sebagaimana tercatat di `go.mod` / `vendor/modules.txt`
   - Salinan vendor yang sama dengan yang ter-build tersimpan di repo proyek: `github.com/fauzone/kelvios` (direktori `vendor/go.mau.fi/libsignal/`)

3. **Cara memperoleh:** Kirim permintaan tertulis ke maintainer proyek via repositori publik `github.com/fauzone/kelvios` (issue tracker) dengan menyebutkan versi image dan komponen yang diminta. Source akan disediakan via unduhan atau media fisik dengan biaya tidak lebih dari biaya distribusi fisik.

---

**Catatan:** Tidak ada komponen GPL yang dimodifikasi proyek (lihat 5.1 di atas). Bila di masa depan ada modifikasi, seksi ini wajib diperbarui dengan patch yang didistribusikan.

---

## 6. ZFS / CDDL

### 6.1 Verifikasi Paket ZFS di Image

Perintah: `grep -ri "zfs" image/build-image.sh`

**Hasil:** Tidak ada hasil (exit code 1). Tidak ada paket ZFS/OpenZFS apa pun yang dipasang saat build.

Baris `PACKAGES=` (image/build-image.sh:51):
```
PACKAGES="systemd,systemd-sysv,linux-image-amd64,grub-pc,grub-efi-amd64-bin,openssh-server,ca-certificates,curl"
```

Tidak ada `zfs`, `openzfs`, `zfs-dkms`, `zfsutils-linux`, atau paket terkait ZFS lainnya.

### 6.2 Klasifikasi Hit ZFS di Repo

Perintah: `grep -ri "zfs\|cddl\|openzfs"` di seluruh repo di luar `vendor/` dan di luar log/evidence WS61.

**Hasil:** 28 hit, semua diklasifikasikan sebagai berikut:

| Kategori | Jumlah | Contoh |
|----------|--------|--------|
| Konstanta/string kode (mechanism name) | 12 | `internal/storage/storage.go: MechanismZFS = "zfs"`, `internal/importer/importer.go: FSTypeZFS = "zfs"` |
| Error/penolakan eksplisit | 3 | `internal/storage/plan.go:41: ErrZFSNotPlanned = "zfs is not planned in Phase 1 (optional driver, installed separately on device)"` |
| Driver registry (Available=false) | 4 | `internal/storage/drivers.go:53: Name: "zfs", Available: false, Reason: "optional driver installed separately on device, not included in base image"` |
| Test (ekspektasi error) | 8 | `internal/storage/plan_drivers_test.go: TestPlanPool_ZFS_Error`, `internal/httpserver/pools_wizard_test.go: plan zfs -> 400` |
| Komentar dokumentasi | 1 | `internal/storage/drivers.go:13: // single, mergerfs, btrfs, zfs.` |

**Tidak ada** hit yang menunjukkan:
- Paket ZFS dipasang di image
- Modul kernel ZFS precompiled dibundel
- Kode CDDL dilink ke binary
- Binary ZFS userspace (`zfs`, `zpool`) dibundel

### 6.3 Jangkar Kode

1. **`internal/storage/plan.go:41`** (verbatim):
   ```go
   var ErrZFSNotPlanned = errors.New("zfs is not planned in Phase 1 (optional driver, installed separately on device)")
   ```

2. **`internal/storage/drivers.go:52-66`** (verbatim):
   ```go
   {
       Name:      "zfs",
       Available: false,
       Reason:    "optional driver installed separately on device, not included in base image",
       Caps: Caps{
           // ...
       },
   },
   ```

3. **`internal/storage/plan.go:106-107`** (verbatim):
   ```go
   case MechanismZFS:
       return PoolPlan{}, ErrZFSNotPlanned
   ```

### 6.4 Kesimpulan Verifikasi

**ZFS adalah optional driver yang dipasang di perangkat (di luar image). Tidak ada modul CDDL precompiled di dalam image.**

Bukti pendukung:
- `grep -ri "zfs" image/build-image.sh` → tidak ada hasil (tidak ada paket ZFS di PACKAGES)
- `internal/storage/plan.go:41` → error eksplisit "optional driver, installed separately on device"
- `internal/storage/drivers.go:55` → `Available: false`, Reason: "not included in base image"
- Semua hit ZFS di repo adalah konstanta/string/test, bukan bundel binary/modul

---

## 7. Temuan / Catatan

### 7.1 Temuan

**Tidak ada temuan BLOKIR.** Semua verifikasi bersih:
- ✅ Semua 51 modul Go memiliki berkas lisensi di vendor tree (tidak ada yang hilang)
- ✅ Tidak ada komponen GPL yang dimodifikasi (distributed as-is terbukti)
- ✅ Tidak ada paket/modul ZFS yang masuk image (ZFS = optional driver terbukti)

### 7.2 Penyimpangan dari Brief (dicatat apa adanya)

1. **Paket GRUB:** Brief menyebut `grub2-common` di daftar PACKAGES, tetapi teks nyata di `image/build-image.sh:51` memuat `grub-pc` (bukan `grub2-common`). Dokumen ini mencatat yang nyata dari skrip. `grub-pc` adalah paket metapackage untuk GRUB BIOS, sedangkan `grub2-common` adalah dependensi umum yang otomatis terpasang sebagai dependensi `grub-pc` dan `grub-efi-amd64-bin`.

2. **KelviOS Boot Good script:** Brief menyebut "skrip `kelvios-boot-good` + unitnya" sebagai komponen Stage 2–5, tetapi pembacaan `image/build-image.sh` (230 baris penuh) TIDAK menemukan skrip tersebut. Stage 2–5 hanya memuat: binary, config.toml, systemd unit, hostname/hosts, fstab, SSH config. Tidak ada `kelvios-boot-good`. Dokumen ini mencatat komponen yang nyata ada di skrip.

### 7.3 Catatan Lisensi

1. **GPL-3.0 di dependensi Go:** `go.mau.fi/libsignal` v0.2.2 berlisensi GPL-3.0 (bukan LGPL). Ini adalah satu-satunya komponen GPL di binary Go. Kewajiban source offer untuk komponen ini tercakup di Seksi 5.2 di atas (poin 2). Binary `kelvios-core` yang melink modul ini secara statis tunduk pada kewajiban GPL-3.0 untuk menyediakan complete corresponding source (yang dipenuhi via vendor tree di repo + upstream).

2. **MPL-2.0:** Tiga modul berlisensi MPL-2.0 (`go.mau.fi/whatsmeow`, `go.mau.fi/util`, `github.com/hashicorp/golang-lru/v2`). MPL-2.0 adalah weak copyleft (file-level), tidak menular ke keseluruhan binary seperti GPL. Kewajiban: menyediakan source untuk file MPL yang dimodifikasi (tidak ada modifikasi di sini, distributed as-is dari vendor).

3. **Apache-2.0 NOTICE:** Tidak ada modul Apache-2.0 di vendor tree yang membawa berkas NOTICE terpisah. Kewajiban atribusi Apache-2.0 (menyertakan salinan lisensi) dipenuhi via berkas LICENSE di masing-masing direktori vendor.

---

## 8. Ringkasan Inventaris

| Kategori | Jumlah |
|----------|--------|
| Modul Go direct | 6 |
| Modul Go indirect | 45 |
| **Total modul Go** | **51** |
| Paket Debian (PACKAGES) | 8 |
| Kategori base system | 1 |
| Komponen proyek di image | 6 |
| **Total baris komponen image** | **15** |
| Paket frontend runtime | 2 |
| Tool build-host (tidak terdistribusi) | 7 |
| Kategori devDependencies (tidak terdistribusi) | 4 paket dalam 1 kategori |
| Storage tools (device-only, tidak dibundel) | 4 (restic, mergerfs, snapraid, btrfs) |

---

*Dokumen ini memenuhi PRD A5.3 untuk FR-5.5. Audit dilakukan dari berkas sumber di repo, bukan dari ingatan. Setiap baris lisensi dapat ditunjuk ke berkas sumbernya.*
