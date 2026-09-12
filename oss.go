package simpleupdater

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

type OSS struct {
	ID       string //
	Key      string //
	Endpoint string //
	Bucket   string //
	Folder   string //
	Prefix   string // Optional download URL prefix; empty uses the OSS SDK.
	Client   *oss.Client
}

func initOSSClient(data *OSS) (*oss.Client, error) {
	client, err := oss.New(data.Endpoint, data.ID, data.Key)
	if err != nil {
		log.Fatal(err)
		return nil, err
	}
	return client, nil
}

func (c *Client) uploadFile(file io.Reader, path string) error {
	bucket, err := c.Client.Bucket(c.Bucket)
	if err != nil {
		return err
	}

	return bucket.PutObject(path, file)
}

func (c *Client) uploadProduct(product *Product) error {
	if product == nil {
		return errors.New("product is nil")
	}
	if product.Data == nil {
		return errors.New("product data is nil")
	}

	prefix := path.Join(c.Folder, product.Version+"-"+product.System+"-"+product.UUID)
	productKey := path.Join(prefix, product.FileName)

	if err := c.uploadFile(product.Data, productKey); err != nil {
		return err
	}
	product.URL = productKey

	for i := range product.Files {
		file := &product.Files[i]
		if file.fileType() == FileTypeSymlink {
			continue
		}
		fileKey := path.Join(prefix, file.Path)
		fileReader := bytes.NewReader(file.Data)
		if err := c.uploadFile(fileReader, fileKey); err != nil {
			return err
		}
		file.URL = fileKey
	}
	return nil
}

// DownloadFile accepts an object key, using Prefix when configured.
func (c *Client) DownloadFile(path string) ([]byte, error) {
	if c == nil {
		return nil, errors.New("client is nil")
	}
	if strings.TrimSpace(c.Prefix) != "" {
		address, err := c.downloadURL(path)
		if err != nil {
			return nil, err
		}
		client := &http.Client{Timeout: 5 * time.Minute}
		response, err := client.Get(address)
		if err != nil {
			return nil, fmt.Errorf("download object: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("download object: HTTP %d", response.StatusCode)
		}
		return io.ReadAll(response.Body)
	}
	if c.Client == nil {
		return nil, errors.New("OSS client is nil")
	}
	bucket, err := c.Client.Bucket(c.Bucket)
	if err != nil {
		return nil, err
	}
	body, err := bucket.GetObject(path)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	return io.ReadAll(body)
}

func (c *Client) DownloadPatch(files []File) ([]byte, error) {
	if c == nil || (c.Client == nil && strings.TrimSpace(c.Prefix) == "") {
		return nil, errors.New("OSS client is nil")
	}

	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)

	manifest, err := json.Marshal(files)
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := writeTarEntry(tarWriter, "manifest.json", manifest); err != nil {
		return nil, err
	}
	archiveNames := map[string]struct{}{"manifest.json": {}}

	for _, file := range files {
		name, err := cleanArchiveName(file.Path)
		if err != nil {
			return nil, err
		}
		if _, exists := archiveNames[name]; exists {
			return nil, fmt.Errorf("duplicate archive file name: %s", name)
		}
		archiveNames[name] = struct{}{}

		if file.fileType() == FileTypeSymlink {
			if err := validateSymlinkTarget(name, file.LinkTarget); err != nil {
				return nil, err
			}
			mode := int64(file.Mode)
			if mode == 0 {
				mode = 0o777
			}
			if err := tarWriter.WriteHeader(&tar.Header{
				Name:     name,
				Mode:     mode,
				Typeflag: tar.TypeSymlink,
				Linkname: file.LinkTarget,
			}); err != nil {
				return nil, fmt.Errorf("write symlink tar header %s: %w", name, err)
			}
			continue
		}

		data, err := c.DownloadFile(file.URL)
		if err != nil {
			return nil, fmt.Errorf("download file %s: %w", file.Path, err)
		}
		if uint64(len(data)) != file.Size {
			return nil, fmt.Errorf("size mismatch for %s: got %d, want %d", name, len(data), file.Size)
		}
		if file.SHA256 != "" {
			digest := sha256.Sum256(data)
			actual := hex.EncodeToString(digest[:])
			if actual != file.SHA256 {
				return nil, fmt.Errorf("sha256 mismatch for %s: got %s, want %s", name, actual, file.SHA256)
			}
		}

		mode := int64(file.Mode)
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{
			Name: name,
			Mode: mode,
			Size: int64(len(data)),
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return nil, fmt.Errorf("write tar header %s: %w", name, err)
		}
		if _, err := tarWriter.Write(data); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
	}

	if err := tarWriter.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gzipWriter.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	return output.Bytes(), nil
}

func cleanArchiveName(filePath string) (string, error) {
	name := path.Clean(filepath.ToSlash(filePath))
	if filepath.IsAbs(filePath) || name == "." || name == ".." ||
		strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("invalid file path: %s", filePath)
	}
	return name, nil
}

func validateSymlinkTarget(name string, target string) error {
	if target == "" || path.IsAbs(target) {
		return fmt.Errorf("invalid symlink target for %s: %s", name, target)
	}
	resolved := path.Clean(path.Join(path.Dir(name), target))
	if resolved == ".." || strings.HasPrefix(resolved, "../") || strings.HasPrefix(resolved, "/") {
		return fmt.Errorf("symlink target escapes archive root for %s: %s", name, target)
	}
	return nil
}

func writeTarEntry(writer *tar.Writer, name string, data []byte) error {
	if err := writer.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o644,
		Size: int64(len(data)),
	}); err != nil {
		return fmt.Errorf("write tar header %s: %w", name, err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("write tar entry %s: %w", name, err)
	}
	return nil
}

// downloadURL appends the literal key without cleaning or parsing it.
func (c *Client) downloadURL(objectKey string) (string, error) {
	prefix := strings.TrimSpace(c.Prefix)
	if !strings.Contains(prefix, "://") {
		prefix = "https://" + prefix
	}
	address, err := url.Parse(prefix)
	if err != nil {
		return "", fmt.Errorf("parse OSS prefix: %w", err)
	}
	if (address.Scheme != "https" && address.Scheme != "http") ||
		address.Hostname() == "" || address.User != nil ||
		address.RawQuery != "" || address.ForceQuery || address.Fragment != "" || strings.Contains(prefix, "#") {
		return "", errors.New("OSS prefix must be an HTTP(S) domain or URL without credentials, query or fragment")
	}
	address.Path = strings.TrimRight(address.Path, "/") + "/" + objectKey
	address.RawPath = ""
	return address.String(), nil
}
