package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Load reads a catalog file. A missing file returns (nil, nil).
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Datasets == nil {
		c.Datasets = NewDatasets()
	}
	return &c, nil
}

// Save writes the catalog atomically: write a temp file in the same
// directory, fsync, then rename over the target. A crash mid-write leaves the
// previous catalog intact instead of a truncated file. encoding/json sorts
// map keys, and locations/partitions are kept sorted, so unchanged data
// produces byte-identical output.
func Save(path string, c *Catalog) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".catalog-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
