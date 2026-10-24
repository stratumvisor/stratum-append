# STRATUM Container Appender

**Offline content packaging for STRATUM container image archives**

STRATUM Container Appender adds customer-supplied files to an existing `stratum-container.tar` image archive **without a Docker daemon, registry, internet access, or Go installation**. It creates a new, loadable image archive containing the original image and one additional filesystem layer, preserving the original archive format (Docker-save or OCI+Docker hybrid).

Typical additions include virtual machine disk images (QCOW2, VHD/VHDX), ISO images, appliance templates, deployment content, and customer configuration artifacts.

Typical appending content examples:
/stratum/addons/qemu/paloalto-8.1.0/virtioa.qcow2
/stratum/html/templates/intel/paloalto.yml
The above would deliver a pre-installed VM Template+disk for immediate use within STRATUM when deployed out-of-box.
NOTE: Many existig templates exist and you may never need to add one yourself. linux- ad win- are existing template files so your addons/qemu/ directory name can be linux-customname-1.0.0, etc..

## Install

Choose the correct executable for your host CPU architecture and make it executable:

```bash
# x86_64 / amd64
chmod +x stratum-container-append-linux-amd64
./stratum-container-append-linux-amd64 --version

# Optional: install system-wide (requires appropriate permissions)
sudo install -m 0755 stratum-container-append-linux-amd64 /usr/local/bin/stratum-container-append
```

For ARM64 hosts, use `stratum-container-append-linux-arm64`. CPU architecture is the **host running this tool**, not necessarily the architecture of the image being modified. The program preserves the existing container image OS/architecture.

## Quick start — add a directory of VM images

Suppose you have this local content:

```text
customer-disks/
  sata.qcow2
  installation.iso
```

The following example embeds both files under `/stratum/addons/qemu/customvm/` inside the image:

```bash
stratum-container-append \
  --input stratum-container.tar-ORIG \
  --output stratum-container.tar \
  --source ./customer-disks \
  --dest /stratum/addons/qemu/customvm
```

Files appear inside the modified container image at:

```text
/stratum/addons/qemu/customvm/sata.qcow2
/stratum/addons/qemu/customvm/installation.iso
```

The input archive is **never changed**. The output is a new, complete image archive. If the source image contains `stratum:latest`, that tag is preserved by default.

### Preview without writing an archive

Append `--dry-run` to validate the image, source directory, destination and layer contents. No output image is created:

```bash
stratum-container-append \
  --input stratum-container.tar-ORIG \
  --output stratum-container.tar \
  --source ./customer-disks \
  --dest /stratum/addons/qemu/customvm \
  --dry-run
```

The dry run reads the content to calculate its layer digest, but does not create a temporary or final image archive.

## Add files to multiple internal STRATUM locations

For a custom VM family, you may need both a template and a disk image at distinct container paths. Assemble them under a common **overlay directory**:

```text
overlay/
  stratum/
    html/
      templates/
        intel/
          customvm/
            customvm.yaml
    addons/
      qemu/
        customvm/
          sata.qcow2
```

Add the entire tree at the container filesystem root:

```bash
stratum-container-append \
  --input stratum-container.tar-ORIG \
  --output stratum-container.tar \
  --source ./overlay \
  --dest /
```

`--source` preserves the content's relative subdirectory structure. Using `--dest /` is appropriate for an overlay tree that explicitly contains the intended top-level paths; **do not use it for an unreviewed collection of files**.

Template naming and disk naming are STRATUM-specific. For example, some template families use `sata.qcow2`, while Arsenal-managed template families may require standardized disk names such as `sda.qcow2`, `virtioa.qcow2`, or special media names such as `cdrom.iso`. Use the naming requirements of the STRATUM version into which the content will be loaded.

## Add a prepared filesystem layer tarball

This mode is useful when a layer has already been assembled, such as `compliance.tar`:

```bash
stratum-container-append \
  --input stratum-container.tar-ORIG \
  --output stratum-container.tar \
  --layer ./compliance.tar
```

**A content layer tarball is not a container image archive.** It contains ordinary filesystem entries with container-relative paths, without a leading `/`. For example:

```text
compliance/
  sbom.json
  provenance.json
```

To create such a layer yourself:

```bash
mkdir -p overlay/compliance
cp sbom.json overlay/compliance/
tar -C overlay -cf compliance.tar .
```

Only ordinary files and directories are supported in prepared content layers; symlinks, hardlinks, device nodes, and unsafe (`..`/absolute) paths are rejected. Compressed `.tar.gz` layers are not accepted; supply an **uncompressed `.tar`**.

## Inspect an image archive

```bash
stratum-container-append inspect --input stratum-container.tar
```

Example output:

```text
--------------------------------------------------------------
STRATUM  /  CONTAINER CONTENT APPENDER  v1.0.0
THINKING DATACENTERS     OFFLINE CONTAINER CUSTOMIZATION
--------------------------------------------------------------
Archive       stratum-container.tar
Image tags    stratum:latest
Platform      linux/amd64
Layers        12
Config        blobs/sha256/8a2e...
Format        OCI + Docker hybrid (single image)
```

Layer counts and config file names shown above are illustrative.

## Reference: command-line options

Run `stratum-container-append --help` to display the complete reference and examples.

| Option | Meaning |
|---|---|
| `-i, --input FILE` | Original image archive. `--base` is an alias. |
| `-o, --output FILE` | New image archive. Must differ from input. |
| `-s, --source DIR` | Directory tree to add. Requires `--dest`. |
| `-d, --dest PATH` | Absolute POSIX path inside the container for `--source`. |
| `-l, --layer FILE` | Existing uncompressed content-layer tar. Alternative to `--source`. |
| `-t, --tag NAME` | Set a new output image tag (otherwise preserve source tag or tags). |
| `-f, --force` | Replace an existing *output* archive. Cannot overwrite the source archive. |
| `-n, --dry-run` | Validate inputs and report what would be added. |
| `-q, --quiet` | Suppress status/banner output. Errors still use stderr. |
| `-h, --help` | Print help. |
| `--version` | Print version. |

Exit code `0` indicates success; nonzero indicates failure. Output archives are written to a temporary file and published at the end of a successful operation, so failed operations do not leave a partially written final image archive. Existing output archives are protected unless `--force` is explicitly supplied.

### Preserve the expected image tag

By default, original image tag(s) are retained, which avoids changing the name expected by an existing STRATUM import workflow. If you intentionally need a new tag:

```bash
stratum-container-append \
  --input stratum-container.tar-ORIG \
  --output stratum-container-custom.tar \
  --source ./overlay --dest / \
  --tag stratum:custom
```

### Confirm the result

```bash
stratum-container-append inspect --input stratum-container.tar
sha256sum stratum-container.tar

# On a system using Docker as its image loader:
docker load -i stratum-container.tar
```

If your STRATUM system imports images using containerd or another host-managed image loader, follow its standard offline image-import procedure instead of assuming Docker is installed.


### Docker and OCI hybrid behavior

The utility reads the input format automatically and preserves it in the output:

- **Docker-save input:** The original layer tar members are copied unchanged; one uncompressed content layer and updated Docker image configuration/manifest are added.
- **OCI+Docker hybrid input:** Every existing OCI layer blob is copied unchanged, including mixed uncompressed and gzip layers. Only the new content layer is compressed. New SHA-256-addressed configuration and OCI image-manifest blobs are created, the OCI index (including nested indexes, if present) is updated, and Docker's `manifest.json` is updated to the same resulting image. No containerd conversion is required.

In either format, the resulting tar can be loaded with the supported image importer. A successful `docker load` alone is not proof of a healthy layer; test actual unpacking and the presence of the added files. An OCI-only archive with no Docker `manifest.json` is not a supported input.

## 8. Operational and security considerations

- **Use trusted input.** These files become part of a container filesystem and are distributed to hosts that import the modified image. Review untrusted supplied VM disks, binaries, templates, and configuration before including them.
- **Do not embed secrets casually.** Passwords, SSH keys, and other secrets in an image layer remain recoverable from the archive, even if files are later removed in another image layer.
- **Storage requirements.** Budget disk space for the untouched input archive, the new complete output archive, and (in `--source` mode) a temporary uncompressed content-layer tar. For OCI+Docker hybrid inputs, the appender also stages one gzip-compressed copy of the new layer. Existing layers are not decompressed or recompressed. Large VM disks and sparse images may consume significant space during packaging.
- **Archive format.** Supports a **single Linux image** in a Docker-save tar or an **OCI+Docker hybrid tar** containing `oci-layout`, `index.json`, and Docker `manifest.json`. Existing gzip-compressed and uncompressed layers are preserved byte-for-byte. For hybrid archives, the new layer is gzip-compressed and all OCI and Docker references, including nested OCI indexes, are updated. OCI-only image archives lacking Docker `manifest.json` and multi-image Docker manifests are not supported. Multiple repository tags for the same image are preserved by default.
- **Guest disks are not automatically registered.** STRATUM may require a template definition, matching family/version folder, content refresh, or import operation to make new images selectable.
- **Host-mounted paths.** Content written into a path covered by a volume or bind mount may not be visible at runtime. Verify the effective STRATUM container mounts before choosing `--dest`.
- **Content metadata.** Directory mode retains the source file and directory permission bits and timestamps, and assigns root ownership (UID/GID 0) inside the layer. Arbitrary ownership metadata from the host is not imported. Prepared tar mode retains metadata in the tarball.
- **No image execution.** This tool does not run the supplied VM images or container image, contact registries, or modify running containers.
- **Private output by default.** The resulting archive is created with file permissions `0600`. Share it only through your normal secure distribution process.

## Build from source

The source is self-contained and uses no external Go modules. Go **1.23 or newer** is sufficient for this release; The binaries ship statically linked, so **Go is not required on the target host**.

```bash
# In the extracted source directory
./scripts/build.sh

# Regression tests
go test ./...
```

Or build a single Linux amd64 executable directly:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -buildvcs=false -ldflags='-s -w' \
  -o stratum-container-append-linux-amd64 \
  ./cmd/stratum-container-append
```

ARM64:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -trimpath -buildvcs=false -ldflags='-s -w' \
  -o stratum-container-append-linux-arm64 \
  ./cmd/stratum-container-append
```

All compilation and tests can be performed offline with the local Go installation. For production distribution, build using your organization's supported and security-maintained Go toolchain (for STRATUM, Go 1.26.5 is the intended build toolchain). This reference distribution's provided binaries were built using Go 1.23.2 in an offline validation environment.

## Troubleshooting

**`manifest.json missing`** — The input has no Docker-compatible `manifest.json`. Export a single-image Docker-save archive or an OCI+Docker hybrid archive containing `manifest.json`.

**`expected one image`** — The Docker manifest contains more than one image entry. Export the required STRATUM image separately; this utility intentionally avoids ambiguously modifying a multi-image bundle.

**`incomplete OCI+Docker hybrid`** — The input contains only one of `index.json` or `oci-layout`. Supply a complete hybrid archive or a Docker-save archive.

**`cannot match Docker manifest image to an OCI image manifest`** — The OCI index and Docker manifest refer to different image metadata or layers. Regenerate a consistent hybrid archive before appending content.

**`source image has no repository tag`** — Add `--tag stratum:latest` (or the image reference expected by your environment).

**`output already exists`** — Choose a new output filename or use `--force` if you intentionally want to replace that output. The input file is never overwritten.

**`source contains unsupported file`** — The source tree contains a symlink, socket, or special file. Replace it with an ordinary file before packaging.

**The image imports, but content is not visible** — Check the exact destination path and whether runtime mounts shadow that directory. If the files are present but the VM is not listed, follow the applicable STRATUM template/Arsenal discovery or import procedure.

---

© 2026 Thinking Datacenters. All rights reserved.
