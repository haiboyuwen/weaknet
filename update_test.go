package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstallMethodAndAssetNames(t *testing.T) {
	if installMethodForPath("/usr/local/lib/node_modules/weaknet/bin/weaknet-darwin-arm64") != "npm" || installMethodForPath(`C:/Users/a/AppData/Roaming/npm/node_modules/weaknet/bin/weaknet-win32-x64.exe`) != "npm" || installMethodForPath("/Users/a/Downloads/weaknet-darwin-arm64") != "self" {
		t.Fatal("install method detection")
	}
	for _, c := range [][3]string{{"darwin", "arm64", "weaknet-darwin-arm64"}, {"windows", "amd64", "weaknet-win32-x64.exe"}, {"windows", "arm64", "weaknet-win32-arm64.exe"}} {
		if got, err := releaseAssetName(c[0], c[1]); err != nil || got != c[2] {
			t.Fatalf("%v: %s %v", c, got, err)
		}
	}
	if _, err := releaseAssetName("linux", "386"); err == nil {
		t.Fatal("unsupported platform should fail")
	}
	if canonicalVersion("1.0.0") != "v1.0.0" || canonicalVersion("v1.0.0") != "v1.0.0" {
		t.Fatal("canonical version")
	}
}

func TestSelfUpdateVerifiesChecksumAndReplacesBinary(t *testing.T) {
	asset, err := releaseAssetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(binary)
	w.Close()
	sum := sha256.Sum256(gz.Bytes())
	sums := fmt.Sprintf("%s  %s.gz\n", hex.EncodeToString(sum[:]), asset)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/weaknet/latest":
			w.Write([]byte(`{"version":"1.2.3"}`))
		case "/download/v1.2.3/SHA256SUMS":
			w.Write([]byte(sums))
		case "/download/v1.2.3/" + asset + ".gz":
			w.Write(gz.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldBase, oldRegistries := releaseBase, npmRegistries
	releaseBase, npmRegistries = server.URL, []string{server.URL + "/missing", server.URL}
	defer func() { releaseBase, npmRegistries = oldBase, oldRegistries }()

	latest, err := latestVersion(context.Background())
	if err != nil || latest != "v1.2.3" {
		t.Fatalf("latest from fallback registry: %s %v", latest, err)
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "weaknet")
	if err = os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err = installFrom(context.Background(), target, latest); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, binary) {
		t.Fatal("binary not replaced")
	}
	matches, _ := filepath.Glob(target + ".old-*")
	if len(matches) != 1 {
		t.Fatalf("backup missing: %v", matches)
	}
	sums = fmt.Sprintf("%s  %s.gz\n", hex.EncodeToString(make([]byte, 32)), asset)
	if _, err = installFrom(context.Background(), target, latest); err == nil {
		t.Fatal("checksum mismatch must be refused")
	}
}
