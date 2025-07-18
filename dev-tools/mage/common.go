// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License 2.0;
// you may not use this file except in compliance with the Elastic License 2.0.

package mage

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"
	"unicode"

	"github.com/magefile/mage/mg"
	"github.com/magefile/mage/sh"
	"github.com/magefile/mage/target"
)

const (
	inlineTemplate      = "inline"
	xpackDirName        = "x-pack"
	windowsBinarySuffix = ".exe"
)

// Expand expands the given Go text/template string.
func Expand(in string, args ...map[string]interface{}) (string, error) {
	return expandTemplate(inlineTemplate, in, FuncMap, EnvMap(args...))
}

// MustExpand expands the given Go text/template string. It panics if there is
// an error.
func MustExpand(in string, args ...map[string]interface{}) string {
	out, err := Expand(in, args...)
	if err != nil {
		panic(err)
	}
	return out
}

// ExpandFile expands the Go text/template read from src and writes the output
// to dst.
func ExpandFile(src, dst string, args ...map[string]interface{}) error {
	return expandFile(src, dst, EnvMap(args...))
}

// MustExpandFile expands the Go text/template read from src and writes the
// output to dst. It panics if there is an error.
func MustExpandFile(src, dst string, args ...map[string]interface{}) {
	if err := ExpandFile(src, dst, args...); err != nil {
		panic(err)
	}
}

func expandTemplate(name, tmpl string, funcs template.FuncMap, args ...map[string]interface{}) (string, error) {
	t := template.New(name).Option("missingkey=error")
	if len(funcs) > 0 {
		t = t.Funcs(funcs)
	}

	t, err := t.Parse(tmpl)
	if err != nil {
		if name == inlineTemplate {
			return "", fmt.Errorf("failed to parse template '%v' : %w", tmpl, err)
		}
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	buf := new(bytes.Buffer)
	if err := t.Execute(buf, joinMaps(args...)); err != nil {
		if name == inlineTemplate {
			return "", fmt.Errorf("failed to expand template '%v': %w", tmpl, err)
		}
		return "", fmt.Errorf("failed to expand template: %w", err)
	}

	return buf.String(), nil
}

func joinMaps(args ...map[string]interface{}) map[string]interface{} {
	switch len(args) {
	case 0:
		return nil
	case 1:
		return args[0]
	}

	out := map[string]interface{}{}
	for _, m := range args {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func expandFile(src, dst string, args ...map[string]interface{}) error {
	tmplData, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("failed reading from template %v, %w", src, err)
	}

	output, err := expandTemplate(src, string(tmplData), FuncMap, args...)
	if err != nil {
		return err
	}

	dst, err = expandTemplate(inlineTemplate, dst, FuncMap, args...)
	if err != nil {
		return err
	}

	if err = os.WriteFile(createDir(dst), []byte(output), 0644); err != nil {
		return fmt.Errorf("failed to write rendered template: %w", err)
	}

	return nil
}

// CWD return the current working directory.
func CWD(elem ...string) string {
	wd, err := os.Getwd()
	if err != nil {
		panic(fmt.Errorf("failed to get the CWD: %w", err))
	}
	return filepath.Join(append([]string{wd}, elem...)...)
}

// EnvOr returns the value of the specified environment variable if it is
// non-empty. Otherwise it return def.
func EnvOr(name, def string) string {
	s := os.Getenv(name)
	if s == "" {
		return def
	}
	return s
}

var (
	dockerInfoValue *DockerInfo
	dockerInfoErr   error
	dockerInfoOnce  sync.Once
)

// DockerInfo contains information about the docker daemon.
type DockerInfo struct {
	OperatingSystem string   `json:"OperatingSystem"`
	Labels          []string `json:"Labels"`
	NCPU            int      `json:"NCPU"`
	MemTotal        int      `json:"MemTotal"`
}

// IsBoot2Docker returns true if the Docker OS is boot2docker.
func (info *DockerInfo) IsBoot2Docker() bool {
	return strings.Contains(strings.ToLower(info.OperatingSystem), "boot2docker")
}

// HaveDocker returns an error if docker is unavailable.
func HaveDocker() error {
	if _, err := GetDockerInfo(); err != nil {
		return fmt.Errorf("docker is not available: %w", err)
	}
	return nil
}

// GetDockerInfo returns data from the docker info command.
func GetDockerInfo() (*DockerInfo, error) {
	dockerInfoOnce.Do(func() {
		dockerInfoValue, dockerInfoErr = dockerInfo()
	})

	return dockerInfoValue, dockerInfoErr
}

func dockerInfo() (*DockerInfo, error) {
	data, err := sh.Output("docker", "info", "-f", "{{ json .}}")
	if err != nil {
		return nil, err
	}

	var info DockerInfo
	if err = json.Unmarshal([]byte(data), &info); err != nil {
		return nil, err
	}

	return &info, nil
}

// HaveDockerCompose returns an error if docker-compose is not found on the
// PATH.
func HaveDockerCompose() error {
	_, err := exec.LookPath("docker-compose")
	if err != nil {
		return fmt.Errorf("docker-compose is not available")
	}
	return nil
}

// HaveKubectl returns an error if kind is not found on the PATH.
func HaveKubectl() error {
	_, err := exec.LookPath("kubectl")
	if err != nil {
		return fmt.Errorf("kubectl is not available")
	}
	return nil
}

// FindReplace reads a file, performs a find/replace operation, then writes the
// output to the same file path.
func FindReplace(file string, re *regexp.Regexp, repl string) error {
	info, err := os.Stat(file)
	if err != nil {
		return err
	}

	contents, err := os.ReadFile(file)
	if err != nil {
		return err
	}

	out := re.ReplaceAllString(string(contents), repl)
	return os.WriteFile(file, []byte(out), info.Mode().Perm())
}

// MustFindReplace invokes FindReplace and panics if an error occurs.
func MustFindReplace(file string, re *regexp.Regexp, repl string) {
	if err := FindReplace(file, re, repl); err != nil {
		panic(fmt.Errorf("failed to find and replace: %w", err))
	}
}

// DownloadFile downloads the given URL and writes the file to destinationDir.
// The path to the file is returned.
func DownloadFile(url, destinationDir string) (string, error) {
	log.Println("Downloading", url)

	//nolint:gosec,noctx // url is not user input
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("http get failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed with http status: %v : %w", resp.StatusCode, err)
	}

	name := filepath.Join(destinationDir, filepath.Base(url))
	f, err := os.Create(createDir(name))
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}
	defer f.Close()

	if _, err = io.Copy(f, resp.Body); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return name, f.Close()
}

// Extract extracts .zip, .tar.gz, or .tgz files to destinationDir.
func Extract(sourceFile, destinationDir string) error {
	ext := filepath.Ext(sourceFile)
	switch {
	case strings.HasSuffix(sourceFile, ".tar.gz"), ext == ".tgz":
		return untar(sourceFile, destinationDir)
	case ext == ".zip":
		return unzip(sourceFile, destinationDir)
	default:
		return fmt.Errorf("failed to extract %v, unhandled file extension", sourceFile)
	}
}

func unzip(sourceFile, destinationDir string) error {
	r, err := zip.OpenReader(sourceFile)
	if err != nil {
		return err
	}
	defer r.Close()

	if err = os.MkdirAll(destinationDir, 0755); err != nil {
		return err
	}

	extractAndWriteFile := func(f *zip.File) error {
		innerFile, err := f.Open()
		if err != nil {
			return err
		}
		defer innerFile.Close()

		//nolint:gosec // G305 zip traversal, no user input
		path := filepath.Join(destinationDir, f.Name)
		if !strings.HasPrefix(path, destinationDir) {
			return fmt.Errorf("illegal file path in zip: %v", f.Name)
		}

		if f.FileInfo().IsDir() {
			return os.MkdirAll(path, f.Mode())
		}

		if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}

		out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			return err
		}
		defer out.Close()

		//nolint:gosec // DoS vulnerability, no user input
		if _, err = io.Copy(out, innerFile); err != nil {
			return err
		}

		return out.Close()
	}

	for _, f := range r.File {
		err := extractAndWriteFile(f)
		if err != nil {
			return err
		}
	}

	return nil
}

// Tar compress a directory using tar + gzip algorithms
func Tar(src string, targetFile string) error {
	fmt.Printf(">> creating TAR file from directory: %s, target: %s\n", src, targetFile)

	f, err := os.Create(targetFile)
	if err != nil {
		return fmt.Errorf("error creating tar file: %w", err)
	}
	defer f.Close()

	// tar > gzip > file
	zr := gzip.NewWriter(f)
	tw := tar.NewWriter(zr)

	// walk through every file in the folder
	err = filepath.WalkDir(src, func(file string, d fs.DirEntry, errFn error) error {
		if errFn != nil {
			return fmt.Errorf("error traversing the file system: %w", errFn)
		}

		// if a symlink, skip file
		if d.Type() == os.ModeSymlink {
			fmt.Printf(">> skipping symlink: %s\n", file)
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return fmt.Errorf("error getting direntry info: %w", err)
		}

		// generate tar header
		header, err := tar.FileInfoHeader(fi, file)
		if err != nil {
			return fmt.Errorf("error getting file info header: %w", err)
		}

		// must provide real name
		// (see https://golang.org/src/archive/tar/common.go?#L626)
		header.Name = filepath.ToSlash(file)

		// write header
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("error writing header: %w", err)
		}

		// if not a dir, write file content
		if !fi.IsDir() {
			data, err := os.Open(file)
			if err != nil {
				return fmt.Errorf("error opening file: %w", err)
			}
			defer data.Close()
			if _, err := io.Copy(tw, data); err != nil {
				return fmt.Errorf("error compressing file: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("error walking path '%s': %w", src, err)
	}

	// produce tar
	if err := tw.Close(); err != nil {
		return fmt.Errorf("error closing tar file: %w", err)
	}
	// produce gzip
	if err := zr.Close(); err != nil {
		return fmt.Errorf("error closing gzip file: %w", err)
	}

	return nil
}

func untar(sourceFile, destinationDir string) error {
	file, err := os.Open(sourceFile)
	if err != nil {
		return err
	}
	defer file.Close()

	var fileReader io.ReadCloser = file

	if strings.HasSuffix(sourceFile, ".gz") || strings.HasSuffix(sourceFile, ".tgz") {
		if fileReader, err = gzip.NewReader(file); err != nil {
			return err
		}
		defer fileReader.Close()
	}

	tarReader := tar.NewReader(fileReader)

	for {
		header, err := tarReader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}

		//nolint:gosec // G305: file traversal, no user input
		path := filepath.Join(destinationDir, header.Name)
		if !strings.HasPrefix(path, destinationDir) {
			return fmt.Errorf("illegal file path in tar: %v", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err = os.MkdirAll(path, os.FileMode(header.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}

			writer, err := os.Create(path)
			if err != nil {
				return err
			}

			//nolint:gosec // decompression bomb, no user input
			if _, err = io.Copy(writer, tarReader); err != nil {
				return err
			}

			if err = os.Chmod(path, os.FileMode(header.Mode)); err != nil {
				return err
			}

			if err = writer.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, path); err != nil {
				return fmt.Errorf("error creating symlink %s pointing to %s: %w", path, header.Linkname, err)
			}

		default:
			return fmt.Errorf("unable to untar type=%c in file=%s", header.Typeflag, path)
		}
	}

	return nil
}

func isSeparator(r rune) bool {
	return unicode.IsSpace(r) || r == ',' || r == ';'
}

// RunCmds runs the given commands and stops upon the first error.
func RunCmds(cmds ...[]string) error {
	for _, cmd := range cmds {
		if err := sh.Run(cmd[0], cmd[1:]...); err != nil {
			return err
		}
	}
	return nil
}

var (
	parallelJobsLock      sync.Mutex
	parallelJobsSemaphore chan int
)

func parallelJobs() chan int {
	parallelJobsLock.Lock()
	defer parallelJobsLock.Unlock()

	if parallelJobsSemaphore == nil {
		max := numParallel()
		parallelJobsSemaphore = make(chan int, max)
		log.Println("Max parallel jobs =", max)
	}

	return parallelJobsSemaphore
}

func numParallel() int {
	if maxParallel := os.Getenv("MAX_PARALLEL"); maxParallel != "" {
		if num, err := strconv.Atoi(maxParallel); err == nil && num > 0 {
			return num
		}
	}

	// To be conservative use the minimum of the number of CPUs between the host
	// and the Docker host.
	maxParallel := runtime.NumCPU()

	info, err := GetDockerInfo()
	// Check that info.NCPU != 0 since docker info doesn't return with an
	// error status if communcation with the daemon failed.
	if err == nil && info.NCPU != 0 && info.NCPU < maxParallel {
		maxParallel = info.NCPU
	}

	return maxParallel
}

// ParallelCtx runs the given functions in parallel with an upper limit set
// based on GOMAXPROCS. The provided ctx is passed to the functions (if they
// accept it as a param).
func ParallelCtx(ctx context.Context, fns ...interface{}) {
	fnWrappers := make([]func(context.Context) error, 0, len(fns))
	for _, f := range fns {
		fnWrapper := funcTypeWrap(f)
		if fnWrapper == nil {
			panic("attempted to add a dep that did not match required function type")
		}
		fnWrappers = append(fnWrappers, fnWrapper)
	}

	var mu sync.Mutex
	var errs []string
	var wg sync.WaitGroup

	for _, fw := range fnWrappers {
		wg.Add(1)
		go func(fw func(context.Context) error) {
			defer func() {
				if v := recover(); v != nil {
					mu.Lock()
					errs = append(errs, fmt.Sprint(v))
					mu.Unlock()
				}
				wg.Done()
				<-parallelJobs()
			}()
			waitStart := time.Now()
			parallelJobs() <- 1
			log.Println("Parallel job waited", time.Since(waitStart), "before starting.")
			if err := fw(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprint(err))
				mu.Unlock()
			}
		}(fw)
	}

	wg.Wait()
	if len(errs) > 0 {
		panic(fmt.Errorf("multiple failures: %s", strings.Join(errs, "\n")))
	}
}

// Parallel runs the given functions in parallel with an upper limit set based
// on GOMAXPROCS.
func Parallel(fns ...interface{}) {
	ParallelCtx(context.TODO(), fns...)
}

// funcTypeWrap wraps a valid FuncType to FuncContextError
func funcTypeWrap(fn interface{}) func(context.Context) error {
	switch f := fn.(type) {
	case func():
		return func(context.Context) error {
			f()
			return nil
		}
	case func() error:
		return func(context.Context) error {
			return f()
		}
	case func(context.Context):
		return func(ctx context.Context) error {
			f(ctx)
			return nil
		}
	case func(context.Context) error:
		return f
	}
	return nil
}

// FindFiles return a list of file matching the given glob patterns.
func FindFiles(globs ...string) ([]string, error) {
	var configFiles []string
	for _, glob := range globs {
		files, err := filepath.Glob(glob)
		if err != nil {
			return nil, fmt.Errorf("failed on glob %v: %w", glob, err)
		}
		configFiles = append(configFiles, files...)
	}
	return configFiles, nil
}

// FindFilesRecursive recursively traverses from the CWD and invokes the given
// match function on each regular file to determine if the given path should be
// returned as a match. It ignores files in .git directories.
func FindFilesRecursive(match func(path string, d fs.FileInfo) bool) ([]string, error) {
	var matches []string
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Don't look for files in git directories
		if d.IsDir() && filepath.Base(path) == ".git" {
			return filepath.SkipDir
		}

		if !d.Type().IsRegular() {
			// continue
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		if match(filepath.ToSlash(path), info) {
			matches = append(matches, path)
		}
		return nil
	})
	return matches, err
}

// FileConcat concatenates files and writes the output to out.
func FileConcat(out string, perm os.FileMode, files ...string) error {
	f, err := os.OpenFile(createDir(out), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)

	append := func(file string) error {
		in, err := os.Open(file)
		if err != nil {
			return err
		}
		defer in.Close()

		if _, err := io.Copy(w, in); err != nil {
			return err
		}

		return nil
	}

	for _, in := range files {
		if err := append(in); err != nil {
			return err
		}
	}

	if err = w.Flush(); err != nil {
		return err
	}
	return f.Close()
}

// MustFileConcat invokes FileConcat and panics if an error occurs.
func MustFileConcat(out string, perm os.FileMode, files ...string) {
	if err := FileConcat(out, perm, files...); err != nil {
		panic(err)
	}
}

// VerifySHA256 reads a file and verifies that its SHA256 sum matches the
// specified hash.
func VerifySHA256(file string, hash string) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("failed to open file for sha256 verification: %w", err)
	}
	defer f.Close()

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Errorf("failed reading from input file: %w", err)
	}

	computedHash := hex.EncodeToString(sum.Sum(nil))
	expectedHash := strings.TrimSpace(hash)

	if computedHash != expectedHash {
		return fmt.Errorf("SHA256 verification of %v failed. Expected=%v, "+
			"but computed=%v", f.Name(), expectedHash, computedHash)
	}
	log.Println("SHA256 OK:", f.Name())

	return nil
}

// CreateSHA512File computes the sha512 sum of the specified file the writes
// a sidecar file containing the hash and filename.
func CreateSHA512File(file string) error {
	computedHash, err := GetSHA512Hash(file)
	if err != nil {
		return err
	}
	out := fmt.Sprintf("%v  %v", computedHash, filepath.Base(file))

	return os.WriteFile(file+".sha512", []byte(out), 0644)
}

// GetSHA512Hash returns SHA512 hash of file.
func GetSHA512Hash(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", fmt.Errorf("failed to open file for sha512 summing: %w", err)
	}
	defer f.Close()

	sum := sha512.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", fmt.Errorf("failed reading from input file: %w", err)
	}

	computedHash := hex.EncodeToString(sum.Sum(nil))
	return computedHash, nil
}

// Mage executes mage targets in the specified directory.
func Mage(dir string, targets ...string) error {
	c := exec.Command("mage", targets...)
	c.Dir = dir
	if mg.Verbose() {
		c.Env = append(os.Environ(), "MAGEFILE_VERBOSE=1")
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	log.Println("exec:", strings.Join(c.Args, " "))
	err := c.Run()
	return err
}

// IsUpToDate returns true if dst exists and is older based on modtime than all the sources.
func IsUpToDate(dst string, sources ...string) bool {
	if len(sources) == 0 {
		panic("No sources passed to IsUpToDate")
	}

	var files []string
	for _, s := range sources {
		err := filepath.WalkDir(s, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}

			if d.Type().IsRegular() {
				files = append(files, path)
			}

			return nil
		})
		if err != nil {
			panic(err)
		}
	}

	execute, err := target.Path(dst, files...)
	return err == nil && !execute
}

// OSSBeatDir returns the OSS beat directory. You can pass paths and they will
// be joined and appended to the OSS beat dir.
func OSSBeatDir(path ...string) string {
	ossDir := CWD()

	// Check if we need to correct ossDir because it's in x-pack.
	if parentDir := filepath.Base(filepath.Dir(ossDir)); parentDir == xpackDirName {
		// If the OSS version of the beat exists.
		tmp := filepath.Join(ossDir, "../..", BeatName)
		if _, err := os.Stat(tmp); !os.IsNotExist(err) {
			ossDir = tmp
		}
	}

	return filepath.Join(append([]string{ossDir}, path...)...)
}

// XPackBeatDir returns the X-Pack beat directory. You can pass paths and they
// will be joined and appended to the X-Pack beat dir.
func XPackBeatDir(path ...string) string {
	// Check if we have an X-Pack only beats
	cur := CWD()

	if parentDir := filepath.Base(filepath.Dir(cur)); parentDir == xpackDirName {
		tmp := filepath.Join(filepath.Dir(cur), BeatName)
		return filepath.Join(append([]string{tmp}, path...)...)
	}

	return OSSBeatDir(append([]string{XPackDir, BeatName}, path...)...)
}

// createDir creates the parent directory for the given file.
// Deprecated: Use CreateDir.
func createDir(file string) string {
	return CreateDir(file)
}

// CreateDir creates the parent directory for the given file.
func CreateDir(file string) string {
	// Create the output directory.
	if dir := filepath.Dir(file); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			panic(fmt.Errorf("failed to create parent dir for %v : %w", file, err))
		}
	}
	return file
}

// binaryExtension returns the appropriate file extension based on GOOS.
func binaryExtension(goos string) string {
	if goos == "windows" {
		return windowsBinarySuffix
	}
	return ""
}

var parseVersionRegex = regexp.MustCompile(`(?m)^[^\d]*(?P<major>\d+)\.(?P<minor>\d+)(?:\.(?P<patch>\d+).*)?$`)

// ParseVersion extracts the major, minor, and optional patch number from a
// version string.
func ParseVersion(version string) (int, int, int, error) {
	names := parseVersionRegex.SubexpNames()
	matches := parseVersionRegex.FindStringSubmatch(version)
	if len(matches) == 0 {
		err := fmt.Errorf("failed to parse version '%v'", version)
		return 0, 0, 0, err
	}

	data := map[string]string{}
	for i, match := range matches {
		data[names[i]] = match
	}
	major, _ := strconv.Atoi(data["major"])
	minor, _ := strconv.Atoi(data["minor"])
	patch, _ := strconv.Atoi(data["patch"])
	return major, minor, patch, nil
}

// ListMatchingEnvVars returns all the environment variables names that begin
// with prefix.
func ListMatchingEnvVars(prefixes ...string) []string {
	var vars []string
	for _, v := range os.Environ() {
		for _, prefix := range prefixes {
			if strings.HasPrefix(v, prefix) {
				eqIdx := strings.Index(v, "=")
				if eqIdx != -1 {
					vars = append(vars, v[:eqIdx])
				}
				break
			}
		}
	}
	return vars
}

// IntegrationTestEnvVars returns the names of environment variables needed to configure
// connections to integration test environments.
func IntegrationTestEnvVars() []string {
	// Environment variables that can be configured with paths to files
	// with authentication information.
	vars := []string{
		"AWS_SHARED_CREDENTIAL_FILE",
		"AZURE_AUTH_LOCATION",
		"GOOGLE_APPLICATION_CREDENTIALS",
	}
	// Environment variables with authentication information.
	prefixes := []string{
		"AWS_",
		"AZURE_",
		"GCP_",

		// Accepted by terraform, but not by many clients, including Beats
		"GOOGLE_",
		"GCLOUD_",
	}
	for _, prefix := range prefixes {
		vars = append(vars, ListMatchingEnvVars(prefix)...)
	}
	return vars
}

// ReadGLIBCRequirement returns the required glibc version for a dynamically
// linked ELF binary. The target machine must have a version equal to or
// greater than (newer) the returned value.
func ReadGLIBCRequirement(elfFile string) (*SemanticVersion, error) {
	e, err := elf.Open(elfFile)
	if err != nil {
		return nil, err
	}

	symbols, err := e.DynamicSymbols()
	if err != nil {
		return nil, err
	}

	versionSet := map[SemanticVersion]struct{}{}
	for _, sym := range symbols {
		if strings.HasPrefix(sym.Version, "GLIBC_") {
			semver, err := NewSemanticVersion(strings.TrimPrefix(sym.Version, "GLIBC_"))
			if err != nil {
				continue
			}

			versionSet[*semver] = struct{}{}
		}
	}

	if len(versionSet) == 0 {
		return nil, fmt.Errorf("no GLIBC symbols found in binary (is this a static binary?)")
	}

	versions := make([]SemanticVersion, 0, len(versionSet))
	for ver := range versionSet {
		versions = append(versions, ver)
	}

	sort.Slice(versions, func(i, j int) bool {
		a := versions[i]
		b := versions[j]
		return a.LessThan(&b)
	})

	max := versions[len(versions)-1]
	return &max, nil
}
