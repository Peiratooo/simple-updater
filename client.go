package simpleupdater

import (
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/google/uuid"
)

type Client struct {
	OSS
	DB
}

func New(client *Client) *Client {
	var err error
	client.OSS.Client, err = initOSSClient(&client.OSS)
	if err != nil {
		log.Fatal(err)
	}
	client.DB.Engine, err = initEngine(&client.DB)
	if err != nil {
		log.Fatal(err)
	}
	return client
}

func (c *Client) Push(setup SetupReader) (*Product, error) {
	if setup == nil {
		return nil, errors.New("setup file is nil")
	}

	system, packageType, err := AnalyzePackage(setup)
	if err != nil {
		return nil, err
	}

	var product *Product
	switch packageType {
	case PackageTypeInno:
		product, err = AnalyzeInnoSetupEXE(setup)
	case PackageTypeDMG:
		product, err = AnalyzeSetupDMG(setup)
	default:
		return nil, fmt.Errorf("unsupported package type: %s", packageType)
	}
	if err != nil {
		return nil, err
	}

	product.System = system
	product.PackageType = packageType

	product.Size, err = GenerateSize(setup)
	if err != nil {
		return nil, err
	}
	product.SHA256, err = generateSHA256(setup, product.Size)
	if err != nil {
		return nil, err
	}
	product.FileName, err = GenerateSetupFileName(product)
	if err != nil {
		return nil, err
	}
	product.Data = setup

	uuidStr, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	product.UUID = uuidStr.String()

	if _, err := setup.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("rewind setup before upload: %w", err)
	}
	if err := c.uploadProduct(product); err != nil {
		return nil, err
	}
	if err := c.uploadProduct2DB(*product); err != nil {
		return nil, err
	}

	for i := range product.Files {
		product.Files[i].Data = nil
	}
	product.Data = nil
	product.Bytes = nil
	return product, nil
}

func (c *Client) Compare(system string, appID string, files []File) ([]File, error) {
	latest, err := c.getLatestProduct(system, appID)
	if err != nil {
		return nil, err
	}
	oldByPath := make(map[string]File, len(files))
	for _, file := range files {
		oldByPath[file.Path] = file
	}

	result := make([]File, 0, len(latest.Files))
	for _, latestFile := range latest.Files {
		oldFile, exists := oldByPath[latestFile.Path]
		if !exists || !sameFileState(oldFile, latestFile) {
			result = append(result, latestFile)
		}
	}
	return result, nil
}

func sameFileState(current File, latest File) bool {
	if current.fileType() != latest.fileType() {
		return false
	}

	if latest.fileType() == FileTypeSymlink {
		return current.LinkTarget == latest.LinkTarget
	}

	if current.SHA256 != latest.SHA256 {
		return false
	}
	if latest.Mode != 0 && current.Mode != latest.Mode {
		return false
	}
	return true
}

func (c *Client) DownloadLatestSetup(system, appID string) (*Product, error) {
	latest, err := c.getLatestProduct(system, appID)
	if err != nil {
		return nil, err
	}
	body, err := c.DownloadFile(latest.URL)
	if err != nil {
		return nil, err
	}
	latest.Bytes = body
	return &latest, nil
}

func (c *Client) GetLatestSetupInfo(system, appID string) (*Product, error) {
	latest, err := c.getLatestProduct(system, appID)
	if err != nil {
		return nil, err
	}
	return &latest, nil
}

// GetAllSetupInfo returns all active versions for a system and app ID,
// ordered from oldest to newest by creation time.
func (c *Client) GetAllSetupInfo(system, appID string) ([]Product, error) {
	return c.getAllProducts(system, appID)
}
