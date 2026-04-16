// SPDX-License-Identifier: Unlicense OR MIT

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAndroidExtraJarsIncludesAndroidSubdirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rootJar := filepath.Join(dir, "crew.jar")
	androidJar := filepath.Join(dir, "android", "vault.jar")
	writeTestFile(t, rootJar, "root")
	writeTestFile(t, androidJar, "android")

	got, err := androidExtraJars(dir)
	if err != nil {
		t.Fatalf("androidExtraJars() error = %v", err)
	}
	want := []string{rootJar, androidJar}
	for _, jar := range want {
		if !containsString(got, jar) {
			t.Fatalf("androidExtraJars() = %v, want %q included", got, jar)
		}
	}
}

func TestRenderAndroidManifestIncludesOptionalSnippets(t *testing.T) {
	t.Parallel()

	manifest, err := renderAndroidManifest(manifestData{
		AppID:        "org.example.heist",
		Version:      Semver{Major: 1, Minor: 2, Patch: 3, VersionCode: 4},
		MinSDK:       28,
		TargetSDK:    35,
		Permissions:  []string{"android.permission.INTERNET"},
		Features:     []string{`name="android.hardware.fingerprint"`},
		IconSnip:     `android:icon="@mipmap/ic_launcher"`,
		AppName:      "Bellagio Crew",
		ManifestSnip: "\n\t<uses-permission android:name=\"android.permission.POST_NOTIFICATIONS\" />\n",
		AppSnip:      "\n\t\t<service android:name=\".CrewService\" />\n",
	})
	if err != nil {
		t.Fatalf("renderAndroidManifest() error = %v", err)
	}

	got := string(manifest)
	for _, want := range []string{
		`android.permission.POST_NOTIFICATIONS`,
		`<service android:name=".CrewService" />`,
		`android.permission.INTERNET`,
		`android.hardware.fingerprint`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("renderAndroidManifest() missing %q in %s", want, got)
		}
	}
}

func TestCopyTreeCopiesNestedResources(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "android", "res")
	dst := filepath.Join(t.TempDir(), "merged")
	resourcePath := filepath.Join(src, "xml", "heist_service.xml")
	writeTestFile(t, resourcePath, "<service />")

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree() error = %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dst, "xml", "heist_service.xml"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(got) != "<service />" {
		t.Fatalf("copyTree() copied %q, want %q", string(got), "<service />")
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
