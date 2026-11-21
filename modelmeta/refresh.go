package modelmeta

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dailz1/go-agent/modelmeta/internal/source"
)

var timeNow = time.Now

// Fetch obtains both fixed sources under one ten-second deadline.
// A nil client uses an internal HTTP client. A provided client is copied;
// redirects and cookie jars are disabled on that copy. No cache is written.
// Failure returns a nil catalog and leaves existing catalogs untouched.
func Fetch(ctx context.Context, client *http.Client) (*Catalog, error) {
	if ctx == nil {
		return nil, fmt.Errorf("fetch context: %w", ErrInvalidArgument)
	}
	s, err := source.Fetch(ctx, client, timeNow)
	if err != nil {
		return nil, fmt.Errorf("fetch catalog: %w", err)
	}
	return catalog(s), nil
}
