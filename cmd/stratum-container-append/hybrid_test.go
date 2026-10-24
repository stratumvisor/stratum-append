package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func makeJSON(t *testing.T, data any) []byte {
	t.Helper()
	out, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func compressTestGzip(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTestTarArchive(t *testing.T, file string, entries map[string][]byte) {
	t.Helper()
	out, err := os.Create(file)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(out)
	for name, data := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

type hybridFixture struct {
	filename   string
	originals  map[string][]byte
	layerNames []string
	cfg        map[string]any
	nested     bool
}

func makeHybridTestArchive(t *testing.T, nested bool) hybridFixture {
	t.Helper()
	folder := t.TempDir()
	var raws [][]byte
	for i := range 3 {
		layerFile := filepath.Join(folder, "layer-"+string(rune('A'+i))+".tar")
		raws = append(raws, testLayer(t, layerFile, map[string][]byte{
			"etc/layer" + string(rune('0'+i)) + ".txt": []byte("original data " + string(rune('0'+i))),
		}))
	}
	layerBodies := [][]byte{raws[0], compressTestGzip(t, raws[1]), compressTestGzip(t, raws[2])}
	original := make(map[string][]byte)
	names := make([]string, 0, len(raws))
	descriptors := make([]map[string]any, 0, len(raws))
	diffs := make([]string, 0, len(raws))
	for i, raw := range raws {
		data := layerBodies[i]
		digest := sha256Digest(data)
		filename := checkedBlobName(digest)
		names = append(names, filename)
		original[filename] = data
		diffs = append(diffs, sha256Digest(raw))
		mediaType := "application/vnd.oci.image.layer.v1.tar+gzip"
		if i == 0 {
			mediaType = "application/vnd.oci.image.layer.v1.tar"
		}
		descriptors = append(descriptors, map[string]any{"mediaType": mediaType, "digest": digest, "size": len(data)})
	}
	cfg := map[string]any{
		"os": "linux", "architecture": "amd64",
		"created": "2026-10-22T00:00:00Z",
		"config":  map[string]any{"User": "stratum", "Cmd": []string{"/stratum/start"}, "Env": []string{"SOMETHING=1"}},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": diffs},
		"history": []any{map[string]any{"created_by": "base layer 0"}, map[string]any{"created_by": "base layer 1"}, map[string]any{"created_by": "base layer 2"}},
	}
	cfgData := makeJSON(t, cfg)
	cfgDigest := sha256Digest(cfgData)
	original[checkedBlobName(cfgDigest)] = cfgData
	im := makeJSON(t, map[string]any{
		"schemaVersion": 2, "mediaType": ociManifestType,
		"config":      map[string]any{"mediaType": "application/vnd.oci.image.config.v1+json", "digest": cfgDigest, "size": len(cfgData)},
		"layers":      descriptors,
		"annotations": map[string]string{"not-to-lose": "custom"},
	})
	imgDigest := sha256Digest(im)
	original[checkedBlobName(imgDigest)] = im
	targetDigest := imgDigest
	targetType := ociManifestType
	if nested {
		sub := makeJSON(t, map[string]any{
			"schemaVersion": 2, "mediaType": ociIndexType,
			"manifests": []any{map[string]any{"mediaType": ociManifestType, "digest": imgDigest, "size": len(im),
				"annotations": map[string]string{"org.opencontainers.image.ref.name": "stratum:latest"}}},
		})
		targetDigest = sha256Digest(sub)
		targetType = ociIndexType
		original[checkedBlobName(targetDigest)] = sub
	}
	index := makeJSON(t, map[string]any{
		"schemaVersion": 2,
		"manifests": []any{map[string]any{"mediaType": targetType, "digest": targetDigest, "size": len(original[checkedBlobName(targetDigest)]),
			"annotations": map[string]string{"io.containerd.image.name": "stratum:latest"}}},
	})
	manifest := makeJSON(t, []map[string]any{
		{"Config": checkedBlobName(cfgDigest), "RepoTags": []string{"stratum:latest", "stratum:old"}, "Layers": names,
			"CustomField": map[string]any{"preserve": true}},
	})
	members := make(map[string][]byte, len(original)+3)
	for k, v := range original {
		members[k] = v
	}
	members["oci-layout"] = []byte(`{"imageLayoutVersion":"1.0.0"}`)
	members["index.json"] = index
	members["manifest.json"] = manifest
	filename := filepath.Join(folder, "base-hybrid.tar")
	writeTestTarArchive(t, filename, members)
	return hybridFixture{filename: filename, originals: original, layerNames: names, cfg: cfg, nested: nested}
}

func verifyHybridTestResult(t *testing.T, fixture hybridFixture, output, tag, expectedFile, expectedContent string) {
	t.Helper()
	src := entries(t, output)
	for name, data := range fixture.originals {
		if !bytes.Equal(src[name], data) {
			t.Fatalf("original member %s changed", name)
		}
	}
	var dock []imageEntry
	if err := json.Unmarshal(src["manifest.json"], &dock); err != nil {
		t.Fatal(err)
	}
	if len(dock) != 1 || len(dock[0].Layers) != 4 {
		t.Fatalf("wrong output layer count: %+v", dock)
	}
	if tag != "" && !reflect.DeepEqual(dock[0].RepoTags, []string{tag}) {
		t.Fatalf("wrong new tag: %+v", dock[0].RepoTags)
	}
	if tag == "" && !reflect.DeepEqual(dock[0].RepoTags, []string{"stratum:latest", "stratum:old"}) {
		t.Fatalf("multiple existing tags not preserved: %+v", dock[0].RepoTags)
	}
	for i, name := range fixture.layerNames {
		if dock[0].Layers[i] != name {
			t.Fatalf("existing layer %d path changed: %s", i, dock[0].Layers[i])
		}
	}
	newLayerPath := dock[0].Layers[3]
	newCompressed := src[newLayerPath]
	if !strings.HasPrefix(newLayerPath, "blobs/sha256/") || sha256Digest(newCompressed) != "sha256:"+strings.TrimPrefix(newLayerPath, "blobs/sha256/") {
		t.Fatal("new OCI layer is not correctly content-addressed")
	}
	zr, err := gzip.NewReader(bytes.NewReader(newCompressed))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(raw))
	found := false
	for {
		hdr, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == expectedFile {
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != expectedContent {
				t.Fatalf("wrong appended bytes: %s", data)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("missing added path: %s", expectedFile)
	}
	cfgPath := dock[0].Config
	if !strings.HasPrefix(cfgPath, "blobs/sha256/") {
		t.Fatalf("config path is not OCI-addressable: %s", cfgPath)
	}
	var newCfg map[string]any
	if err := json.Unmarshal(src[cfgPath], &newCfg); err != nil {
		t.Fatal(err)
	}
	if sha256Digest(src[cfgPath]) != "sha256:"+strings.TrimPrefix(cfgPath, "blobs/sha256/") {
		t.Fatal("new config digest mismatched")
	}
	oldRuntime, _ := json.Marshal(fixture.cfg["config"])
	newRuntime, _ := json.Marshal(newCfg["config"])
	if !bytes.Equal(oldRuntime, newRuntime) {
		t.Fatal("container runtime configuration was not preserved")
	}
	newIDs := newCfg["rootfs"].(map[string]any)["diff_ids"].([]any)
	if len(newIDs) != 4 || newIDs[3] != sha256Digest(raw) {
		t.Fatalf("wrong new diffIDs: %v", newIDs)
	}
	var index rawObject
	if err := json.Unmarshal(src["index.json"], &index); err != nil {
		t.Fatal(err)
	}
	desc, err := jsonObjects(index["manifests"])
	if err != nil || len(desc) != 1 {
		t.Fatalf("bad OCI index descriptors: %v %v", desc, err)
	}
	d := jsonString(desc[0]["digest"])
	if fixture.nested {
		nestedContents := src[checkedBlobName(d)]
		if sha256Digest(nestedContents) != d {
			t.Fatal("nested OCI index digest wrong")
		}
		nested, err := jsonObject(nestedContents)
		if err != nil {
			t.Fatal(err)
		}
		sub, err := jsonObjects(nested["manifests"])
		if err != nil || len(sub) != 1 {
			t.Fatalf("invalid nested OCI index: %v %v", sub, err)
		}
		if tag != "" {
			anns := make(map[string]string)
			if err := json.Unmarshal(sub[0]["annotations"], &anns); err != nil {
				t.Fatal(err)
			}
			if anns["org.opencontainers.image.ref.name"] != tag {
				t.Fatalf("stale nested OCI annotation: %v", anns)
			}
		}
		d = jsonString(sub[0]["digest"])
	}
	if tag != "" {
		anns := make(map[string]string)
		if err := json.Unmarshal(desc[0]["annotations"], &anns); err != nil {
			t.Fatal(err)
		}
		if anns["io.containerd.image.name"] != tag {
			t.Fatalf("root OCI image name was not updated: %+v", anns)
		}
	}
	contents := src[checkedBlobName(d)]
	if sha256Digest(contents) != d {
		t.Fatal("OCI image manifest digest does not match index")
	}
	mani, err := jsonObject(contents)
	if err != nil {
		t.Fatal(err)
	}
	cfgDesc, err := jsonObject(mani["config"])
	if err != nil || jsonString(cfgDesc["digest"]) != "sha256:"+strings.TrimPrefix(cfgPath, "blobs/sha256/") {
		t.Fatal("OCI config digest does not match Docker")
	}
	ociLayers, err := jsonObjects(mani["layers"])
	if err != nil || len(ociLayers) != 4 {
		t.Fatal("wrong OCI layer count")
	}
	for i, layer := range ociLayers {
		if checkedBlobName(jsonString(layer["digest"])) != dock[0].Layers[i] {
			t.Fatalf("OCI/Docker layer mismatch at %d", i)
		}
		if i == 3 && jsonString(layer["mediaType"]) != "application/vnd.oci.image.layer.v1.tar+gzip" {
			t.Fatalf("new layer has wrong media type: %s", layer["mediaType"])
		}
	}
}

func TestHybridMixedCompressionPreparedLayer(t *testing.T) {
	fixture := makeHybridTestArchive(t, false)
	input := fixture.filename
	output := filepath.Join(t.TempDir(), "modified.tar")
	layer := filepath.Join(t.TempDir(), "compliance.tar")
	testLayer(t, layer, map[string][]byte{"compliance/sbom.json": []byte(`{"bomFormat":"CycloneDX"}`)})
	var status bytes.Buffer
	if err := execute([]string{"--input", input, "--output", output, "--layer", layer}, &status, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.String(), "SUCCESS") {
		t.Fatal("no success status")
	}
	verifyHybridTestResult(t, fixture, output, "", "compliance/sbom.json", `{"bomFormat":"CycloneDX"}`)
	var inspected bytes.Buffer
	if err := execute([]string{"inspect", "--input", output}, &inspected, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inspected.String(), "OCI + Docker hybrid") {
		t.Fatalf("incorrect hybrid inspect: %s", inspected.String())
	}
}

func TestHybridNestedIndexAndDirectorySource(t *testing.T) {
	fixture := makeHybridTestArchive(t, true)
	source := filepath.Join(t.TempDir(), "overlay")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "vm.qcow2"), []byte("customer VM disk"), 0644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "hybrid.tar")
	if err := execute([]string{"--input", fixture.filename, "--output", output, "--source", source,
		"--dest", "/stratum/addons/qemu", "--tag", "stratum:custom"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	verifyHybridTestResult(t, fixture, output, "stratum:custom", "stratum/addons/qemu/nested/vm.qcow2", "customer VM disk")
}

func TestRejectMalformedHybridIndex(t *testing.T) {
	fixture := makeHybridTestArchive(t, false)
	items := entries(t, fixture.filename)
	delete(items, "oci-layout")
	bad := filepath.Join(t.TempDir(), "bad.tar")
	writeTestTarArchive(t, bad, items)
	layer := filepath.Join(t.TempDir(), "compliance.tar")
	testLayer(t, layer, map[string][]byte{"compliance/sbom.json": []byte("ok")})
	output := filepath.Join(t.TempDir(), "out.tar")
	err := execute([]string{"--input", bad, "--output", output, "--layer", layer}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "incomplete OCI+Docker hybrid") {
		t.Fatalf("expected malformed hybrid rejection; got %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("incomplete output was published")
	}
}

func TestHybridSHA256IsConsistent(t *testing.T) {
	fixture := makeHybridTestArchive(t, false)
	var out bytes.Buffer
	if err := execute([]string{"inspect", "--input", fixture.filename}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Layers        3") {
		t.Fatal(out.String())
	}
	// The first layer in this fixture is raw and subsequent layers are gzip.
	// This is the exact mixed-compression pattern that previously failed.
	for i, name := range fixture.layerNames {
		data := fixture.originals[name]
		compressed := len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
		if compressed != (i > 0) {
			t.Fatalf("unexpected fixture layer encoding for %d", i)
		}
		actual := sha256.Sum256(data)
		if name != "blobs/sha256/"+hex.EncodeToString(actual[:]) {
			t.Fatal("fixture digest mismatch")
		}
	}
}

func TestHybridDirectIndexTagOverride(t *testing.T) {
	fixture := makeHybridTestArchive(t, false)
	layer := filepath.Join(t.TempDir(), "content.tar")
	testLayer(t, layer, map[string][]byte{"test/new.txt": []byte("new test")})
	output := filepath.Join(t.TempDir(), "new-tag.tar")
	if err := execute([]string{"--input", fixture.filename, "--output", output, "--layer", layer, "--tag", "stratum:override"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	verifyHybridTestResult(t, fixture, output, "stratum:override", "test/new.txt", "new test")
}

// Docker-save archives may also contain differently encoded layers. Their
// bytes must be preserved even though Docker-save has no OCI descriptor graph.
func TestDockerOnlyMixedLayersArePreserved(t *testing.T) {
	fixture := makeHybridTestArchive(t, false)
	items := entries(t, fixture.filename)
	delete(items, "index.json")
	delete(items, "oci-layout")
	input := filepath.Join(t.TempDir(), "docker-save.tar")
	writeTestTarArchive(t, input, items)

	content := filepath.Join(t.TempDir(), "content.tar")
	rawContent := testLayer(t, content, map[string][]byte{"extra/config.txt": []byte("customer data")})
	output := filepath.Join(t.TempDir(), "docker-custom.tar")
	if err := execute([]string{"--input", input, "--output", output, "--layer", content}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	result := entries(t, output)
	if _, hasIndex := result["index.json"]; hasIndex {
		t.Fatal("unexpected OCI index in Docker-only output")
	}
	for _, layer := range fixture.layerNames {
		if !bytes.Equal(items[layer], result[layer]) {
			t.Fatalf("base layer was altered: %s", layer)
		}
	}
	var manifest []imageEntry
	if err := json.Unmarshal(result["manifest.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 1 || len(manifest[0].Layers) != 4 {
		t.Fatalf("bad output manifest: %v", manifest)
	}
	if !bytes.Equal(result[manifest[0].Layers[3]], rawContent) {
		t.Fatal("new Docker layer is wrong")
	}
	updated, err := readImageMeta(output)
	if err != nil {
		t.Fatal(err)
	}
	if updated.hybrid != nil || updated.diffIDs[3] != sha256Digest(rawContent) {
		t.Fatalf("invalid Docker-only updated config: %+v", updated.diffIDs)
	}
}
