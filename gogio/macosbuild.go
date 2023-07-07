package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
)

func buildMac(tmpDir string, bi *buildInfo) error {
	builder := &macBuilder{TempDir: tmpDir}
	builder.DestDir = *destPath
	if builder.DestDir == "" {
		builder.DestDir = bi.pkgPath
	}

	name := bi.name
	if *destPath != "" {
		if filepath.Ext(*destPath) != ".app" {
			return fmt.Errorf("invalid output name %q, it must end with `.app`", *destPath)
		}
		name = filepath.Base(*destPath)
	}
	name = strings.TrimSuffix(name, ".app")

	if bi.appID == "" {
		return errors.New("app id is empty; use -appid to set it")
	}

	if err := builder.setIcon(bi.iconPath); err != nil {
		return err
	}

	if err := builder.setInfo(bi, name); err != nil {
		return fmt.Errorf("can't build the resources: %v", err)
	}

	for _, arch := range bi.archs {
		if err := builder.buildProgram(bi, name, arch); err != nil {
			return err
		}

		if bi.key != "" {
			if err := builder.signProgram(bi, name, arch); err != nil {
				return err
			}
		}
	}

	return nil
}

type macBuilder struct {
	TempDir string
	DestDir string

	Icons        []byte
	Manifest     []byte
	Entitlements []byte
}

func (b *macBuilder) setIcon(path string) (err error) {
	if _, err := os.Stat(path); err != nil {
		return nil
	}

	out := filepath.Join(b.TempDir, "iconset.iconset")
	if err := os.MkdirAll(out, 0777); err != nil {
		return err
	}

	err = buildIcons(out, path, []iconVariant{
		{path: "icon_512x512@2x.png", size: 1024},
		{path: "icon_512x512.png", size: 512},
		{path: "icon_256x256@2x.png", size: 512},
		{path: "icon_256x256.png", size: 256},
		{path: "icon_128x128@2x.png", size: 256},
		{path: "icon_128x128.png", size: 128},
		{path: "icon_64x64@2x.png", size: 128},
		{path: "icon_64x64.png", size: 64},
		{path: "icon_32x32@2x.png", size: 64},
		{path: "icon_32x32.png", size: 32},
		{path: "icon_16x16@2x.png", size: 32},
		{path: "icon_16x16.png", size: 16},
	})

	if err != nil {
		return err
	}

	cmd := exec.Command("iconutil",
		"-c", "icns", out,
		"-o", filepath.Join(b.TempDir, "icon.icns"))
	if _, err := runCmd(cmd); err != nil {
		return err
	}

	b.Icons, err = os.ReadFile(filepath.Join(b.TempDir, "icon.icns"))
	return err
}

func (b *macBuilder) setInfo(buildInfo *buildInfo, name string) error {
	t, err := template.New("manifest").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>{{.Name}}</string>
	<key>CFBundleIconFile</key>
	<string>icon.icns</string>
	<key>CFBundleIdentifier</key>
	<string>{{.Bundle}}</string>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
</dict>
</plist>`)
	if err != nil {
		return err
	}

	var manifest bufferCoff
	if err := t.Execute(&manifest, struct {
		Name, Bundle string
	}{
		Name:   name,
		Bundle: buildInfo.appID,
	}); err != nil {
		return err
	}
	b.Manifest = manifest.Bytes()

	b.Entitlements = []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
<key>com.apple.security.cs.allow-unsigned-executable-memory</key>
<true/>
<key>com.apple.security.cs.allow-jit</key>
<true/>
</dict>
</plist>`)

	return nil
}

func (b *macBuilder) buildProgram(buildInfo *buildInfo, name string, arch string) error {
	dest := b.DestDir
	if len(buildInfo.archs) > 1 {
		dest = filepath.Join(filepath.Dir(b.DestDir), name+"_"+arch+".app")
	}

	for _, path := range []string{"/Contents/MacOS", "/Contents/Resources"} {
		if err := os.MkdirAll(filepath.Join(dest, path), 0755); err != nil {
			return err
		}
	}

	if len(b.Icons) > 0 {
		if err := os.WriteFile(filepath.Join(dest, "/Contents/Resources/icon.icns"), b.Icons, 0755); err != nil {
			return err
		}
	}

	if err := os.WriteFile(filepath.Join(dest, "/Contents/Info.plist"), b.Manifest, 0755); err != nil {
		return err
	}

	cmd := exec.Command(
		"go",
		"build",
		"-ldflags="+buildInfo.ldflags,
		"-tags="+buildInfo.tags,
		"-o", filepath.Join(dest, "/Contents/MacOS/"+name),
		buildInfo.pkgPath,
	)
	cmd.Env = append(
		os.Environ(),
		"GOOS=darwin",
		"GOARCH="+arch,
		"CGO_ENABLED=1", // Required to cross-compile between AMD/ARM
	)
	_, err := runCmd(cmd)
	return err
}

func (b *macBuilder) signProgram(buildInfo *buildInfo, name string, arch string) error {
	dest := b.DestDir
	if len(buildInfo.archs) > 1 {
		dest = filepath.Join(filepath.Dir(b.DestDir), name+"_"+arch+".app")
	}

	options := filepath.Join(b.TempDir, "ent.ent")
	if err := os.WriteFile(options, b.Entitlements, 0777); err != nil {
		return err
	}

	xattr := exec.Command("xattr", "-rc", dest)
	if _, err := runCmd(xattr); err != nil {
		return err
	}

	cmd := exec.Command(
		"codesign",
		"--deep",
		"--force",
		"--options", "runtime",
		"--entitlements", options,
		"--sign", buildInfo.key,
		dest,
	)
	_, err := runCmd(cmd)
	return err
}
