package terraform

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const Protocol = "6.0"

type Files struct {
	Archive    string
	Sums       string
	Signature  string
	Manifest   string
	Executable string
}

type Binary struct {
	OS   string
	Arch string
	Path string
}

type Package struct {
	Type     string
	Version  string
	Binaries []Binary
	Modules  string
}

type Manifest struct {
	Version  int              `json:"version"`
	Metadata ManifestMetadata `json:"metadata"`
}

type ManifestMetadata struct {
	ProtocolVersions []string `json:"protocol_versions"`
}

func GetFiles(providerType, version, goos, goarch string) Files {
	version = strings.TrimPrefix(version, "v")
	prefix := "terraform-provider-" + providerType + "_" + version
	executable := "terraform-provider-" + providerType + "_v" + version
	if goos == "windows" {
		executable += ".exe"
	}
	return Files{
		Archive:    prefix + "_" + goos + "_" + goarch + ".zip",
		Sums:       prefix + "_SHA256SUMS",
		Signature:  prefix + "_SHA256SUMS.sig",
		Manifest:   prefix + "_manifest.json",
		Executable: executable,
	}
}

func GetModuleArchive(name, system, version string) string {
	return "terraform-module-" + name + "-" + system + "_" + strings.TrimPrefix(version, "v") + ".tar.gz"
}

func PutPackage(dir string, p Package) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	version := strings.TrimPrefix(p.Version, "v")
	release := GetFiles(p.Type, version, "", "")
	var written []string
	var sums []string
	sum := func(path, name string) error {
		hash, err := sumFile(path)
		if err != nil {
			return err
		}
		sums = append(sums, hash+"  "+name)
		written = append(written, path)
		return nil
	}
	for _, binary := range p.Binaries {
		files := GetFiles(p.Type, version, binary.OS, binary.Arch)
		path := filepath.Join(dir, files.Archive)
		if err := putZip(path, files.Executable, binary.Path); err != nil {
			return written, err
		}
		if err := sum(path, files.Archive); err != nil {
			return written, err
		}
	}
	if len(sums) > 0 {
		manifest, err := json.Marshal(Manifest{Version: 1, Metadata: ManifestMetadata{ProtocolVersions: []string{Protocol}}})
		if err != nil {
			return written, err
		}
		path := filepath.Join(dir, release.Manifest)
		if err := os.WriteFile(path, append(manifest, '\n'), 0o644); err != nil {
			return written, err
		}
		if err := sum(path, release.Manifest); err != nil {
			return written, err
		}
		sort.Slice(sums, func(i, j int) bool { return sums[i][66:] < sums[j][66:] })
		path = filepath.Join(dir, release.Sums)
		if err := os.WriteFile(path, []byte(strings.Join(sums, "\n")+"\n"), 0o644); err != nil {
			return written, err
		}
		written = append(written, path)
	}
	if p.Modules == "" {
		return written, nil
	}
	names, err := os.ReadDir(p.Modules)
	if err != nil {
		return written, err
	}
	for _, name := range names {
		if !name.IsDir() {
			continue
		}
		systems, err := os.ReadDir(filepath.Join(p.Modules, name.Name()))
		if err != nil {
			return written, err
		}
		for _, system := range systems {
			if !system.IsDir() {
				continue
			}
			path := filepath.Join(dir, GetModuleArchive(name.Name(), system.Name(), version))
			if err := putTar(path, filepath.Join(p.Modules, name.Name(), system.Name())); err != nil {
				return written, err
			}
			written = append(written, path)
		}
	}
	return written, nil
}

func sumFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func putZip(path, name, binary string) error {
	source, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	archive := zip.NewWriter(out)
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		out.Close()
		return err
	}
	header.Name = name
	header.Method = zip.Deflate
	header.SetMode(0o755)
	entry, err := archive.CreateHeader(header)
	if err == nil {
		_, err = io.Copy(entry, source)
	}
	if err == nil {
		err = archive.Close()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

func skipped(entry fs.DirEntry) bool {
	base := entry.Name()
	if entry.IsDir() {
		return base == ".terraform"
	}
	return strings.HasSuffix(base, ".tfstate") || strings.HasSuffix(base, ".tfstate.backup") || strings.HasSuffix(base, ".tfvars") || strings.HasSuffix(base, ".tfvars.json")
}

func putTar(path, root string) error {
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	compressed := gzip.NewWriter(out)
	archive := tar.NewWriter(compressed)
	err = filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, file)
		if err != nil || rel == "." {
			return err
		}
		if skipped(entry) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() && !entry.IsDir() {
			return fmt.Errorf("%s is neither a file nor a directory", file)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if entry.IsDir() {
			header.Name += "/"
		}
		if err := archive.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		source, err := os.Open(file)
		if err != nil {
			return err
		}
		defer source.Close()
		_, err = io.Copy(archive, source)
		return err
	})
	if err == nil {
		err = archive.Close()
	}
	if err == nil {
		err = compressed.Close()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}
