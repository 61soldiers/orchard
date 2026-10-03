package download

import (
	"context"
	"net/http"
	"time"
)

// coverClient fetches artwork. Separate from the streaming client because these
// are small, quick requests that should time out fast.
var coverClient = &http.Client{Timeout: 30 * time.Second}

func newRequest(ctx context.Context, url string) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
}

func httpDo(req *http.Request) (*http.Response, error) { return coverClient.Do(req) }
