// STRATUM Container Appender
// Copyright (c) 2026 Thinking Datacenters.
// Offline editing of a single-image, Docker-save-format container archive.
// Only the Go standard library is used: no Docker daemon, registry, or Crane required.
package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	version     = "1.1.0"
	maxMetadata = 16 << 20 // Per metadata document and total candidate cache limit
	toolName    = "stratum-container-append"
	banner      = "STRATUM  /  CONTAINER CONTENT APPENDER"
	vendorLine  = "THINKING DATACENTERS     OFFLINE CONTAINER CUSTOMIZATION"
)

type options struct {
	input  string
	output string
	source string
	dest   string
	layer  string
	tag    string
	force  bool
	dryRun bool
	quiet  bool
}

type imageEntry struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type imageMeta struct {
	entry          imageEntry
	manifestFields map[string]json.RawMessage
	config         map[string]json.RawMessage
	paths          map[string]bool
	os             string
	arch           string
	diffIDs        []string
	hybrid         *hybridImage
}

type layerInfo struct {
	path    string
	diffID  string
	bytes   int64
	files   int
	entries int
}

func main() {
	if err := execute(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
}

func execute(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printHelp(stdout)
		return nil
	}
	if len(args) > 0 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		printHelp(stdout)
		return nil
	}
	if len(args) > 0 && (args[0] == "--version" || args[0] == "version") {
		fmt.Fprintf(stdout, "%s %s\n", toolName, version)
		return nil
	}
	inspect := len(args) > 0 && args[0] == "inspect"
	if inspect {
		args = args[1:]
	} else if len(args) > 0 && args[0] == "append" {
		args = args[1:]
	}

	opt, help, err := parseArgs(args, inspect, stderr)
	if err != nil {
		return err
	}
	if help {
		if inspect {
			printInspectHelp(stdout)
		} else {
			printHelp(stdout)
		}
		return nil
	}
	if inspect {
		if opt.input == "" {
			return errors.New("inspect requires --input FILE")
		}
		if opt.source != "" || opt.layer != "" || opt.output != "" || opt.dest != "" || opt.tag != "" || opt.force || opt.dryRun {
			return errors.New("inspect only accepts --input (and --quiet)")
		}
		if !opt.quiet {
			printBanner(stdout)
			fmt.Fprintln(stdout, "Inspecting source image archive ...")
		}
		meta, err := readImageMeta(opt.input)
		if err != nil {
			return err
		}
		printInspection(stdout, opt.input, meta)
		return nil
	}
	if err := validateOptions(opt); err != nil {
		return err
	}
	if err := validateTag(opt.tag); err != nil {
		return err
	}
	if !opt.quiet {
		printBanner(stdout)
		fmt.Fprintln(stdout, "Inspecting source image archive ...")
	}
	meta, err := readImageMeta(opt.input)
	if err != nil {
		return err
	}
	if len(meta.entry.RepoTags) == 0 && opt.tag == "" {
		return errors.New("source image has no repository tag; supply --tag stratum:latest")
	}
	if opt.dryRun && !opt.quiet {
		fmt.Fprint(stdout, "DRY RUN  /  Checking input and layer; output will not be written.\n\n")
	}

	var layer layerInfo
	var staged string
	if !opt.quiet {
		if opt.source != "" {
			fmt.Fprintln(stdout, "Preparing customer content layer ...")
		} else {
			fmt.Fprintln(stdout, "Validating supplied content layer ...")
		}
	}
	if opt.source != "" {
		// With --source, build an uncompressed layer in the output filesystem so
		// subsequent image generation can stream it without retaining VM images in RAM.
		if opt.dryRun {
			layer, err = packDirectory(opt.source, opt.dest, io.Discard)
		} else {
			var f *os.File
			f, err = os.CreateTemp(filepath.Dir(opt.output), ".stratum-content-layer-*.tar")
			if err == nil {
				staged = f.Name()
				defer os.Remove(staged)
				layer, err = packDirectory(opt.source, opt.dest, f)
				if closeErr := f.Close(); err == nil {
					err = closeErr
				}
				layer.path = staged
			}
		}
		if err != nil {
			return fmt.Errorf("preparing content directory: %w", err)
		}
	} else {
		layer, err = validateLayer(opt.layer)
		if err != nil {
			return fmt.Errorf("validating content layer: %w", err)
		}
	}

	if contains(meta.diffIDs, layer.diffID) {
		return errors.New("an identical layer already exists in the image; refusing to add it again")
	}
	layerTarPath := fmt.Sprintf("stratum-content/%s/layer.tar", strings.TrimPrefix(layer.diffID, "sha256:"))
	if meta.hybrid == nil && meta.paths[layerTarPath] {
		return fmt.Errorf("the generated layer name %q already exists in the archive", layerTarPath)
	}
	outTags := append([]string(nil), meta.entry.RepoTags...)
	if opt.tag != "" {
		outTags = []string{opt.tag}
	}
	if !opt.quiet {
		fmt.Fprintf(stdout, "Input archive   %s\n", opt.input)
		fmt.Fprintf(stdout, "Output archive  %s\n", opt.output)
		fmt.Fprintf(stdout, "Image           %s (%s/%s)\n", strings.Join(outTags, ", "), meta.os, meta.arch)
		if opt.source != "" {
			fmt.Fprintf(stdout, "Content source  %s\n", opt.source)
			fmt.Fprintf(stdout, "Container path  %s\n", opt.dest)
		} else {
			fmt.Fprintf(stdout, "Content layer   %s\n", opt.layer)
		}
		fmt.Fprintf(stdout, "New layer       %d file(s), %s\n", layer.files, humanBytes(layer.bytes))
		fmt.Fprintf(stdout, "Layers          %d -> %d\n", len(meta.entry.Layers), len(meta.entry.Layers)+1)
	}
	if opt.dryRun {
		if !opt.quiet {
			fmt.Fprintln(stdout, "\nVALIDATION PASSED  /  No output archive created.")
		}
		return nil
	}
	if !opt.quiet {
		fmt.Fprintln(stdout, "\nWriting updated container archive ...")
	}
	var outputSHA string
	if meta.hybrid != nil {
		outputSHA, err = writeUpdatedHybrid(opt, meta, layer, outTags)
	} else {
		outputSHA, err = writeUpdatedImage(opt, meta, layer, layerTarPath, outTags)
	}
	if err != nil {
		return err
	}
	if !opt.quiet {
		fmt.Fprintf(stdout, "\nSUCCESS  /  %s\n", opt.output)
		fmt.Fprintf(stdout, "SHA256   /  %s\n", outputSHA)
		fmt.Fprintln(stdout, "Load this archive with your normal STRATUM/container-image import procedure.")
	}
	return nil
}

func parseArgs(args []string, inspect bool, stderr io.Writer) (options, bool, error) {
	var o options
	fs := flag.NewFlagSet(toolName, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.input, "input", "", "Existing Docker-save or OCI+Docker hybrid image tar")
	fs.StringVar(&o.input, "i", "", "Alias for --input")
	fs.StringVar(&o.input, "base", "", "Alias for --input")
	fs.StringVar(&o.output, "output", "", "New image tar archive (never the input path)")
	fs.StringVar(&o.output, "o", "", "Alias for --output")
	fs.StringVar(&o.source, "source", "", "Directory to package into a new filesystem layer")
	fs.StringVar(&o.source, "s", "", "Alias for --source")
	fs.StringVar(&o.dest, "dest", "", "Absolute container destination for --source")
	fs.StringVar(&o.dest, "d", "", "Alias for --dest")
	fs.StringVar(&o.layer, "layer", "", "Prepared uncompressed filesystem layer .tar")
	fs.StringVar(&o.layer, "l", "", "Alias for --layer")
	fs.StringVar(&o.tag, "tag", "", "Override output image tag; default: preserve existing tags")
	fs.StringVar(&o.tag, "t", "", "Alias for --tag")
	fs.BoolVar(&o.force, "force", false, "Allow replacing an existing output file")
	fs.BoolVar(&o.force, "f", false, "Alias for --force")
	fs.BoolVar(&o.dryRun, "dry-run", false, "Validate and report without writing an output archive")
	fs.BoolVar(&o.dryRun, "n", false, "Alias for --dry-run")
	fs.BoolVar(&o.quiet, "quiet", false, "Suppress status and banner output (errors still shown)")
	fs.BoolVar(&o.quiet, "q", false, "Alias for --quiet")
	help := false
	fs.BoolVar(&help, "help", false, "Show help")
	fs.BoolVar(&help, "h", false, "Alias for --help")
	if err := fs.Parse(args); err != nil {
		return o, false, fmt.Errorf("argument error (use --help): %w", err)
	}
	if len(fs.Args()) != 0 {
		return o, false, fmt.Errorf("unexpected arguments %q (use --help)", fs.Args())
	}
	return o, help, nil
}

func printBanner(w io.Writer) {
	fmt.Fprintln(w, "--------------------------------------------------------------")
	fmt.Fprintf(w, "%s  v%s\n", banner, version)
	fmt.Fprintln(w, vendorLine)
	fmt.Fprintln(w, "--------------------------------------------------------------")
}

func printHelp(w io.Writer) {
	printBanner(w)
	fmt.Fprint(w, `
Add local files to a STRATUM container image archive without Docker, a
registry, root privileges, or an internet connection. Existing layers and
image runtime configuration are retained; one content layer is appended.

USAGE
  stratum-container-append --input IMAGE.tar --output NEW.tar \
      --source DIRECTORY --dest /CONTAINER/PATH [OPTIONS]

  stratum-container-append --input IMAGE.tar --output NEW.tar \
      --layer CONTENT.tar [OPTIONS]

  stratum-container-append inspect --input IMAGE.tar
  stratum-container-append --version

CONTENT OPTIONS (choose one)
  -s, --source DIR       Directory whose contents should be included.
  -d, --dest PATH        Absolute location inside the container for --source.
  -l, --layer FILE       Existing, uncompressed filesystem layer tarball.
                         Archive entries already contain container-relative paths.

ARCHIVE OPTIONS
  -i, --input FILE       Source container image tar (alias: --base).
  -o, --output FILE      New image tar; input is never modified.
  -t, --tag NAME         Replace the existing image tag(s) in the output.
  -f, --force            Replace an existing output archive (never the input).
  -n, --dry-run          Validate content and image, without writing output.
  -q, --quiet            Suppress banner and status output.
  -h, --help             Show this help.
      --version          Print the utility version.

EXAMPLES
  # Include a directory tree beneath a container path:
  stratum-container-append \
    --input stratum-container.tar --output stratum-container-custom.tar \
    --source ./sideload --dest /stratum/addons/qemu/customvm

  # Add a prebuilt filesystem layer with explicit container paths:
  stratum-container-append \
    --input stratum-container.tar --output stratum-container-custom.tar \
    --layer ./compliance.tar

  # Examine archive architecture, tags and current layer count:
  stratum-container-append inspect --input stratum-container.tar

NOTES
  * Supports single-image Docker-save and OCI+Docker hybrid .tar archives.
  * Existing gzip and uncompressed base layers are preserved byte-for-byte.
  * OCI-only archives without Docker manifest.json are not supported.
  * The destination must match STRATUM's expected content/discovery location.
  * Adding a VM disk or ISO does not automatically register an Arsenal template.
  * A prepared layer tar must contain safe, relative POSIX paths; no links or
    device nodes are accepted. Source directories cannot contain symlinks.
  * Files are embedded in the image. Host volume/bind mounts may hide them.
  * The image may grow considerably when including QCOW2/VHDX/ISO media.

Documentation: README.md included with the distribution.
`)
}

func printInspectHelp(w io.Writer) {
	printBanner(w)
	fmt.Fprint(w, "\nUSAGE\n  stratum-container-append inspect --input IMAGE.tar [--quiet]\n\nShows tags, OS/architecture, layer count and config path. No modification.\n")
}

func validateOptions(o options) error {
	if o.input == "" || o.output == "" {
		return errors.New("--input and --output are required (use --help)")
	}
	if (o.source == "") == (o.layer == "") {
		return errors.New("choose exactly one of --source DIRECTORY or --layer CONTENT.tar")
	}
	if o.source != "" {
		if err := validateDest(o.dest); err != nil {
			return err
		}
	} else if o.dest != "" {
		return errors.New("--dest is only valid together with --source (tar entries in --layer already have paths)")
	}
	inStat, err := os.Stat(o.input)
	if err != nil {
		return fmt.Errorf("opening input archive: %w", err)
	}
	if !inStat.Mode().IsRegular() {
		return errors.New("input archive must be a regular file")
	}
	if outStat, err := os.Stat(o.output); err == nil {
		if os.SameFile(inStat, outStat) {
			return errors.New("--output must not be the same file as --input")
		}
		if !o.force {
			return fmt.Errorf("output archive %q already exists (use --force to replace it)", o.output)
		}
		if !outStat.Mode().IsRegular() {
			return errors.New("output must be a regular file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("checking output archive: %w", err)
	}
	inAbs, _ := filepath.Abs(o.input)
	outAbs, _ := filepath.Abs(o.output)
	if filepath.Clean(inAbs) == filepath.Clean(outAbs) {
		return errors.New("--output must differ from --input")
	}
	if info, err := os.Stat(filepath.Dir(o.output)); err != nil || !info.IsDir() {
		return fmt.Errorf("output directory does not exist: %q", filepath.Dir(o.output))
	}
	if o.source != "" {
		src, err := filepath.Abs(o.source)
		if err != nil {
			return err
		}
		src, err = filepath.EvalSymlinks(src)
		if err != nil {
			return fmt.Errorf("opening source directory: %w", err)
		}
		info, err := os.Lstat(o.source)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("--source must be an ordinary directory, not a file or symlink")
		}
		resolvedOutDir, resolveErr := filepath.EvalSymlinks(filepath.Dir(outAbs))
		if resolveErr == nil {
			outAbs = filepath.Join(resolvedOutDir, filepath.Base(outAbs))
		}
		rel, err := filepath.Rel(src, outAbs)
		if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))) {
			return errors.New("--output must not be inside --source; that would package the output image into itself")
		}
	} else {
		st, err := os.Stat(o.layer)
		if err != nil {
			return fmt.Errorf("opening layer: %w", err)
		}
		if !st.Mode().IsRegular() {
			return errors.New("layer must be an uncompressed regular .tar file")
		}
		if os.SameFile(inStat, st) {
			return errors.New("--layer must be a filesystem content tar, not the input container archive")
		}
		if outStat, err := os.Stat(o.output); err == nil && os.SameFile(outStat, st) {
			return errors.New("--output must not overwrite the --layer input")
		}
	}
	return nil
}

func validateDest(dest string) error {
	if dest == "" || !strings.HasPrefix(dest, "/") {
		return errors.New("--dest must be an absolute POSIX container path, such as /stratum/addons/qemu/customvm")
	}
	if strings.ContainsAny(dest, "\\\x00\n\r") || path.Clean(dest) != dest {
		return fmt.Errorf("--dest %q must be normalized (no .., duplicate slashes or trailing slash)", dest)
	}
	for _, seg := range strings.Split(dest, "/") {
		if seg == ".." || seg == "." {
			return errors.New("--dest cannot contain . or ..")
		}
	}
	return nil
}

var tagFormat = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*:[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)

func validateTag(tag string) error {
	if tag == "" {
		return nil
	}
	if !tagFormat.MatchString(tag) || strings.Contains(tag, "//") {
		return fmt.Errorf("invalid --tag %q; use a Docker image name and tag, e.g. stratum:custom", tag)
	}
	lastSlash := strings.LastIndex(tag, "/")
	if !strings.Contains(tag[lastSlash+1:], ":") {
		return fmt.Errorf("--tag %q must include an explicit :tag suffix", tag)
	}
	return nil
}

func readImageMeta(input string) (imageMeta, error) {
	var m imageMeta
	f, err := os.Open(input)
	if err != nil {
		return m, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var manifestRaw []byte
	candidates := make(map[string][]byte)
	var cached int64
	paths := make(map[string]bool)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return m, fmt.Errorf("reading source image tar: %w", err)
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if paths[name] {
			return m, fmt.Errorf("duplicate tar member %q in input archive", name)
		}
		paths[name] = true
		if name == "manifest.json" {
			if hdr.Size > maxMetadata {
				return m, errors.New("source manifest.json is unexpectedly large")
			}
			manifestRaw, err = io.ReadAll(io.LimitReader(tr, maxMetadata+1))
			if err != nil {
				return m, err
			}
			continue
		}
		// Image configuration is normally a small *.json member, but some
		// Docker-save tools place it in a blobs/sha256/... directory.
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			if hdr.Size >= 0 && hdr.Size <= maxMetadata && cached+hdr.Size <= maxMetadata &&
				(strings.HasSuffix(name, ".json") || strings.HasPrefix(name, "blobs/sha256/") || strings.HasPrefix(name, "sha256:")) {
				data, err := io.ReadAll(io.LimitReader(tr, maxMetadata+1))
				if err != nil {
					return m, err
				}
				candidates[name] = data
				cached += int64(len(data))
			}
		}
	}
	// OCI + Docker hybrid archives carry BOTH index.json and oci-layout in
	// addition to Docker's manifest.json. Reject half-formed hybrid metadata.
	if paths["index.json"] != paths["oci-layout"] {
		return m, errors.New("incomplete OCI+Docker hybrid: both index.json and oci-layout are required")
	}
	if len(manifestRaw) == 0 {
		return m, errors.New("not a supported Docker-save image archive (manifest.json missing); OCI image indexes are not supported")
	}
	var imageEntries []imageEntry
	if err := json.Unmarshal(manifestRaw, &imageEntries); err != nil {
		return m, fmt.Errorf("parsing Docker image manifest.json: %w", err)
	}
	if len(imageEntries) != 1 {
		return m, fmt.Errorf("expected one image in input archive; found %d (multi-image/index archives are not supported)", len(imageEntries))
	}
	m.entry = imageEntries[0]
	var rawEntries []map[string]json.RawMessage
	if err := json.Unmarshal(manifestRaw, &rawEntries); err != nil {
		return m, err
	}
	m.manifestFields = rawEntries[0]
	m.paths = paths
	configRaw, ok := candidates[path.Clean(m.entry.Config)]
	if !ok {
		// Archive producers may use arbitrary config filenames. A targeted
		// second pass supports them without caching whole base layers in RAM.
		configRaw, err = readSmallMember(input, m.entry.Config, maxMetadata)
		if err != nil {
			return m, fmt.Errorf("reading source config %q: %w", m.entry.Config, err)
		}
	}
	if err := json.Unmarshal(configRaw, &m.config); err != nil {
		return m, fmt.Errorf("parsing source image configuration: %w", err)
	}
	if err := json.Unmarshal(m.config["architecture"], &m.arch); err != nil {
		return m, errors.New("image architecture is missing or invalid")
	}
	if err := json.Unmarshal(m.config["os"], &m.os); err != nil {
		return m, errors.New("image OS is missing or invalid")
	}
	if m.os != "linux" {
		return m, fmt.Errorf("unsupported base image operating system %q; STRATUM Container Appender currently supports Linux images", m.os)
	}
	root := make(map[string]json.RawMessage)
	if err := json.Unmarshal(m.config["rootfs"], &root); err != nil {
		return m, errors.New("source image config rootfs missing or invalid")
	}
	var typ string
	if err := json.Unmarshal(root["type"], &typ); err != nil || typ != "layers" {
		return m, errors.New("source image rootfs is not of type layers")
	}
	if err := json.Unmarshal(root["diff_ids"], &m.diffIDs); err != nil {
		return m, errors.New("source image has invalid rootfs.diff_ids")
	}
	if len(m.entry.Layers) != len(m.diffIDs) {
		return m, fmt.Errorf("source archive inconsistent: manifest contains %d layers but rootfs has %d diff IDs", len(m.entry.Layers), len(m.diffIDs))
	}
	for _, n := range m.entry.Layers {
		if !paths[path.Clean(n)] {
			return m, fmt.Errorf("source layer %q missing from input tar", n)
		}
	}
	if paths["index.json"] {
		m.hybrid, err = readHybridImage(input, m)
		if err != nil {
			return m, fmt.Errorf("reading OCI+Docker hybrid image: %w", err)
		}
	}
	return m, nil
}

// readSmallMember is a bounded fallback for Docker archives with config
// filenames that do not match the standard common naming conventions.
func readSmallMember(archivePath, member string, limit int64) ([]byte, error) {
	f, err := os.Open(archivePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	wanted := path.Clean(member)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if path.Clean(strings.TrimPrefix(hdr.Name, "./")) != wanted {
			continue
		}
		if hdr.Size > limit {
			return nil, fmt.Errorf("image config exceeds %d MiB", limit/(1024*1024))
		}
		return io.ReadAll(io.LimitReader(tr, limit+1))
	}
	return nil, errors.New("image config not present in archive")
}

func printInspection(w io.Writer, input string, m imageMeta) {
	tags := strings.Join(m.entry.RepoTags, ", ")
	if tags == "" {
		tags = "(untagged; --tag is required)"
	}
	fmt.Fprintf(w, "Archive       %s\n", input)
	fmt.Fprintf(w, "Image tags    %s\n", tags)
	fmt.Fprintf(w, "Platform      %s/%s\n", m.os, m.arch)
	fmt.Fprintf(w, "Layers        %d\n", len(m.entry.Layers))
	fmt.Fprintf(w, "Config        %s\n", m.entry.Config)
	if m.hybrid != nil {
		fmt.Fprintln(w, "Format        OCI + Docker hybrid (single image)")
	} else {
		fmt.Fprintln(w, "Format        Docker-save (single image)")
	}
}

func contains(slice []string, x string) bool {
	for _, v := range slice {
		if v == x {
			return true
		}
	}
	return false
}

func validateLayer(layerPath string) (layerInfo, error) {
	var info layerInfo
	f, err := os.Open(layerPath)
	if err != nil {
		return info, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return info, err
	}
	info.path = layerPath
	info.bytes = stat.Size()
	hash := sha256.New()
	tr := tar.NewReader(io.TeeReader(f, hash))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return info, fmt.Errorf("invalid uncompressed tar: %w", err)
		}
		if err := validateLayerEntry(hdr); err != nil {
			return info, err
		}
		info.entries++
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeRegA {
			info.files++
		}
	}
	// A tar archive may have valid trailing zero padding. Hash all bytes,
	// not just the records returned by tar.Reader.
	if _, err := io.Copy(hash, f); err != nil {
		return info, err
	}
	if info.entries == 0 {
		return info, errors.New("content layer is empty")
	}
	info.diffID = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return info, nil
}

func validateLayerEntry(h *tar.Header) error {
	name := h.Name
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("unsafe layer entry name %q: must be relative POSIX path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return fmt.Errorf("unsafe layer entry %q: .. is forbidden", name)
		}
	}
	// Docker layers technically support whiteouts and symlinks. This utility
	// restricts customer content to ordinary files and directories by design.
	switch h.Typeflag {
	case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
		return nil
	default:
		return fmt.Errorf("unsupported layer member %q (type %d): use regular files and directories only", name, h.Typeflag)
	}
}

type byteCounter struct {
	w io.Writer
	n int64
}

func (b *byteCounter) Write(p []byte) (int, error) {
	n, err := b.w.Write(p)
	b.n += int64(n)
	return n, err
}

func packDirectory(dir, dest string, writer io.Writer) (layerInfo, error) {
	var layer layerInfo
	h := sha256.New()
	counted := &byteCounter{w: writer}
	tw := tar.NewWriter(io.MultiWriter(counted, h))
	root, err := filepath.Abs(dir)
	if err != nil {
		return layer, err
	}
	err = filepath.WalkDir(root, func(filePath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return fmt.Errorf("source contains unsupported file %q (only ordinary files and directories; no symlinks)", filePath)
		}
		rel, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		name := path.Join(strings.TrimPrefix(dest, "/"), filepath.ToSlash(rel))
		if rel == "." && dest == "/" {
			return nil
		}
		hdr, err := tar.FileInfoHeader(fi, "")
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Uid, hdr.Gid = 0, 0
		hdr.Uname, hdr.Gname = "root", "root"
		// Ensure deterministic ownership while retaining caller's permissions,
		// timestamps and byte-for-byte file content.
		hdr.Format = tar.FormatPAX
		if fi.IsDir() {
			if !strings.HasSuffix(hdr.Name, "/") {
				hdr.Name += "/"
			}
		} else {
			layer.files++
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("writing tar header for %q: %w", filePath, err)
		}
		layer.entries++
		if fi.Mode().IsRegular() {
			f, err := os.Open(filePath)
			if err != nil {
				return err
			}
			n, copyErr := io.Copy(tw, f)
			closeErr := f.Close()
			if copyErr != nil {
				return fmt.Errorf("reading %q: %w", filePath, copyErr)
			}
			if closeErr != nil {
				return closeErr
			}
			if n != fi.Size() {
				return fmt.Errorf("source file %q changed during packaging", filePath)
			}
		}
		return nil
	})
	if err != nil {
		return layer, err
	}
	if layer.files == 0 {
		return layer, errors.New("source directory contains no regular files")
	}
	if err := tw.Close(); err != nil {
		return layer, err
	}
	layer.diffID = "sha256:" + hex.EncodeToString(h.Sum(nil))
	// The dry-run path writes only to io.Discard but still measures tar bytes.
	layer.bytes = counted.n
	return layer, nil
}

func newConfig(m imageMeta, diffID string, now time.Time) ([]byte, string, error) {
	root := make(map[string]json.RawMessage)
	if err := json.Unmarshal(m.config["rootfs"], &root); err != nil {
		return nil, "", err
	}
	ids := append(append([]string(nil), m.diffIDs...), diffID)
	idsRaw, err := json.Marshal(ids)
	if err != nil {
		return nil, "", err
	}
	root["diff_ids"] = idsRaw
	rootRaw, err := json.Marshal(root)
	if err != nil {
		return nil, "", err
	}
	// Duplicate the map: never mutate source config in memory.
	updated := make(map[string]json.RawMessage, len(m.config)+2)
	for k, v := range m.config {
		updated[k] = v
	}
	updated["rootfs"] = rootRaw
	nowRaw, _ := json.Marshal(now.Format(time.RFC3339Nano))
	updated["created"] = nowRaw
	var history []json.RawMessage
	if len(m.config["history"]) != 0 && string(m.config["history"]) != "null" {
		if err := json.Unmarshal(m.config["history"], &history); err != nil {
			return nil, "", fmt.Errorf("parsing image history: %w", err)
		}
	}
	historyEntry, _ := json.Marshal(map[string]string{
		"created":    now.Format(time.RFC3339Nano),
		"created_by": "STRATUM Container Appender: add offline content layer",
		"comment":    "Customer-supplied image content",
	})
	history = append(history, historyEntry)
	historyRaw, err := json.Marshal(history)
	if err != nil {
		return nil, "", err
	}
	updated["history"] = historyRaw
	data, err := json.Marshal(updated)
	if err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]) + ".json", nil
}

func writeUpdatedImage(o options, m imageMeta, layer layerInfo, layerTarPath string, tags []string) (string, error) {
	now := time.Now().UTC()
	cfgRaw, cfgName, err := newConfig(m, layer.diffID, now)
	if err != nil {
		return "", fmt.Errorf("updating image config: %w", err)
	}
	if m.paths[cfgName] && cfgName != m.entry.Config {
		return "", fmt.Errorf("new image config filename %q already exists in input tar", cfgName)
	}
	newFields := make(map[string]json.RawMessage, len(m.manifestFields))
	for k, v := range m.manifestFields {
		newFields[k] = v
	}
	newFields["Config"], _ = json.Marshal(cfgName)
	newFields["RepoTags"], _ = json.Marshal(tags)
	newFields["Layers"], _ = json.Marshal(append(append([]string(nil), m.entry.Layers...), layerTarPath))
	manifestRaw, err := json.Marshal([]map[string]json.RawMessage{newFields})
	if err != nil {
		return "", err
	}
	out, err := os.CreateTemp(filepath.Dir(o.output), ".stratum-container-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating output: %w", err)
	}
	tempName := out.Name()
	defer os.Remove(tempName)
	// Archives can contain credentials and private VM disks. Keep access limited.
	if err := out.Chmod(0600); err != nil {
		out.Close()
		return "", err
	}
	archiveHash := sha256.New()
	tw := tar.NewWriter(io.MultiWriter(out, archiveHash))
	closeWithErr := func(inErr error) (string, error) {
		if err := out.Close(); inErr == nil {
			inErr = err
		}
		return "", inErr
	}
	in, err := os.Open(o.input)
	if err != nil {
		return closeWithErr(err)
	}
	tr := tar.NewReader(in)
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			in.Close()
			return closeWithErr(fmt.Errorf("reading source image: %w", nextErr))
		}
		n := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		// Replace manifest and config. Stale repositories metadata can point
		// at a superseded image ID; manifest.json RepoTags is authoritative.
		if n == "manifest.json" || n == "repositories" || n == path.Clean(m.entry.Config) {
			continue
		}
		if err := tw.WriteHeader(hdr); err != nil {
			in.Close()
			return closeWithErr(fmt.Errorf("copying tar header %q: %w", n, err))
		}
		if _, err := io.CopyN(tw, tr, hdr.Size); err != nil {
			in.Close()
			return closeWithErr(fmt.Errorf("copying %q: %w", n, err))
		}
	}
	if err := in.Close(); err != nil {
		return closeWithErr(err)
	}
	if err := addBytes(tw, cfgName, cfgRaw, now); err != nil {
		return closeWithErr(err)
	}
	layerFile, err := os.Open(layer.path)
	if err != nil {
		return closeWithErr(fmt.Errorf("opening prepared layer: %w", err))
	}
	hdr := &tar.Header{Name: layerTarPath, Mode: 0644, Size: layer.bytes, Typeflag: tar.TypeReg, ModTime: now}
	if err := tw.WriteHeader(hdr); err != nil {
		layerFile.Close()
		return closeWithErr(err)
	}
	if _, err := io.CopyN(tw, layerFile, layer.bytes); err != nil {
		layerFile.Close()
		return closeWithErr(fmt.Errorf("copying content layer: %w", err))
	}
	if err := layerFile.Close(); err != nil {
		return closeWithErr(err)
	}
	if err := addBytes(tw, "manifest.json", manifestRaw, now); err != nil {
		return closeWithErr(err)
	}
	if err := tw.Close(); err != nil {
		return closeWithErr(err)
	}
	if err := out.Sync(); err != nil {
		return closeWithErr(err)
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	// Same-directory temporary file ensures atomic publication. The normal
	// case uses hard-link creation to implement no-clobber atomically.
	if o.force {
		if err := os.Rename(tempName, o.output); err != nil {
			return "", fmt.Errorf("publishing output: %w", err)
		}
	} else {
		if err := os.Link(tempName, o.output); err != nil {
			return "", fmt.Errorf("publishing output (already exists? use --force): %w", err)
		}
	}
	return hex.EncodeToString(archiveHash.Sum(nil)), nil
}

func addBytes(tw *tar.Writer, filename string, data []byte, modtime time.Time) error {
	hdr := &tar.Header{Name: filename, Size: int64(len(data)), Mode: 0644, Typeflag: tar.TypeReg, ModTime: modtime}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err := tw.Write(data)
	return err
}

func humanBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	u := []string{"KiB", "MiB", "GiB", "TiB"}
	f := float64(n)
	for _, unit := range u {
		f /= 1024
		if f < 1024 {
			return fmt.Sprintf("%.1f %s", f, unit)
		}
	}
	return fmt.Sprintf("%.1f PiB", f/1024)
}
