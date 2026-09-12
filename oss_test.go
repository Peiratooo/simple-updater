package simpleupdater

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func TestDownloadRoutingAndStoredKeys(t *testing.T) {
	key := "releases/100% #中文.exe"
	bucketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/demo-bucket/"+key {
			t.Errorf("bucket path = %q", r.URL.Path)
		}
		io.WriteString(w, "bucket")
	}))
	defer bucketServer.Close()
	sdk, err := oss.New(bucketServer.URL, "test-id", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{OSS: OSS{Client: sdk, Bucket: "demo-bucket", Folder: "releases"}}
	for _, label := range []string{"first", "second"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/downloads/"+key {
				t.Errorf("custom path = %q", r.URL.Path)
			}
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected query %q", r.URL.RawQuery)
			}
			if r.Header.Get("Authorization") != "" {
				t.Error("OSS credentials sent to CDN")
			}
			io.WriteString(w, label)
		}))
		client.Prefix = server.URL + "/downloads/"
		data, err := client.DownloadFile(key)
		server.Close()
		if err != nil || string(data) != label {
			t.Fatalf("download = %q, %v", data, err)
		}
	}
	client.Prefix = ""
	data, err := client.DownloadFile(key)
	if err != nil || string(data) != "bucket" {
		t.Fatalf("bucket = %q, %v", data, err)
	}
	client.Prefix = "https://unused.example/downloads"
	product := &Product{Version: "1", System: "windows", UUID: "id", FileName: "setup.exe", Data: bytes.NewReader([]byte("setup")), Files: []File{{Path: "app.exe", Data: []byte("app")}}}
	if err := client.uploadProduct(product); err != nil {
		t.Fatal(err)
	}
	if product.URL != "releases/1-windows-id/setup.exe" || product.Files[0].URL != "releases/1-windows-id/app.exe" {
		t.Fatalf("stored URLs must remain keys: %q, %q", product.URL, product.Files[0].URL)
	}
}

func TestCustomDownloadPatchAndHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, "abc")
	}))
	defer server.Close()
	client := &Client{OSS: OSS{Prefix: server.URL}}
	patch, err := client.DownloadPatch([]File{{Path: "app.exe", URL: "app.exe", Size: 3}})
	if err != nil || len(patch) == 0 {
		t.Fatalf("patch = %d bytes, %v", len(patch), err)
	}
	if _, err := client.DownloadFile("missing"); err == nil {
		t.Fatal("expected HTTP error")
	}
	for _, prefix := range []string{"ftp://example.com", "https://example.com?x=1", "https://user:pass@example.com", "https://example.com/#fragment"} {
		client.Prefix = prefix
		if _, err := client.DownloadFile("app.exe"); err == nil {
			t.Errorf("accepted invalid prefix %q", prefix)
		}
	}
}
