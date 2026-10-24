package main

// OCI + Docker hybrid archive support.
//
// Existing layer members are copied verbatim. The new content layer is gzip
// compressed exactly once, and both the OCI descriptor graph and Docker's
// manifest.json are updated to refer to the new immutable blobs. This avoids
// inferring layer compression from the first layer (a mixed-layer image can
// legitimately contain both uncompressed tar and gzip-compressed tar layers).

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maxOCIMetadata int64 = 32 << 20

const (
	ociManifestType    = "application/vnd.oci.image.manifest.v1+json"
	dockerManifestType = "application/vnd.docker.distribution.manifest.v2+json"
	ociIndexType       = "application/vnd.oci.image.index.v1+json"
	dockerIndexType    = "application/vnd.docker.distribution.manifest.list.v2+json"
)

type rawObject map[string]json.RawMessage

type hybridImage struct {
	blobs          map[string][]byte // JSON config, manifests, indexes, content addressed by digest
	index          []byte            // top-level index.json
	manifestDigest string            // selected OCI image manifest digest
}

func jsonObject(data []byte) (rawObject, error) {
	var obj rawObject
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, errors.New("expected JSON object")
	}
	return obj, nil
}

func jsonString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func jsonObjects(raw json.RawMessage) ([]rawObject, error) {
	var objs []rawObject
	err := json.Unmarshal(raw, &objs)
	return objs, err
}

func jsonStrings(raw json.RawMessage) ([]string, error) {
	var values []string
	err := json.Unmarshal(raw, &values)
	return values, err
}

func putJSON(obj rawObject, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	obj[key] = encoded
	return nil
}

func sha256Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func ociBlobName(digest string) (string, error) {
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != 71 {
		return "", fmt.Errorf("invalid OCI digest %q", digest)
	}
	for _, r := range digest[len("sha256:"):] {
		if r < '0' || (r > '9' && r < 'a') || r > 'f' {
			return "", fmt.Errorf("invalid OCI digest %q", digest)
		}
	}
	return "blobs/sha256/" + digest[len("sha256:"):], nil
}

func checkedBlobName(digest string) string {
	name, _ := ociBlobName(digest)
	return name
}

func referencedJSONBlob(h *hybridImage, digest string) ([]byte, error) {
	name, err := ociBlobName(digest)
	if err != nil {
		return nil, err
	}
	contents, ok := h.blobs[name]
	if !ok {
		return nil, fmt.Errorf("OCI metadata %s is missing or too large", name)
	}
	if sha256Digest(contents) != digest {
		return nil, fmt.Errorf("OCI metadata hash mismatch for %s", name)
	}
	return contents, nil
}

// Load only small JSON metadata. Never load the customer's VM disk, or any
// existing large binary layer, into memory.
func readHybridImage(archive string, m imageMeta) (*hybridImage, error) {
	h := &hybridImage{blobs: make(map[string][]byte)}
	f, err := os.Open(archive)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		header, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, fmt.Errorf("reading OCI archive metadata: %w", nextErr)
		}
		name := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if name == "index.json" {
			if header.Size < 1 || header.Size > maxOCIMetadata {
				return nil, errors.New("invalid or oversized OCI index.json")
			}
			h.index, err = io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			continue
		}
		if name == "oci-layout" {
			if header.Size < 1 || header.Size > maxOCIMetadata {
				return nil, errors.New("invalid OCI layout metadata")
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, err
			}
			var layout map[string]string
			if err := json.Unmarshal(data, &layout); err != nil || layout["imageLayoutVersion"] != "1.0.0" {
				return nil, errors.New("invalid OCI image layout version")
			}
			continue
		}
		if !strings.HasPrefix(name, "blobs/sha256/") || header.Size < 2 || header.Size > maxOCIMetadata {
			continue
		}
		// Most layers begin with a gzip magic header, or a TAR filename.
		// Only cache blobs that appear to contain JSON objects.
		head := make([]byte, min(int64(96), header.Size))
		if _, err := io.ReadFull(tr, head); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(strings.TrimSpace(string(head)), "{") {
			continue
		}
		rest, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		raw := append(head, rest...)
		if json.Valid(raw) {
			h.blobs[name] = raw
		}
	}
	if len(h.index) == 0 {
		return nil, errors.New("hybrid archive is missing index.json")
	}
	index, err := jsonObject(h.index)
	if err != nil {
		return nil, fmt.Errorf("parsing index.json: %w", err)
	}
	var schema int
	if err := json.Unmarshal(index["schemaVersion"], &schema); err != nil || schema != 2 {
		return nil, errors.New("OCI index schemaVersion must be 2")
	}
	cfgPath := path.Clean(m.entry.Config)
	cfgDigest := "sha256:" + strings.TrimPrefix(cfgPath, "blobs/sha256/")
	expectedCfgPath, err := ociBlobName(cfgDigest)
	if err != nil || cfgPath != expectedCfgPath {
		return nil, fmt.Errorf("hybrid archive config is not content-addressed: %q", cfgPath)
	}
	configRaw, err := referencedJSONBlob(h, cfgDigest)
	if err != nil {
		return nil, err
	}
	// The referenced OCI config must be the same JSON document used by
	// Docker's manifest.json; readImageMeta has parsed that same path.
	_ = configRaw

	// Select the OCI manifest describing the same image and identical sequence
	// of existing layer blob digests as Docker manifest.json.
	var matches []string
	for filename, bytes := range h.blobs {
		if !strings.HasPrefix(filename, "blobs/sha256/") {
			continue
		}
		manifest, err := jsonObject(bytes)
		if err != nil {
			continue
		}
		desc, err := jsonObject(manifest["config"])
		if err != nil || jsonString(desc["digest"]) != cfgDigest {
			continue
		}
		layers, err := jsonObjects(manifest["layers"])
		if err != nil || len(layers) != len(m.entry.Layers) {
			continue
		}
		equal := true
		for i, entry := range layers {
			lp, err := ociBlobName(jsonString(entry["digest"]))
			if err != nil || lp != path.Clean(m.entry.Layers[i]) {
				equal = false
				break
			}
			// Require a known TAR media type rather than silently retagging
			// data whose encoding we cannot understand.
			switch jsonString(entry["mediaType"]) {
			case "application/vnd.oci.image.layer.v1.tar", "application/vnd.oci.image.layer.v1.tar+gzip",
				"application/vnd.docker.image.rootfs.diff.tar", "application/vnd.docker.image.rootfs.diff.tar.gzip":
			default:
				equal = false
			}
			if !equal {
				break
			}
		}
		if !equal {
			continue
		}
		digest := "sha256:" + strings.TrimPrefix(filename, "blobs/sha256/")
		if sha256Digest(bytes) != digest {
			return nil, fmt.Errorf("OCI manifest digest mismatch: %s", filename)
		}
		matches = append(matches, digest)
	}
	for _, candidate := range matches {
		found, err := indexHasManifest(h, h.index, candidate, 0, make(map[string]bool))
		if err != nil {
			return nil, err
		}
		if found {
			if h.manifestDigest != "" {
				return nil, errors.New("multiple matching OCI image manifests; unsupported ambiguity")
			}
			h.manifestDigest = candidate
		}
	}
	if h.manifestDigest == "" {
		return nil, errors.New("cannot match Docker manifest image to an OCI image manifest referenced by index.json")
	}
	return h, nil
}

func isIndexDescriptor(mediaType string) bool {
	return mediaType == ociIndexType || mediaType == dockerIndexType
}

func indexHasManifest(h *hybridImage, data []byte, digest string, depth int, visiting map[string]bool) (bool, error) {
	if depth > 8 {
		return false, errors.New("OCI nested index depth exceeds 8")
	}
	obj, err := jsonObject(data)
	if err != nil {
		return false, err
	}
	descs, err := jsonObjects(obj["manifests"])
	if err != nil {
		return false, err
	}
	found := false
	for _, desc := range descs {
		d := jsonString(desc["digest"])
		if d == digest {
			found = true
			continue
		}
		if !isIndexDescriptor(jsonString(desc["mediaType"])) {
			continue
		}
		if visiting[d] {
			return false, errors.New("OCI index contains a cycle")
		}
		visiting[d] = true
		child, err := referencedJSONBlob(h, d)
		if err != nil {
			return false, err
		}
		sub, err := indexHasManifest(h, child, digest, depth+1, visiting)
		delete(visiting, d)
		if err != nil {
			return false, err
		}
		if sub {
			found = true
		}
	}
	return found, nil
}

func editOCIDescriptorAnnotations(desc rawObject, changedTag string) error {
	if changedTag == "" {
		return nil
	}
	var annotations map[string]string
	if raw, ok := desc["annotations"]; ok {
		if err := json.Unmarshal(raw, &annotations); err != nil {
			return fmt.Errorf("parsing OCI tag annotations: %w", err)
		}
	}
	if annotations == nil {
		annotations = make(map[string]string)
	}
	if _, exists := annotations["org.opencontainers.image.ref.name"]; exists {
		annotations["org.opencontainers.image.ref.name"] = changedTag
	}
	if _, exists := annotations["io.containerd.image.name"]; exists {
		annotations["io.containerd.image.name"] = changedTag
	}
	if len(annotations) != 0 {
		return putJSON(desc, "annotations", annotations)
	}
	return nil
}

// Update references all the way through nested OCI indexes. The old blobs
// remain in the output but become unreferenced by the updated image.
func rewriteOCIIndex(h *hybridImage, contents []byte, oldDigest, newDigest string,
	newSize int, renamedTag string, newBlobs map[string][]byte,
	depth int, visiting map[string]bool) ([]byte, bool, error) {
	if depth > 8 {
		return nil, false, errors.New("OCI nested index depth exceeds 8")
	}
	obj, err := jsonObject(contents)
	if err != nil {
		return nil, false, err
	}
	descriptors, err := jsonObjects(obj["manifests"])
	if err != nil {
		return nil, false, err
	}
	changed := false
	for _, desc := range descriptors {
		d := jsonString(desc["digest"])
		if d == oldDigest {
			if err := putJSON(desc, "digest", newDigest); err != nil {
				return nil, false, err
			}
			if err := putJSON(desc, "size", newSize); err != nil {
				return nil, false, err
			}
			if err := editOCIDescriptorAnnotations(desc, renamedTag); err != nil {
				return nil, false, err
			}
			changed = true
			continue
		}
		if !isIndexDescriptor(jsonString(desc["mediaType"])) {
			continue
		}
		if visiting[d] {
			return nil, false, errors.New("OCI index contains a cycle")
		}
		visiting[d] = true
		child, err := referencedJSONBlob(h, d)
		if err != nil {
			return nil, false, err
		}
		rewritten, altered, err := rewriteOCIIndex(h, child, oldDigest, newDigest, newSize,
			renamedTag, newBlobs, depth+1, visiting)
		delete(visiting, d)
		if err != nil {
			return nil, false, err
		}
		if !altered {
			continue
		}
		hash := sha256Digest(rewritten)
		newBlobs[checkedBlobName(hash)] = rewritten
		if err := putJSON(desc, "digest", hash); err != nil {
			return nil, false, err
		}
		if err := putJSON(desc, "size", len(rewritten)); err != nil {
			return nil, false, err
		}
		// A descriptor for a nested index can itself carry a tag. Keep it
		// synchronized with the leaf descriptor and Docker manifest.
		if err := editOCIDescriptorAnnotations(desc, renamedTag); err != nil {
			return nil, false, err
		}
		changed = true
	}
	if !changed {
		return contents, false, nil
	}
	if err := putJSON(obj, "manifests", descriptors); err != nil {
		return nil, false, err
	}
	bytes, err := json.Marshal(obj)
	return bytes, true, err
}

func updateOCIImageManifest(contents []byte, configDigest string, configSize int, layerDigest string, layerSize int64) ([]byte, error) {
	obj, err := jsonObject(contents)
	if err != nil {
		return nil, err
	}
	config, err := jsonObject(obj["config"])
	if err != nil {
		return nil, fmt.Errorf("OCI manifest config descriptor: %w", err)
	}
	if err := putJSON(config, "digest", configDigest); err != nil {
		return nil, err
	}
	if err := putJSON(config, "size", configSize); err != nil {
		return nil, err
	}
	if err := putJSON(obj, "config", config); err != nil {
		return nil, err
	}
	layers, err := jsonObjects(obj["layers"])
	if err != nil {
		return nil, err
	}
	layerType := "application/vnd.oci.image.layer.v1.tar+gzip"
	switch jsonString(obj["mediaType"]) {
	case dockerManifestType:
		layerType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	case ociManifestType, "":
		// Some producers omit the optional mediaType field in the manifest.
	default:
		return nil, fmt.Errorf("unsupported image manifest type %q", jsonString(obj["mediaType"]))
	}
	layers = append(layers, rawObject{
		"mediaType": json.RawMessage(fmt.Sprintf("%q", layerType)),
		"digest":    json.RawMessage(fmt.Sprintf("%q", layerDigest)),
		"size":      json.RawMessage(fmt.Sprintf("%d", layerSize)),
	})
	if err := putJSON(obj, "layers", layers); err != nil {
		return nil, err
	}
	return json.Marshal(obj)
}

// Only the newly appended layer is compressed, while a hash of both the raw
// TAR and stored gzip bytes provides their respective OCI digests.
func gzipNewLayer(layer layerInfo, outputDir string) (path string, digest string, size int64, cleanup func(), err error) {
	in, err := os.Open(layer.path)
	if err != nil {
		return "", "", 0, nil, err
	}
	defer in.Close()
	out, err := os.CreateTemp(outputDir, ".stratum-gzip-layer-*.tmp")
	if err != nil {
		return "", "", 0, nil, err
	}
	filename := out.Name()
	cleanup = func() { _ = os.Remove(filename) }
	defer out.Close()
	rawHash := sha256.New()
	compressedHash := sha256.New()
	gw, err := gzip.NewWriterLevel(io.MultiWriter(out, compressedHash), gzip.BestSpeed)
	if err != nil {
		cleanup()
		return "", "", 0, nil, err
	}
	copied, copyErr := io.Copy(gw, io.TeeReader(in, rawHash))
	closeErr := gw.Close()
	if copyErr != nil {
		cleanup()
		return "", "", 0, nil, copyErr
	}
	if closeErr != nil {
		cleanup()
		return "", "", 0, nil, closeErr
	}
	if copied != layer.bytes || "sha256:"+hex.EncodeToString(rawHash.Sum(nil)) != layer.diffID {
		cleanup()
		return "", "", 0, nil, errors.New("content layer changed after validation; refusing to write an inconsistent image")
	}
	if err := out.Sync(); err != nil {
		cleanup()
		return "", "", 0, nil, err
	}
	st, err := out.Stat()
	if err != nil {
		cleanup()
		return "", "", 0, nil, err
	}
	return filename, "sha256:" + hex.EncodeToString(compressedHash.Sum(nil)), st.Size(), cleanup, nil
}

// writeUpdatedHybrid leaves all original layer bytes untouched, replacing only
// root manifest/index entries, and adding the new content-addressed blobs.
func writeUpdatedHybrid(o options, m imageMeta, layer layerInfo, tags []string) (string, error) {
	h := m.hybrid
	if h == nil {
		return "", errors.New("internal: missing OCI hybrid metadata")
	}
	now := time.Now().UTC()
	newCfg, _, err := newConfig(m, layer.diffID, now)
	if err != nil {
		return "", err
	}
	newCfgDigest := sha256Digest(newCfg)
	cfgName := checkedBlobName(newCfgDigest)
	if m.paths[cfgName] {
		return "", fmt.Errorf("new OCI config member unexpectedly already exists: %s", cfgName)
	}

	gzipFile, gzipDigest, gzipSize, cleanup, err := gzipNewLayer(layer, filepath.Dir(o.output))
	if err != nil {
		return "", err
	}
	defer cleanup()
	gzipName := checkedBlobName(gzipDigest)
	if m.paths[gzipName] {
		return "", fmt.Errorf("new OCI layer member unexpectedly already exists: %s", gzipName)
	}
	prevManifest, err := referencedJSONBlob(h, h.manifestDigest)
	if err != nil {
		return "", err
	}
	newManifest, err := updateOCIImageManifest(prevManifest, newCfgDigest, len(newCfg), gzipDigest, gzipSize)
	if err != nil {
		return "", err
	}
	newManifestDigest := sha256Digest(newManifest)
	manifestName := checkedBlobName(newManifestDigest)
	if m.paths[manifestName] {
		return "", fmt.Errorf("new OCI manifest unexpectedly already exists: %s", manifestName)
	}
	extra := map[string][]byte{cfgName: newCfg, manifestName: newManifest}
	renamedTag := ""
	if o.tag != "" {
		renamedTag = o.tag
	}
	newIndex, changed, err := rewriteOCIIndex(h, h.index, h.manifestDigest, newManifestDigest, len(newManifest),
		renamedTag, extra, 0, make(map[string]bool))
	if err != nil {
		return "", err
	}
	if !changed {
		return "", errors.New("OCI index did not reference selected image manifest")
	}

	// Update the Docker manifest for the same image; keep vendor-specific
	// fields as well as all existing layer filenames in their original order.
	fields := make(map[string]json.RawMessage, len(m.manifestFields))
	for key, value := range m.manifestFields {
		fields[key] = value
	}
	fields["Config"], _ = json.Marshal(cfgName)
	fields["Layers"], _ = json.Marshal(append(append([]string(nil), m.entry.Layers...), gzipName))
	fields["RepoTags"], _ = json.Marshal(tags)
	newDockerManifest, err := json.Marshal([]map[string]json.RawMessage{fields})
	if err != nil {
		return "", err
	}

	for key, content := range extra {
		if m.paths[key] {
			return "", fmt.Errorf("new OCI metadata member already exists: %s", key)
		}
		if sha256Digest(content) != "sha256:"+strings.TrimPrefix(key, "blobs/sha256/") {
			return "", fmt.Errorf("generated OCI metadata hash mismatch: %s", key)
		}
	}
	return streamUpdatedHybridArchive(o, m, newDockerManifest, newIndex, extra, gzipName, gzipFile, gzipSize)
}

func streamUpdatedHybridArchive(o options, m imageMeta, dockerManifest, index []byte,
	extra map[string][]byte, compressedName, compressedFilename string, compressedSize int64) (string, error) {
	out, err := os.CreateTemp(filepath.Dir(o.output), ".stratum-container-*.tmp")
	if err != nil {
		return "", fmt.Errorf("creating output: %w", err)
	}
	tmpName := out.Name()
	defer os.Remove(tmpName)
	if err := out.Chmod(0600); err != nil {
		out.Close()
		return "", err
	}
	archivedHash := sha256.New()
	writer := tar.NewWriter(io.MultiWriter(out, archivedHash))
	closeOnError := func(inErr error) (string, error) {
		if err := out.Close(); inErr == nil {
			inErr = err
		}
		return "", inErr
	}
	input, err := os.Open(o.input)
	if err != nil {
		return closeOnError(err)
	}
	tr := tar.NewReader(input)
	seenManifest, seenIndex := false, false
	for {
		hdr, nextErr := tr.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			input.Close()
			return closeOnError(fmt.Errorf("reading source image: %w", nextErr))
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "./"))
		if name == "repositories" {
			continue
		} // can refer to obsolete Docker config
		data := []byte(nil)
		switch name {
		case "manifest.json":
			data = dockerManifest
			seenManifest = true
		case "index.json":
			data = index
			seenIndex = true
		}
		if data == nil {
			if err := writer.WriteHeader(hdr); err != nil {
				input.Close()
				return closeOnError(err)
			}
			if _, err := io.CopyN(writer, tr, hdr.Size); err != nil {
				input.Close()
				return closeOnError(err)
			}
			continue
		}
		updatedHdr := *hdr
		updatedHdr.Size = int64(len(data))
		updatedHdr.PAXRecords = nil
		updatedHdr.Xattrs = nil
		if err := writer.WriteHeader(&updatedHdr); err != nil {
			input.Close()
			return closeOnError(err)
		}
		if _, err := writer.Write(data); err != nil {
			input.Close()
			return closeOnError(err)
		}
	}
	if err := input.Close(); err != nil {
		return closeOnError(err)
	}
	if !seenManifest || !seenIndex {
		return closeOnError(errors.New("source hybrid is missing manifest.json or index.json"))
	}
	names := make([]string, 0, len(extra))
	for name := range extra {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := addBytes(writer, name, extra[name], time.Time{}); err != nil {
			return closeOnError(err)
		}
	}
	compressedFile, err := os.Open(compressedFilename)
	if err != nil {
		return closeOnError(err)
	}
	if err := writer.WriteHeader(&tar.Header{Name: compressedName, Mode: 0644, Typeflag: tar.TypeReg, Size: compressedSize}); err != nil {
		compressedFile.Close()
		return closeOnError(err)
	}
	copied, err := io.CopyN(writer, compressedFile, compressedSize)
	closeErr := compressedFile.Close()
	if err != nil {
		return closeOnError(err)
	}
	if closeErr != nil {
		return closeOnError(closeErr)
	}
	if copied != compressedSize {
		return closeOnError(io.ErrUnexpectedEOF)
	}
	if err := writer.Close(); err != nil {
		return closeOnError(err)
	}
	if err := out.Sync(); err != nil {
		return closeOnError(err)
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if o.force {
		if err := os.Rename(tmpName, o.output); err != nil {
			return "", err
		}
	} else {
		if err := os.Link(tmpName, o.output); err != nil {
			return "", fmt.Errorf("publishing output (already exists? use --force): %w", err)
		}
	}
	return hex.EncodeToString(archivedHash.Sum(nil)), nil
}
