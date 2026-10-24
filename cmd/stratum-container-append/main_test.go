package main

import (
	"archive/tar"
	"bytes"
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

func testLayer(t *testing.T, path string, files map[string][]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for filename, data := range files {
		if err := tw.WriteHeader(&tar.Header{Name: filename, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func buildTestImage(t *testing.T, name string, tags []string, manifestCount int, formats ...string) (string, []byte, map[string]any) {
	t.Helper()
	layerPath := filepath.Join(t.TempDir(), "base-layer.tar")
	baseLayer := testLayer(t, layerPath, map[string][]byte{"etc/os-release": []byte("STRATUM=test\n")})
	digest := sha256.Sum256(baseLayer)
	config := map[string]any{
		"os": "linux", "architecture": "amd64",
		"created": "2026-01-01T00:00:00Z",
		"config":  map[string]any{"Env": []string{"FOO=BAR"}, "Cmd": []string{"/stratum/start"}, "WorkingDir": "/stratum"},
		"rootfs":  map[string]any{"type": "layers", "diff_ids": []string{"sha256:" + hex.EncodeToString(digest[:])}},
		"history": []map[string]any{{"created_by": "initial build"}},
	}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	cfgDigest := sha256.Sum256(raw)
	configName := hex.EncodeToString(cfgDigest[:]) + ".json"
	if len(formats) > 0 && formats[0] == "crane" {
		configName = "sha256:" + hex.EncodeToString(cfgDigest[:])
	}
	layerName := "base/layer.tar"
	entries := []imageEntry{{Config: configName, Layers: []string{layerName}, RepoTags: tags}}
	if manifestCount > 1 {
		entries = append(entries, imageEntry{Config: configName, Layers: []string{layerName}, RepoTags: []string{"other:latest"}})
	}
	manifest, _ := json.Marshal(entries)
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, item := range []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifest}, {configName, raw}, {layerName, baseLayer},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: item.name, Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(item.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(item.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return name, baseLayer, config
}

func entries(t *testing.T, filename string) map[string][]byte {
	t.Helper()
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := make(map[string][]byte)
	tr := tar.NewReader(f)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		d, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		out[h.Name] = d
	}
	return out
}

func TestSourceAppendsLayerPreservesConfigurationAndTags(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "stratum-container.tar")
	original, baseBytes, oldConfig := buildTestImage(t, input, []string{"stratum:latest", "stratum:2026"}, 1)
	source := filepath.Join(temp, "sideload")
	if err := os.MkdirAll(filepath.Join(source, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "nested", "ubuntu.qcow2"), []byte("test VM disk"), 0640); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(temp, "stratum-container-custom.tar")
	var stdout, stderr bytes.Buffer
	err := execute([]string{"--input", input, "--output", output, "--source", source, "--dest", "/stratum/addons/qemu/customvm"}, &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "SUCCESS") {
		t.Fatalf("missing success banner: %s", stdout.String())
	}
	if _, err := os.Stat(original); err != nil {
		t.Fatalf("original lost: %v", err)
	}

	updated, err := readImageMeta(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.entry.Layers) != 2 {
		t.Fatalf("expected 2 layers, got %v", updated.entry.Layers)
	}
	if !reflect.DeepEqual(updated.entry.RepoTags, []string{"stratum:latest", "stratum:2026"}) {
		t.Fatalf("tags not preserved: %v", updated.entry.RepoTags)
	}
	all := entries(t, output)
	if !bytes.Equal(all["base/layer.tar"], baseBytes) {
		t.Fatal("base filesystem layer changed")
	}
	var updatedConfig map[string]any
	if err := json.Unmarshal(all[updated.entry.Config], &updatedConfig); err != nil {
		t.Fatal(err)
	}
	expectedConfig, _ := json.Marshal(oldConfig["config"])
	actualConfig, _ := json.Marshal(updatedConfig["config"])
	if !bytes.Equal(actualConfig, expectedConfig) {
		t.Fatalf("container runtime config changed: %s", actualConfig)
	}
	layerName := updated.entry.Layers[1]
	var expected bool
	tr := tar.NewReader(bytes.NewReader(all[layerName]))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "stratum/addons/qemu/customvm/nested/ubuntu.qcow2" {
			data, _ := io.ReadAll(tr)
			if string(data) != "test VM disk" {
				t.Fatalf("unexpected file contents: %s", data)
			}
			if h.Uid != 0 || h.Gid != 0 || h.Mode != 0640 {
				t.Fatalf("unexpected ownership/mode: %d %d %o", h.Uid, h.Gid, h.Mode)
			}
			expected = true
		}
	}
	if !expected {
		t.Fatal("sideloaded file was not packaged at destination")
	}
	hashed := sha256.Sum256(all[layerName])
	if updated.diffIDs[1] != "sha256:"+hex.EncodeToString(hashed[:]) {
		t.Fatalf("wrong image rootfs diff ID %s", updated.diffIDs[1])
	}
	if len(all) != 4 {
		t.Fatalf("unexpected archive members: %v", keys(all))
	}
}

func keys(m map[string][]byte) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

func TestCraneArchiveConfigFilename(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "crane.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1, "crane")
	src := filepath.Join(temp, "custom")
	if err := os.Mkdir(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "disk.qcow2"), []byte("sample"), 0644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(temp, "out.tar")
	if err := execute([]string{"--input", input, "--output", output, "--source", src, "--dest", "/stratum/addons/qemu/customvm"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	updated, err := readImageMeta(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.diffIDs) != 2 {
		t.Fatal(updated.diffIDs)
	}
}

func TestPrebuiltLayerPreservesAndChangesTag(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "base.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	layer := filepath.Join(temp, "compliance.tar")
	testLayer(t, layer, map[string][]byte{"compliance/sbom.json": []byte(`{"bomFormat":"CycloneDX"}`)})
	output := filepath.Join(temp, "output.tar")
	err := execute([]string{"--input", input, "--output", output, "--layer", layer, "--tag", "stratum:compliance", "--quiet"}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := readImageMeta(output)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.entry.RepoTags, []string{"stratum:compliance"}) {
		t.Fatal(updated.entry.RepoTags)
	}
	l := updated.entry.Layers[1]
	layerArchive := entries(t, output)[l]
	tr := tar.NewReader(bytes.NewReader(layerArchive))
	h, err := tr.Next()
	if err != nil || h.Name != "compliance/sbom.json" {
		t.Fatalf("bad layer file %v: %v", h, err)
	}
}

func TestInspectAndHelp(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "input.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	var out bytes.Buffer
	if err := execute([]string{"inspect", "--input", input}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "linux/amd64") || !strings.Contains(out.String(), "Layers        1") {
		t.Fatalf("wrong inspect output %s", out.String())
	}
	out.Reset()
	if err := execute([]string{"--help"}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "CONTAINER CONTENT APPENDER") || !strings.Contains(out.String(), "--dry-run") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := execute([]string{"--version"}, &out, io.Discard); err != nil || !strings.Contains(out.String(), version) {
		t.Fatal(err, out.String())
	}
}

func TestDryRunDoesNotWrite(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "input.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	source := filepath.Join(temp, "data")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "vm.vhdx"), []byte("file"), 0644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(temp, "output.tar")
	if err := execute([]string{"--input", input, "--output", output, "--source", source, "--dest", "/opt/stratum/sideload", "--dry-run"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("dry run created output: %v", err)
	}
}

func TestRejectBadLayer(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "input.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	layer := filepath.Join(temp, "traversal.tar")
	testLayer(t, layer, map[string][]byte{"../escape.qcow2": []byte("test")})
	err := execute([]string{"--input", input, "--output", filepath.Join(temp, "output.tar"), "--layer", layer}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("expected unsafe tar error; got %v", err)
	}
}

func TestRejectOverwriteAndMultiImage(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "input.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	source := filepath.Join(temp, "data")
	os.Mkdir(source, 0755)
	os.WriteFile(filepath.Join(source, "vm.qcow2"), []byte("file"), 0644)
	args := []string{"--input", input, "--output", input, "--source", source, "--dest", "/media"}
	if err := execute(args, io.Discard, io.Discard); err == nil {
		t.Fatal("input overwrite accepted")
	}
	multi := filepath.Join(temp, "multi.tar")
	buildTestImage(t, multi, []string{"stratum:latest"}, 2)
	args = []string{"--input", multi, "--output", filepath.Join(temp, "custom.tar"), "--source", source, "--dest", "/media"}
	err := execute(args, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "expected one image") {
		t.Fatalf("multi-image archive accepted: %v", err)
	}
}

func TestExistingOutputAndForce(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "input.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	src := filepath.Join(temp, "content")
	os.Mkdir(src, 0755)
	os.WriteFile(filepath.Join(src, "data"), []byte("abc"), 0644)
	output := filepath.Join(temp, "output.tar")
	os.WriteFile(output, []byte("DO NOT CLOBBER"), 0600)
	args := []string{"--input", input, "--output", output, "--source", src, "--dest", "/content"}
	if err := execute(args, io.Discard, io.Discard); err == nil {
		t.Fatal("overwrite without force was accepted")
	}
	orig, _ := os.ReadFile(output)
	if string(orig) != "DO NOT CLOBBER" {
		t.Fatal("existing output was modified")
	}
	if err := execute(append(args, "--force"), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := readImageMeta(output); err != nil {
		t.Fatal(err)
	}
}

func TestRejectSourceSymlink(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "input.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	src := filepath.Join(temp, "content")
	os.Mkdir(src, 0755)
	os.WriteFile(filepath.Join(src, "original.qcow2"), []byte("abc"), 0644)
	if err := os.Symlink("original.qcow2", filepath.Join(src, "linked.qcow2")); err != nil {
		t.Skip(err)
	}
	err := execute([]string{"--input", input, "--output", filepath.Join(temp, "out.tar"), "--source", src, "--dest", "/content"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unsupported file") {
		t.Fatalf("symlink accepted: %v", err)
	}
}

func TestStageLayersLargeFilesWithoutBuffering(t *testing.T) {
	// Check that a multi-megabyte source is correctly streamed, without
	// embedding the file as a JSON or Go byte slice.
	temp := t.TempDir()
	content := filepath.Join(temp, "file.img")
	f, err := os.Create(content)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(2 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	li, err := packDirectory(temp, "/stratum/addons/qemu/test", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if li.files != 1 || li.diffID == "" || li.bytes == 0 {
		t.Fatalf("unexpected layer: %+v", li)
	}
}

func TestArchiveDigestAndFormat(t *testing.T) {
	temp := t.TempDir()
	input := filepath.Join(temp, "input.tar")
	buildTestImage(t, input, []string{"stratum:latest"}, 1)
	source := filepath.Join(temp, "source")
	os.Mkdir(source, 0755)
	os.WriteFile(filepath.Join(source, "test.img"), []byte("sample"), 0644)
	output := filepath.Join(temp, "output.tar")
	var stdout bytes.Buffer
	err := execute([]string{"--input", input, "--output", output, "--source", source, "--dest", "/stratum/addons/qemu/test"}, &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if !strings.Contains(stdout.String(), hex.EncodeToString(sum[:])) {
		t.Fatal("wrong output tar checksum")
	}
	stat, _ := os.Stat(output)
	if stat.Mode().Perm() != 0600 {
		t.Fatalf("unexpected output permissions %v", stat.Mode())
	}

}

func TestPreserveUnknownManifestFields(t *testing.T) {
	temp := t.TempDir()
	original := filepath.Join(temp, "original.tar")
	buildTestImage(t, original, []string{"stratum:latest"}, 1)
	members := entries(t, original)
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(members["manifest.json"], &arr); err != nil {
		t.Fatal(err)
	}
	extra := json.RawMessage(`{"sha256:feedbeef":{"mediaType":"application/example"}}`)
	arr[0]["LayerSources"] = extra
	manifestRaw, _ := json.Marshal(arr)
	members["manifest.json"] = manifestRaw

	input := filepath.Join(temp, "with-extras.tar")
	f, err := os.Create(input)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for member, data := range members {
		if err := tw.WriteHeader(&tar.Header{Name: member, Size: int64(len(data)), Typeflag: tar.TypeReg, Mode: 0644}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	layer := filepath.Join(temp, "custom.tar")
	testLayer(t, layer, map[string][]byte{"stratum/test.txt": []byte("hi")})
	output := filepath.Join(temp, "output.tar")
	if err := execute([]string{"--input", input, "--output", output, "--layer", layer}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	var updated []map[string]json.RawMessage
	if err := json.Unmarshal(entries(t, output)["manifest.json"], &updated); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(updated[0]["LayerSources"], extra) {
		t.Fatalf("extra manifest fields lost: %s", updated[0]["LayerSources"])
	}
}

func TestNoArgumentsShowsHelp(t *testing.T) {
	var out bytes.Buffer
	if err := execute(nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "USAGE") {
		t.Fatal(out.String())
	}
}
