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
	OS   string // gn target_os: linux, mac, win, android, ios
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

// getExtraFlags returns the EXTRA_FLAGS for a target
func getExtraFlags(t Target) string {
	flags := []string{
		fmt.Sprintf(`target_os="%s"`, t.OS),
		fmt.Sprintf(`target_cpu="%s"`, t.CPU),
	}
	return strings.Join(flags, " ")
}

// runGetClang runs naiveproxy's get-clang.sh with appropriate EXTRA_FLAGS
func runGetClang(t Target) {
	// For cross-compilation on Linux, we need to also build host sysroot first
	// because GN needs host sysroot in addition to target sysroot
	hostOS := runtime.GOOS
	hostCPU := hostToCPU(runtime.GOARCH)
	if hostOS == "linux" && (t.OS == "linux" || t.OS == "android") && t.CPU != hostCPU {
		// Run get-clang.sh with host target to ensure host sysroot is downloaded
		hostFlags := fmt.Sprintf(`target_os="linux" target_cpu="%s"`, hostCPU)
		log("Running get-clang.sh for host sysroot with EXTRA_FLAGS=%s", hostFlags)
		cmd := exec.Command("bash", "./get-clang.sh")
		cmd.Dir = srcRoot
		cmd.Env = append(os.Environ(), "EXTRA_FLAGS="+hostFlags)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fatal("get-clang.sh (host) failed: %v", err)
		}

		// Create symlink for host sysroot so GN can find it at the default location
		hostSysrootSrc := filepath.Join(srcRoot, "out/sysroot-build/bullseye/bullseye_amd64_staging")
		hostSysrootDst := filepath.Join(srcRoot, "build/linux/debian_bullseye_amd64-sysroot")
		if _, err := os.Stat(hostSysrootDst); os.IsNotExist(err) {
			log("Creating symlink for host sysroot: %s -> %s", hostSysrootDst, hostSysrootSrc)
			if err := os.Symlink(hostSysrootSrc, hostSysrootDst); err != nil {
				fatal("failed to create host sysroot symlink: %v", err)
			}
		}
	}

	extraFlags := getExtraFlags(t)
	log("Running get-clang.sh with EXTRA_FLAGS=%s", extraFlags)

	cmd := exec.Command("bash", "./get-clang.sh")
	cmd.Dir = srcRoot
	cmd.Env = append(os.Environ(), "EXTRA_FLAGS="+extraFlags)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fatal("get-clang.sh failed: %v", err)
	}
}

// hostToCPU converts Go GOARCH to GN cpu
func hostToCPU(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	case "386":
		return "x86"
	case "arm":
		return "arm"
	default:
		return goarch
	}
}

func buildTarget(t Target) {
	// Run get-clang.sh to ensure toolchain is available
	runGetClang(t)

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
		"enable_dsyms=false",
		fmt.Sprintf("target_os=\"%s\"", t.OS),
		fmt.Sprintf("target_cpu=\"%s\"", t.CPU),
	}

	// Platform-specific args
	switch t.OS {
	case "mac":
		args = append(args, "use_sysroot=false")
	case "linux":
		// Sysroot is handled by get-clang.sh, use the naiveproxy path
		sysrootArch := map[string]string{"x64": "amd64", "arm64": "arm64"}[t.CPU]
		sysrootDir := fmt.Sprintf("out/sysroot-build/bullseye/bullseye_%s_staging", sysrootArch)
		args = append(args, "use_sysroot=true", fmt.Sprintf("target_sysroot=\"//%s\"", sysrootDir))
		if t.CPU == "x64" {
			args = append(args, "use_cfi_icall=false")
		}
	case "win":
		args = append(args, "use_sysroot=false")
	case "android":
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

	// Determine GN path
	gnPath := filepath.Join(srcRoot, "gn", "out", "gn")
	if runtime.GOOS == "windows" {
		gnPath += ".exe"
	}

	// Run gn gen
	log("Running: gn gen %s", outDir)
	gnCmd := exec.Command(gnPath, "gen", outDir, "--args="+gnArgs)
	gnCmd.Dir = srcRoot
	gnCmd.Stdout = os.Stdout
	gnCmd.Stderr = os.Stderr
	// On Windows, use system Visual Studio instead of depot_tools
	if runtime.GOOS == "windows" {
		gnCmd.Env = append(os.Environ(), "DEPOT_TOOLS_WIN_TOOLCHAIN=0")
	}
	if err := gnCmd.Run(); err != nil {
		fatal("gn gen failed: %v", err)
	}

	// Run ninja
	log("Running: ninja -C %s cronet_static", outDir)
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
