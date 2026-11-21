package modelmeta

import (
	_ "embed"
	"fmt"

	"github.com/dailz1/go-agent/modelmeta/internal/source"
)

//go:embed data/windows.json
var bundled []byte

// Bundled decodes the shipped format-2 snapshot without network or file I/O.
// Expiry is checked when exporting or querying windows, not when loading.
func Bundled() (*Catalog, error) {
	s, err := source.ParseSnapshot(bundled)
	if err != nil {
		return nil, fmt.Errorf("load bundled snapshot: %w", err)
	}
	return catalog(s), nil
}
