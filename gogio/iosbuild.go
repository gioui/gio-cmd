// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"golang.org/x/sync/errgroup"
)

const (
	minIOSVersion = 10
	// Some Metal features require tvOS 11
	minTVOSVersion = 11
	// Metal is available from iOS 8 on devices, yet from version 13 on the
	// simulator.
	minSimulatorVersion = 13
)

func buildIOS(tmpDir, target string, bi *buildInfo) error {
	appName := bi.name
	switch *buildMode {
	case "archive":
		framework := *destPath
		if framework == "" {
			framework = fmt.Sprintf("%s.framework", UppercaseName(appName))
		}
		return archiveIOS(tmpDir, target, framework, bi)
	case "exe":
		out := *destPath
		if out == "" {
			out = appName + ".ipa"
		}
		forDevice := strings.HasSuffix(out, ".ipa")
		// Filter out unsupported architectures.
		for i := len(bi.archs) - 1; i >= 0; i-- {
			switch bi.archs[i] {
			case "arm", "arm64":
				if forDevice {
					continue
				}
			case "386", "amd64":
				if !forDevice {
					continue
				}
			}

			bi.archs = append(bi.archs[:i], bi.archs[i+1:]...)
		}
		if !forDevice && !strings.HasSuffix(out, ".app") {
			return fmt.Errorf("the specified output directory %q does not end in .app or .ipa", out)
		}
		if !forDevice {
			return exeIOS(tmpDir, target, out, bi)
		}
		payload := filepath.Join(tmpDir, "Payload")
		appDir := filepath.Join(payload, appName+".app")
		if err := os.MkdirAll(appDir, 0755); err != nil {
			return err
		}
		if err := exeIOS(tmpDir, target, appDir, bi); err != nil {
			return err
		}
		if err := signIOS(bi, tmpDir, appDir); err != nil {
			return err
		}
		return zipDir(out, tmpDir, "Payload")
	default:
		panic("unreachable")
	}
}

func signIOS(bi *buildInfo, tmpDir, app string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	provPattern := filepath.Join(home, "Library", "MobileDevice", "Provisioning Profiles", "*.mobileprovision")
	provisions, err := filepath.Glob(provPattern)
	if err != nil {
		return err
	}
	provInfo := filepath.Join(tmpDir, "provision.plist")
	var avail []string
	for _, prov := range provisions {
		// Decode the provision file to a plist.
		_, err := runCmd(exec.Command("security", "cms", "-D", "-i", prov, "-o", provInfo))
		if err != nil {
			return err
		}
		expUnix, err := runCmd(exec.Command("/usr/libexec/PlistBuddy", "-c", "Print:ExpirationDate", provInfo))
		if err != nil {
			return err
		}
		exp, err := time.Parse(time.UnixDate, expUnix)
		if err != nil {
			return fmt.Errorf("sign: failed to parse expiration date from %q: %v", prov, err)
		}
		if exp.Before(time.Now()) {
			continue
		}
		appIDPrefix, err := runCmd(exec.Command("/usr/libexec/PlistBuddy", "-c", "Print:ApplicationIdentifierPrefix:0", provInfo))
		if err != nil {
			return err
		}
		provAppID, err := runCmd(exec.Command("/usr/libexec/PlistBuddy", "-c", "Print:Entitlements:application-identifier", provInfo))
		if err != nil {
			return err
		}
		expAppID := fmt.Sprintf("%s.%s", appIDPrefix, bi.appID)
		avail = append(avail, provAppID)
		if expAppID != provAppID {
			continue
		}
		// Copy provisioning file.
		embedded := filepath.Join(app, "embedded.mobileprovision")
		if err := copyFile(embedded, prov); err != nil {
			return err
		}
		certDER, err := runCmdRaw(exec.Command("/usr/libexec/PlistBuddy", "-c", "Print:DeveloperCertificates:0", provInfo))
		if err != nil {
			return err
		}
		// Omit trailing newline.
		certDER = certDER[:len(certDER)-1]
		entitlements, err := runCmd(exec.Command("/usr/libexec/PlistBuddy", "-x", "-c", "Print:Entitlements", provInfo))
		if err != nil {
			return err
		}
		entFile := filepath.Join(tmpDir, "entitlements.plist")
		if err := os.WriteFile(entFile, []byte(entitlements), 0660); err != nil {
			return err
		}
		identity := sha1.Sum(certDER)
		idHex := hex.EncodeToString(identity[:])
		_, err = runCmd(exec.Command("codesign", "-s", idHex, "-v", "--entitlements", entFile, app))
		return err
	}
	return fmt.Errorf("sign: no valid provisioning profile found for bundle id %q among %v", bi.appID, avail)
}

func exeIOS(tmpDir, target, app string, bi *buildInfo) error {
	if bi.appID == "" {
		return errors.New("app id is empty; use -appid to set it")
	}
	if err := os.RemoveAll(app); err != nil {
		return err
	}
	if err := os.Mkdir(app, 0755); err != nil {
		return err
	}
	appName := UppercaseName(bi.name)
	exe := filepath.Join(app, appName)
	lipo := exec.Command("xcrun", "lipo", "-o", exe, "-create")
	var builds errgroup.Group
	for _, a := range bi.archs {
		clang, cflags, err := iosCompilerFor(target, a, bi.minsdk)
		if err != nil {
			return err
		}
		cflags = append(cflags,
			"-fobjc-arc",
		)
		cflagsLine := strings.Join(cflags, " ")
		exeSlice := filepath.Join(tmpDir, "app-"+a)
		lipo.Args = append(lipo.Args, exeSlice)
		compile := exec.Command(
			"go",
			"build",
			"-ldflags=-s -w "+bi.ldflags,
			"-o", exeSlice,
			"-tags", bi.tags,
			bi.pkgPath,
		)
		compile.Env = append(
			os.Environ(),
			"GOOS=ios",
			"GOARCH="+a,
			"CGO_ENABLED=1",
			"CC="+clang,
			"CGO_CFLAGS="+cflagsLine,
			"CGO_LDFLAGS=-lresolv "+cflagsLine,
		)
		builds.Go(func() error {
			_, err := runCmd(compile)
			return err
		})
	}
	if err := builds.Wait(); err != nil {
		return err
	}
	if _, err := runCmd(lipo); err != nil {
		return err
	}
	infoPlist, err := buildInfoPlist(bi)
	if err != nil {
		return err
	}
	plistFile := filepath.Join(app, "Info.plist")
	if err := os.WriteFile(plistFile, []byte(infoPlist), 0660); err != nil {
		return err
	}
	if _, err := os.Stat(bi.iconPath); err == nil {
		assetPlist, err := iosIcons(bi, tmpDir, app, bi.iconPath)
		if err != nil {
			return err
		}
		// Merge assets plist with Info.plist
		cmd := exec.Command(
			"/usr/libexec/PlistBuddy",
			"-c", "Merge "+assetPlist,
			plistFile,
		)
		if _, err := runCmd(cmd); err != nil {
			return err
		}
	}
	if _, err := runCmd(exec.Command("plutil", "-convert", "binary1", plistFile)); err != nil {
		return err
	}
	return nil
}

// iosIcons builds an asset catalog and compile it with the Xcode command actool.
// iosIcons returns the asset plist file to be merged into Info.plist.
func iosIcons(bi *buildInfo, tmpDir, appDir, icon string) (string, error) {
	assets := filepath.Join(tmpDir, "Assets.xcassets")
	if err := os.Mkdir(assets, 0700); err != nil {
		return "", err
	}
	appIcon := filepath.Join(assets, "AppIcon.appiconset")
	err := buildIcons(appIcon, icon, []iconVariant{
		{path: "ios_2x.png", size: 120},
		{path: "ios_3x.png", size: 180},
		// The App Store icon is not allowed to contain
		// transparent pixels.
		{path: "ios_store.png", size: 1024, fill: true},
	})
	if err != nil {
		return "", err
	}
	contentJson := `{
	"images" : [
		{
			"size" : "60x60",
			"idiom" : "iphone",
			"filename" : "ios_2x.png",
			"scale" : "2x"
		},
		{
			"size" : "60x60",
			"idiom" : "iphone",
			"filename" : "ios_3x.png",
			"scale" : "3x"
		},
		{
			"size" : "1024x1024",
			"idiom" : "ios-marketing",
			"filename" : "ios_store.png",
			"scale" : "1x"
		}
	]
}`
	contentFile := filepath.Join(appIcon, "Contents.json")
	if err := os.WriteFile(contentFile, []byte(contentJson), 0600); err != nil {
		return "", err
	}
	assetPlist := filepath.Join(tmpDir, "assets.plist")

	minsdk := bi.minsdk
	if minsdk == 0 {
		minsdk = minIOSVersion
	}
	compile := exec.Command(
		"actool",
		"--compile", appDir,
		"--platform", iosPlatformFor(bi.target),
		"--minimum-deployment-target", strconv.Itoa(minsdk),
		"--app-icon", "AppIcon",
		"--output-partial-info-plist", assetPlist,
		assets)
	_, err = runCmd(compile)
	return assetPlist, err
}

func buildInfoPlist(bi *buildInfo) (string, error) {
	appName := UppercaseName(bi.name)
	platform := iosPlatformFor(bi.target)
	var supportPlatform string
	switch bi.target {
	case "ios":
		supportPlatform = "iPhoneOS"
	case "tvos":
		supportPlatform = "AppleTVOS"
	}

	manifestSrc := struct {
		AppName         string
		AppID           string
		Version         string
		VersionCode     uint32
		Platform        string
		MinVersion      int
		SupportPlatform string
		Schemes         []string
	}{
		AppName:         appName,
		AppID:           bi.appID,
		Version:         bi.version.String(),
		VersionCode:     bi.version.VersionCode,
		Platform:        platform,
		MinVersion:      minIOSVersion,
		SupportPlatform: supportPlatform,
		Schemes:         bi.schemes,
	}

	tmpl, err := template.New("manifest").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key>
	<string>en</string>
	<key>CFBundleExecutable</key>
	<string>{{.AppName}}</string>
	<key>CFBundleIdentifier</key>
	<string>{{.AppID}}</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleName</key>
	<string>{{.AppName}}</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>{{.Version}}</string>
	<key>CFBundleVersion</key>
	<string>{{.VersionCode}}</string>
	<key>UILaunchStoryboardName</key>
	<string>LaunchScreen</string>
	<key>UIRequiredDeviceCapabilities</key>
	<array><string>arm64</string></array>
	<key>DTPlatformName</key>
	<string>{{.Platform}}</string>
	<key>DTPlatformVersion</key>
	<string>12.4</string>
	<key>MinimumOSVersion</key>
	<string>{{.MinVersion}}</string>
	<key>UIDeviceFamily</key>
	<array>
		<integer>1</integer>
		<integer>2</integer>
	</array>
	<key>CFBundleSupportedPlatforms</key>
	<array>
		<string>{{.SupportPlatform}}</string>
	</array>
	<key>UISupportedInterfaceOrientations</key>
	<array>
		<string>UIInterfaceOrientationPortrait</string>
		<string>UIInterfaceOrientationLandscapeLeft</string>
		<string>UIInterfaceOrientationLandscapeRight</string>
	</array>
	<key>DTCompiler</key>
	<string>com.apple.compilers.llvm.clang.1_0</string>
	<key>DTPlatformBuild</key>
	<string>16G73</string>
	<key>DTSDKBuild</key>
	<string>16G73</string>
	<key>DTSDKName</key>
	<string>{{.Platform}}12.4</string>
	<key>DTXcode</key>
	<string>1030</string>
	<key>DTXcodeBuild</key>
	<string>10G8</string>
    {{if .Schemes}}
	<key>CFBundleURLTypes</key>
	<array>
	  {{range .Schemes}}
	  <dict>
		<key>CFBundleURLSchemes</key>
		<array>
		  <string>{{.}}</string>
		</array>
	  </dict>
	  {{end}}
	</array>
    {{end}}
</dict>
</plist>`)
	if err != nil {
		return "", err
	}

	var manifestBuffer bytes.Buffer
	if err := tmpl.Execute(&manifestBuffer, manifestSrc); err != nil {
		return "", err
	}

	return manifestBuffer.String(), nil
}

func iosPlatformFor(target string) string {
	switch target {
	case "ios":
		return "iphoneos"
	case "tvos":
		return "appletvos"
	default:
		panic("invalid platform " + target)
	}
}

func archiveIOS(tmpDir, target, frameworkRoot string, bi *buildInfo) error {
	framework := filepath.Base(frameworkRoot)
	const suf = ".framework"
	if !strings.HasSuffix(framework, suf) {
		return fmt.Errorf("the specified output %q does not end in '.framework'", frameworkRoot)
	}
	framework = framework[:len(framework)-len(suf)]
	if err := os.RemoveAll(frameworkRoot); err != nil {
		return err
	}
	frameworkDir := filepath.Join(frameworkRoot, "Versions", "A")
	for _, dir := range []string{"Headers", "Modules"} {
		p := filepath.Join(frameworkDir, dir)
		if err := os.MkdirAll(p, 0755); err != nil {
			return err
		}
	}
	symlinks := [][2]string{
		{"Versions/Current/Headers", "Headers"},
		{"Versions/Current/Modules", "Modules"},
		{"Versions/Current/" + framework, framework},
		{"A", filepath.Join("Versions", "Current")},
	}
	for _, l := range symlinks {
		if err := os.Symlink(l[0], filepath.Join(frameworkRoot, l[1])); err != nil && !os.IsExist(err) {
			return err
		}
	}
	exe := filepath.Join(frameworkDir, framework)
	lipo := exec.Command("xcrun", "lipo", "-o", exe, "-create")
	var builds errgroup.Group
	tags := bi.tags
	for _, a := range bi.archs {
		clang, cflags, err := iosCompilerFor(target, a, bi.minsdk)
		if err != nil {
			return err
		}
		lib := filepath.Join(tmpDir, "gio-"+a)
		cmd := exec.Command(
			"go",
			"build",
			"-ldflags=-s -w "+bi.ldflags,
			"-buildmode=c-archive",
			"-o", lib,
			"-tags", tags,
			bi.pkgPath,
		)
		lipo.Args = append(lipo.Args, lib)
		cflagsLine := strings.Join(cflags, " ")
		cmd.Env = append(
			os.Environ(),
			"GOOS=ios",
			"GOARCH="+a,
			"CGO_ENABLED=1",
			"CC="+clang,
			"CGO_CFLAGS="+cflagsLine,
			"CGO_LDFLAGS="+cflagsLine,
		)
		builds.Go(func() error {
			_, err := runCmd(cmd)
			return err
		})
	}
	if err := builds.Wait(); err != nil {
		return err
	}
	if _, err := runCmd(lipo); err != nil {
		return err
	}
	appDir, err := runCmd(exec.Command("go", "list", "-tags", tags, "-f", "{{.Dir}}", "gioui.org/app/"))
	if err != nil {
		return err
	}
	headerDst := filepath.Join(frameworkDir, "Headers", framework+".h")
	headerSrc := filepath.Join(appDir, "framework_ios.h")
	if err := copyFile(headerDst, headerSrc); err != nil {
		return err
	}
	module := fmt.Sprintf(`framework module "%s" {
    header "%[1]s.h"

    export *
}`, framework)
	moduleFile := filepath.Join(frameworkDir, "Modules", "module.modulemap")
	return os.WriteFile(moduleFile, []byte(module), 0644)
}

func iosCompilerFor(target, arch string, minsdk int) (string, []string, error) {
	var (
		platformSDK string
		platformOS  string
	)
	switch target {
	case "ios":
		platformOS = "ios"
		platformSDK = "iphone"
	case "tvos":
		platformOS = "tvos"
		platformSDK = "appletv"
	}
	switch arch {
	case "arm", "arm64":
		platformSDK += "os"
		if minsdk == 0 {
			minsdk = minIOSVersion
			if target == "tvos" {
				minsdk = minTVOSVersion
			}
		}
	case "386", "amd64":
		platformOS += "-simulator"
		platformSDK += "simulator"
		if minsdk == 0 {
			minsdk = minSimulatorVersion
		}
	default:
		return "", nil, fmt.Errorf("unsupported -arch: %s", arch)
	}
	sdkPath, err := runCmd(exec.Command("xcrun", "--sdk", platformSDK, "--show-sdk-path"))
	if err != nil {
		return "", nil, err
	}
	clang, err := runCmd(exec.Command("xcrun", "--sdk", platformSDK, "--find", "clang"))
	if err != nil {
		return "", nil, err
	}
	cflags := []string{
		"-fembed-bitcode",
		"-arch", allArchs[arch].iosArch,
		"-isysroot", sdkPath,
		"-m" + platformOS + "-version-min=" + strconv.Itoa(minsdk),
	}
	return clang, cflags, nil
}

func zipDir(dst, base, dir string) (err error) {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	zipf := zip.NewWriter(f)
	err = filepath.Walk(filepath.Join(base, dir), func(path string, f os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if f.IsDir() {
			return nil
		}
		rel := filepath.ToSlash(path[len(base)+1:])
		entry, err := zipf.Create(rel)
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(entry, src)
		return err
	})
	if err != nil {
		return err
	}
	return zipf.Close()
}
