// Command build manages cronet static library builds for cronet-go.
//
// Usage:
//
//	go run ./cmd/build [flags] <command>
//
// Commands:
//
//	build    Build cronet_static for specified targets
//	package  Package libraries and generate CGO config files
//	publish  Commit to go branch and push
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Target represents a build target platform
type Target struct {
	OS   string // gn target_os: linux, mac, win, android
	CPU  string // gn target_cpu: x64, arm64, x86, arm
	GOOS string // Go GOOS
	ARCH string // Go GOARCH
}

var allTargets = []Target{
	{OS: "linux", CPU: "x64", GOOS: "linux", ARCH: "amd64"},
	{OS: "linux", CPU: "arm64", GOOS: "linux", ARCH: "arm64"},
	{OS: "mac", CPU: "x64", GOOS: "darwin", ARCH: "amd64"},
	{OS: "mac", CPU: "arm64", GOOS: "darwin", ARCH: "arm64"},
	{OS: "win", CPU: "x64", GOOS: "windows", ARCH: "amd64"},
	{OS: "win", CPU: "arm64", GOOS: "windows", ARCH: "arm64"},
	{OS: "ios", CPU: "arm64", GOOS: "ios", ARCH: "arm64"},
	{OS: "android", CPU: "arm64", GOOS: "android", ARCH: "arm64"},
	{OS: "android", CPU: "x64", GOOS: "android", ARCH: "amd64"},
	{OS: "android", CPU: "arm", GOOS: "android", ARCH: "arm"},
	{OS: "android", CPU: "x86", GOOS: "android", ARCH: "386"},
}

var (
	projectRoot string
	naiveRoot   string
	srcRoot     string
)

func init() {
	// Find project root (directory containing go.mod)
	wd, err := os.Getwd()
	if err != nil {
		fatal("failed to get working directory: %v", err)
	}

	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			projectRoot = dir
			break
		}
		if dir == filepath.Dir(dir) {
			fatal("could not find project root (go.mod)")
		}
	}

	naiveRoot = filepath.Join(projectRoot, "naiveproxy")
	srcRoot = filepath.Join(naiveRoot, "src")
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [flags] <command>\n\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "Commands:\n")
		fmt.Fprintf(os.Stderr, "  sync      Download Chromium cronet components\n")
		fmt.Fprintf(os.Stderr, "  build     Build cronet_static for specified targets\n")
		fmt.Fprintf(os.Stderr, "  package   Package libraries and generate CGO config files\n")
		fmt.Fprintf(os.Stderr, "  publish   Commit to go branch and push\n")
		fmt.Fprintf(os.Stderr, "\nFlags:\n")
		flag.PrintDefaults()
	}

	var targetStr string
	flag.StringVar(&targetStr, "targets", "", "Comma-separated list of targets (e.g., linux/amd64,darwin/arm64). Empty means host only.")

	flag.Parse()

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}

	cmd := flag.Arg(0)

	targets := parseTargets(targetStr)

	switch cmd {
	case "sync":
		cmdSync()
	case "build":
		cmdBuild(targets)
	case "package":
		cmdPackage(targets)
	case "publish":
		cmdPublish()
	default:
		fatal("unknown command: %s", cmd)
	}
}

func parseTargets(s string) []Target {
	if s == "" {
		// Default to host platform
		hostOS := runtime.GOOS
		hostArch := runtime.GOARCH
		for _, t := range allTargets {
			if t.GOOS == hostOS && t.ARCH == hostArch {
				return []Target{t}
			}
		}
		fatal("unsupported host platform: %s/%s", hostOS, hostArch)
	}

	if s == "all" {
		return allTargets
	}

	var targets []Target
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		parts := strings.Split(part, "/")
		if len(parts) != 2 {
			fatal("invalid target format: %s (expected os/arch)", part)
		}
		goos, goarch := parts[0], parts[1]
		found := false
		for _, t := range allTargets {
			if t.GOOS == goos && t.ARCH == goarch {
				targets = append(targets, t)
				found = true
				break
			}
		}
		if !found {
			fatal("unsupported target: %s/%s", goos, goarch)
		}
	}
	return targets
}

func cmdBuild(targets []Target) {
	log("Building cronet_static for %d target(s)", len(targets))

	for _, t := range targets {
		log("Building %s/%s...", t.GOOS, t.ARCH)
		buildTarget(t)
	}

	log("Build complete!")
}

// SysrootInfo contains information about a Linux sysroot
type SysrootInfo struct {
	Sha256Sum  string `json:"Sha256Sum"`
	SysrootDir string `json:"SysrootDir"`
	Tarball    string `json:"Tarball"`
	URL        string `json:"URL"`
}

func ensureAndroidNDK() string {
	ndkDir := filepath.Join(srcRoot, "third_party", "android_toolchain", "ndk")

	// Check if already set up
	if _, err := os.Stat(filepath.Join(ndkDir, "toolchains")); err == nil {
		log("Android NDK already configured")
		return ndkDir
	}

	// Check for local Android SDK NDK
	homeDir, _ := os.UserHomeDir()
	localSDK := filepath.Join(homeDir, "Library", "Android", "sdk")
	localNDKBase := filepath.Join(localSDK, "ndk")

	// Find r28 NDK (28.x.x)
	var localNDK string
	if entries, err := os.ReadDir(localNDKBase); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "28.") {
				localNDK = filepath.Join(localNDKBase, entry.Name())
				break
			}
		}
	}

	if localNDK == "" {
		// Try to install via sdkmanager
		sdkManager := filepath.Join(localSDK, "cmdline-tools", "latest", "bin", "sdkmanager")
		if _, err := os.Stat(sdkManager); err == nil {
			log("Installing Android NDK r28 via sdkmanager...")
			runCmd(localSDK, sdkManager, "--install", "ndk;28.0.13004108")
			localNDK = filepath.Join(localNDKBase, "28.0.13004108")
		} else {
			fatal("Android NDK r28 not found and sdkmanager not available. Please install NDK r28 via Android Studio.")
		}
	}

	log("Using Android NDK from: %s", localNDK)

	// Create directory structure
	os.MkdirAll(filepath.Join(ndkDir, "sources", "android"), 0755)
	os.MkdirAll(filepath.Join(ndkDir, "toolchains", "llvm"), 0755)

	// Symlink cpufeatures
	cpuFeatSrc := filepath.Join(localNDK, "sources", "android", "cpufeatures")
	cpuFeatDst := filepath.Join(ndkDir, "sources", "android", "cpufeatures")
	if _, err := os.Stat(cpuFeatDst); os.IsNotExist(err) {
		os.Symlink(cpuFeatSrc, cpuFeatDst)
	}

	// Symlink prebuilt toolchain
	prebuiltSrc := filepath.Join(localNDK, "toolchains", "llvm", "prebuilt")
	prebuiltDst := filepath.Join(ndkDir, "toolchains", "llvm", "prebuilt")
	if _, err := os.Stat(prebuiltDst); os.IsNotExist(err) {
		os.Symlink(prebuiltSrc, prebuiltDst)
	}

	log("Android NDK configured at: %s", ndkDir)
	return ndkDir
}

func ensureLinuxSysroot(arch string) string {
	// Map CPU to sysroot arch
	sysrootArch := map[string]string{
		"x64":   "amd64",
		"arm64": "arm64",
		"x86":   "i386",
		"arm":   "armhf",
	}[arch]

	if sysrootArch == "" {
		fatal("unsupported Linux arch for sysroot: %s", arch)
	}

	sysrootKey := "bullseye_" + sysrootArch
	sysrootDir := filepath.Join(srcRoot, "build", "linux", "debian_bullseye_"+sysrootArch+"-sysroot")

	// Check if sysroot already exists
	if _, err := os.Stat(sysrootDir); err == nil {
		log("Sysroot already exists: %s", sysrootDir)
		return sysrootDir
	}

	// Load sysroots.json
	sysrootsFile := filepath.Join(srcRoot, "build", "linux", "sysroot_scripts", "sysroots.json")
	data, err := os.ReadFile(sysrootsFile)
	if err != nil {
		fatal("failed to read sysroots.json: %v", err)
	}

	var sysroots map[string]SysrootInfo
	if err := json.Unmarshal(data, &sysroots); err != nil {
		fatal("failed to parse sysroots.json: %v", err)
	}

	info, ok := sysroots[sysrootKey]
	if !ok {
		fatal("sysroot not found in sysroots.json: %s", sysrootKey)
	}

	// Download sysroot (URL format is {URL}/{Sha256Sum})
	url := info.URL + "/" + info.Sha256Sum
	log("Downloading sysroot from %s...", url)

	tarballPath := filepath.Join(srcRoot, "build", "linux", info.Tarball)
	if err := downloadFile(url, tarballPath, info.Sha256Sum); err != nil {
		fatal("failed to download sysroot: %v", err)
	}

	// Extract sysroot
	log("Extracting sysroot...")
	if err := os.MkdirAll(sysrootDir, 0755); err != nil {
		fatal("failed to create sysroot directory: %v", err)
	}

	runCmd(filepath.Join(srcRoot, "build", "linux"), "tar", "-xf", info.Tarball, "-C", info.SysrootDir)

	// Clean up tarball
	os.Remove(tarballPath)

	log("Sysroot installed: %s", sysrootDir)
	return sysrootDir
}

func downloadFile(url, dest, expectedSha256 string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	hash := sha256.New()
	writer := io.MultiWriter(out, hash)

	if _, err := io.Copy(writer, resp.Body); err != nil {
		return err
	}

	actualSha256 := hex.EncodeToString(hash.Sum(nil))
	if actualSha256 != expectedSha256 {
		os.Remove(dest)
		return fmt.Errorf("sha256 mismatch: expected %s, got %s", expectedSha256, actualSha256)
	}

	return nil
}

func buildTarget(t Target) {
	outDir := fmt.Sprintf("out/cronet-%s-%s", t.OS, t.CPU)

	// Prepare GN args
	args := []string{
		"is_official_build=true",
		"is_debug=false",
		"is_clang=true",
		"fatal_linker_warnings=false",
		"treat_warnings_as_errors=false",
		"is_cronet_build=true",
		"use_udev=false",
		"use_aura=false",
		"use_ozone=false",
		"use_gio=false",
		"use_platform_icu_alternatives=true",
		"use_glib=false",
		"disable_file_support=true",
		"enable_websockets=false",
		"use_kerberos=false",
		"disable_zstd_filter=false",
		"enable_mdns=false",
		"enable_reporting=false",
		"include_transport_security_state_preload_list=false",
		"enable_device_bound_sessions=false",
		"enable_bracketed_proxy_uris=true",
		"enable_quic_proxy_support=true",
		"enable_disk_cache_sql_backend=false",
		"use_nss_certs=false",
		"enable_backup_ref_ptr_support=false",
		"enable_dangling_raw_ptr_checks=false",
		"exclude_unwind_tables=true",
		"enable_resource_allowlist_generation=false",
		"symbol_level=0",
		fmt.Sprintf("target_os=\"%s\"", t.OS),
		fmt.Sprintf("target_cpu=\"%s\"", t.CPU),
	}

	// Always disable dsyms (even for host tools when cross-compiling)
	args = append(args, "enable_dsyms=false")

	// Platform-specific args
	switch t.OS {
	case "mac":
		args = append(args, "use_sysroot=false")
	case "linux":
		// For Linux cross-compilation, we need a sysroot
		sysrootDir := ensureLinuxSysroot(t.CPU)
		relSysroot, _ := filepath.Rel(srcRoot, sysrootDir)
		args = append(args, "use_sysroot=true", fmt.Sprintf("target_sysroot=\"//%s\"", relSysroot))
		if t.CPU == "x64" {
			args = append(args, "use_cfi_icall=false")
		}
	case "win":
		args = append(args, "use_sysroot=false")
	case "android":
		ensureAndroidNDK()
		args = append(args,
			"use_sysroot=false",
			"default_min_sdk_version=24",
			"is_high_end_android=true",
			"android_ndk_major_version=28",
		)
	case "ios":
		args = append(args,
			"use_sysroot=false",
			"ios_enable_code_signing=false",
			"enable_ios_bitcode=false",
			`target_environment="device"`,
		)
	}

	gnArgs := strings.Join(args, " ")

	// Run gn gen
	gnPath := filepath.Join(srcRoot, "gn", "out", "gn")
	runCmd(srcRoot, gnPath, "gen", outDir, "--args="+gnArgs)

	// Run ninja
	runCmd(srcRoot, "ninja", "-C", outDir, "cronet_static")
}

func cmdPackage(targets []Target) {
	log("Packaging libraries for %d target(s)", len(targets))

	// Create lib directories
	libDir := filepath.Join(projectRoot, "lib")
	includeDir := filepath.Join(projectRoot, "include")

	os.RemoveAll(libDir)
	os.RemoveAll(includeDir)
	os.MkdirAll(includeDir, 0755)

	// Copy headers
	headers := []struct {
		src  string
		dest string
	}{
		{filepath.Join(srcRoot, "components/cronet/native/include/cronet_c.h"), "cronet_c.h"},
		{filepath.Join(srcRoot, "components/cronet/native/include/cronet_export.h"), "cronet_export.h"},
		{filepath.Join(srcRoot, "components/cronet/native/generated/cronet.idl_c.h"), "cronet.idl_c.h"},
		{filepath.Join(srcRoot, "components/grpc_support/include/bidirectional_stream_c.h"), "bidirectional_stream_c.h"},
	}

	for _, h := range headers {
		copyFile(h.src, filepath.Join(includeDir, h.dest))
	}
	log("Copied headers to include/")

	// Copy libraries for each target
	for _, t := range targets {
		targetDir := filepath.Join(libDir, fmt.Sprintf("%s_%s", t.GOOS, t.ARCH))
		os.MkdirAll(targetDir, 0755)

		srcLib := filepath.Join(srcRoot, fmt.Sprintf("out/cronet-%s-%s/obj/components/cronet/libcronet_static.a", t.OS, t.CPU))
		dstLib := filepath.Join(targetDir, "libcronet.a")

		if _, err := os.Stat(srcLib); os.IsNotExist(err) {
			log("Warning: library not found for %s/%s, skipping", t.GOOS, t.ARCH)
			continue
		}

		copyFile(srcLib, dstLib)
		log("Copied library for %s/%s", t.GOOS, t.ARCH)
	}

	// Generate CGO config files
	generateCGOConfigs(targets)

	log("Package complete!")
}

func generateCGOConfigs(targets []Target) {
	for _, t := range targets {
		filename := fmt.Sprintf("cgo_%s_%s.go", t.GOOS, t.ARCH)
		filepath := filepath.Join(projectRoot, filename)

		var ldflags []string

		// Common flags
		ldflags = append(ldflags, "-L${SRCDIR}/lib/"+t.GOOS+"_"+t.ARCH)
		ldflags = append(ldflags, "-lcronet")
		ldflags = append(ldflags, "-lc++")

		// Platform-specific flags
		switch t.GOOS {
		case "linux":
			ldflags = append(ldflags, "-ldl", "-lpthread", "-lm", "-lresolv")
		case "darwin":
			ldflags = append(ldflags,
				"-framework Security",
				"-framework CoreFoundation",
				"-framework SystemConfiguration",
				"-framework Network",
				"-framework AppKit",
				"-framework CFNetwork",
				"-framework UniformTypeIdentifiers",
			)
		case "windows":
			ldflags = append(ldflags,
				"-lws2_32",
				"-lcrypt32",
				"-lsecur32",
				"-ladvapi32",
				"-lwinhttp",
			)
		case "android":
			ldflags = append(ldflags, "-ldl", "-llog", "-landroid")
		case "ios":
			ldflags = append(ldflags,
				"-framework Security",
				"-framework CoreFoundation",
				"-framework SystemConfiguration",
				"-framework Network",
				"-framework UIKit",
			)
		}

		content := fmt.Sprintf(`//go:build %s && %s

package cronet

// #cgo CFLAGS: -I${SRCDIR}/include
// #cgo LDFLAGS: %s
import "C"
`, t.GOOS, t.ARCH, strings.Join(ldflags, " "))

		if err := os.WriteFile(filepath, []byte(content), 0644); err != nil {
			fatal("failed to write %s: %v", filename, err)
		}
		log("Generated %s", filename)
	}
}

func cmdPublish() {
	log("Publishing to go branch...")

	// Check for uncommitted changes
	output := runCmdOutput(projectRoot, "git", "status", "--porcelain")
	if strings.TrimSpace(output) != "" {
		fatal("uncommitted changes in working directory")
	}

	// Get current branch
	currentBranch := strings.TrimSpace(runCmdOutput(projectRoot, "git", "rev-parse", "--abbrev-ref", "HEAD"))
	if currentBranch != "main" {
		fatal("must be on main branch to publish (current: %s)", currentBranch)
	}

	// Get current commit
	mainCommit := strings.TrimSpace(runCmdOutput(projectRoot, "git", "rev-parse", "HEAD"))

	// Check if go branch exists
	goBranchExists := true
	if err := exec.Command("git", "-C", projectRoot, "rev-parse", "--verify", "go").Run(); err != nil {
		goBranchExists = false
	}

	// Create or checkout go branch
	if goBranchExists {
		runCmd(projectRoot, "git", "checkout", "go")
	} else {
		runCmd(projectRoot, "git", "checkout", "--orphan", "go")
		runCmd(projectRoot, "git", "reset", "--hard")
	}

	// Copy files from main branch
	filesToCopy := []string{
		"*.go",
		"go.mod",
		"go.sum",
		"include/",
		"lib/",
		"naive/",
		"LICENSE",
		"README.md",
	}

	// Clean current state
	runCmd(projectRoot, "git", "rm", "-rf", "--ignore-unmatch", ".")

	// Checkout files from main
	for _, pattern := range filesToCopy {
		exec.Command("git", "-C", projectRoot, "checkout", mainCommit, "--", pattern).Run()
	}

	// Stage and commit
	runCmd(projectRoot, "git", "add", "-A")

	commitMsg := fmt.Sprintf("Build from %s", mainCommit[:8])
	runCmd(projectRoot, "git", "commit", "-m", commitMsg, "--allow-empty")

	// Force push
	runCmd(projectRoot, "git", "push", "-f", "origin", "go")

	// Switch back to main
	runCmd(projectRoot, "git", "checkout", "main")

	log("Published to go branch!")
}

// Helper functions

func log(format string, args ...interface{}) {
	fmt.Printf("[build] "+format+"\n", args...)
}

func fatal(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[build] ERROR: "+format+"\n", args...)
	os.Exit(1)
}

func runCmd(dir string, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("command failed: %s %s: %v", name, strings.Join(args, " "), err)
	}
}

func runCmdOutput(dir string, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("command failed: %s %s: %v", name, strings.Join(args, " "), err)
	}
	return stdout.String()
}

func copyFile(src, dst string) {
	srcFile, err := os.Open(src)
	if err != nil {
		fatal("failed to open %s: %v", src, err)
	}
	defer srcFile.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		fatal("failed to create directory for %s: %v", dst, err)
	}

	dstFile, err := os.Create(dst)
	if err != nil {
		fatal("failed to create %s: %v", dst, err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		fatal("failed to copy %s to %s: %v", src, dst, err)
	}
}

func cmdSync() {
	log("Syncing Chromium cronet components...")

	// Read CHROMIUM_VERSION
	versionFile := filepath.Join(naiveRoot, "CHROMIUM_VERSION")
	versionData, err := os.ReadFile(versionFile)
	if err != nil {
		fatal("failed to read CHROMIUM_VERSION: %v", err)
	}
	version := strings.TrimSpace(string(versionData))
	log("Chromium version: %s", version)

	// Check if components exist and are committed
	cronetDir := filepath.Join(srcRoot, "components", "cronet")
	if _, err := os.Stat(cronetDir); err == nil {
		// Directory exists, check if it's committed
		status := runCmdOutput(naiveRoot, "git", "status", "--porcelain", "src/components/cronet")
		if strings.TrimSpace(status) == "" {
			log("Components already up to date")
			return
		}
	}

	// Components to download
	components := []string{"cronet", "grpc_support", "prefs"}

	for _, name := range components {
		log("Downloading %s...", name)

		url := fmt.Sprintf(
			"https://chromium.googlesource.com/chromium/src/+archive/refs/tags/%s/components/%s.tar.gz",
			version, name)

		destDir := filepath.Join(srcRoot, "components", name)

		// Remove existing directory
		os.RemoveAll(destDir)
		if err := os.MkdirAll(destDir, 0755); err != nil {
			fatal("failed to create directory %s: %v", destDir, err)
		}

		// Download and extract
		if err := downloadAndExtract(url, destDir); err != nil {
			fatal("failed to download %s: %v", name, err)
		}

		log("Downloaded %s", name)
	}

	// Git add and commit
	log("Creating git commit...")
	runCmd(naiveRoot, "git", "add",
		"src/components/cronet",
		"src/components/grpc_support",
		"src/components/prefs")

	commitMsg := fmt.Sprintf(`Add Chromium cronet components (v%s)

Downloaded from Chromium source:
- components/cronet/
- components/grpc_support/
- components/prefs/

Use 'go run ./cmd/build sync' to re-download.`, version)

	runCmd(naiveRoot, "git", "commit", "-m", commitMsg)

	log("Sync complete!")
}

func downloadAndExtract(url, destDir string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, resp.Status)
	}

	// Use tar command to extract (simpler than using archive/tar with gzip)
	cmd := exec.Command("tar", "-xzf", "-", "-C", destDir)
	cmd.Stdin = resp.Body
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tar extraction failed: %w", err)
	}

	return nil
}
