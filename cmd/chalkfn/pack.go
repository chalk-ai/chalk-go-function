package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// buildTarget is a main package inside a Go module.
type buildTarget struct {
	ModuleDir  string // absolute path of the module root
	ModulePath string // module path from go.mod
	GoVersion  string // `go` directive from go.mod
	PkgDir     string // package directory, relative to ModuleDir, slash-separated
}

// goEnv is the environment every go command the CLI runs uses. Workspaces are
// disabled so the local build resolves dependencies exactly as the remote
// build will from the shipped go.mod and go.sum.
func goEnv(extra ...string) []string {
	return append(append(os.Environ(), "GOWORK=off"), extra...)
}

func goCmd(dir string, env []string, args ...string) *exec.Cmd {
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = os.Stderr
	return cmd
}

func resolveTarget(pkg string) (buildTarget, error) {
	abs, err := filepath.Abs(pkg)
	if err != nil {
		return buildTarget{}, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return buildTarget{}, fmt.Errorf("%s is not a package directory", pkg)
	}
	out, err := goCmd(abs, goEnv(), "list", "-json", ".").Output()
	if err != nil {
		return buildTarget{}, fmt.Errorf("go list %s: %w", pkg, err)
	}
	var p struct {
		Name   string
		Dir    string
		Module *struct {
			Path      string
			Dir       string
			GoVersion string
		}
	}
	if err := json.Unmarshal(out, &p); err != nil {
		return buildTarget{}, err
	}
	if p.Name != "main" {
		return buildTarget{}, fmt.Errorf("%s is package %s; deploy a main package that calls chalkfn.Serve()", pkg, p.Name)
	}
	if p.Module == nil {
		return buildTarget{}, fmt.Errorf("%s is not inside a Go module", pkg)
	}
	rel, err := filepath.Rel(p.Module.Dir, p.Dir)
	if err != nil {
		return buildTarget{}, err
	}
	return buildTarget{
		ModuleDir:  p.Module.Dir,
		ModulePath: p.Module.Path,
		GoVersion:  p.Module.GoVersion,
		PkgDir:     filepath.ToSlash(rel),
	}, nil
}

// buildPath is the package path to pass to `go build` from the module root.
func (t buildTarget) buildPath() string {
	if t.PkgDir == "." {
		return "."
	}
	return "./" + t.PkgDir
}

// buildLocal compiles the target for the host into dir so its registry can be
// listed.
func buildLocal(t buildTarget, dir string) (string, error) {
	bin := filepath.Join(dir, "chalkfn-target")
	cmd := goCmd(t.ModuleDir, goEnv(), "build", "-o", bin, t.buildPath())
	cmd.Stdout = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build %s: %w", t.buildPath(), err)
	}
	return bin, nil
}

// sourceFiles lists the module-relative paths the remote build needs: go.mod,
// go.sum, and the files of every main-module package the target imports when
// built for linux/amd64. Dependencies outside the module are downloaded by the
// remote build, so only the target's own code ships.
func sourceFiles(t buildTarget, cgo bool) ([]string, error) {
	env := goEnv("GOOS=linux", "GOARCH=amd64", "CGO_ENABLED="+boolToBit(cgo))
	out, err := goCmd(t.ModuleDir, env, "list", "-deps", "-json", t.buildPath()).Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps %s: %w", t.buildPath(), err)
	}
	type listedPackage struct {
		ImportPath string
		Dir        string
		Standard   bool
		Module     *struct {
			Path    string
			Main    bool
			Replace *struct {
				Path    string
				Version string
			}
		}
		GoFiles, CgoFiles, CFiles, CXXFiles, HFiles, SFiles, SysoFiles, EmbedFiles []string
	}
	seen := map[string]bool{"go.mod": true}
	if _, err := os.Stat(filepath.Join(t.ModuleDir, "go.sum")); err == nil {
		seen["go.sum"] = true
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p listedPackage
		if err := dec.Decode(&p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, err
		}
		if p.Standard || p.Module == nil {
			continue
		}
		if r := p.Module.Replace; r != nil && r.Version == "" {
			return nil, fmt.Errorf("module %s is replaced by local directory %s, which the remote build cannot see; publish it or move the code into this module", p.Module.Path, r.Path)
		}
		if !p.Module.Main {
			continue
		}
		rel, err := filepath.Rel(t.ModuleDir, p.Dir)
		if err != nil {
			return nil, err
		}
		groups := [][]string{p.GoFiles, p.CgoFiles, p.CFiles, p.CXXFiles, p.HFiles, p.SFiles, p.SysoFiles, p.EmbedFiles}
		for _, files := range groups {
			for _, f := range files {
				seen[filepath.ToSlash(filepath.Join(rel, f))] = true
			}
		}
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	sort.Strings(files)
	return files, nil
}

// tarGz archives files (module-relative) deterministically: entries are
// sorted and carry no timestamps or ownership, so unchanged sources produce
// identical bytes and the image build cache hits.
func tarGz(root string, files []string) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	tw := tar.NewWriter(gz)
	for _, rel := range files {
		if strings.HasPrefix(rel, "../") {
			return nil, fmt.Errorf("file %s is outside the module", rel)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		hdr := &tar.Header{Name: rel, Mode: 0o644, Size: int64(len(data)), Format: tar.FormatPAX, Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func boolToBit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
