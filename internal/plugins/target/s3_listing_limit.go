package target

import (
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/zekihan/mailvault/internal/plugins"
)

// MaxKeys is advisory for a hostile endpoint. Cap the XML response before the
// SDK deserializer can allocate an arbitrary number of keys or a huge token.
const maxS3ListingPageBytes = 2 << 20
const s3ListingPageSize = 128

type listingHTTPClient struct{ next s3.HTTPClient }

func (c listingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	response, err := c.next.Do(req)
	if err != nil {
		return response, err
	}
	if response.ContentLength > maxS3ListingPageBytes {
		_ = response.Body.Close()
		return nil, fmt.Errorf("%w: S3 listing page exceeds %d bytes", plugins.ErrBackupListingLimit, maxS3ListingPageBytes)
	}
	response.Body = &listingPageBody{ReadCloser: response.Body, remaining: maxS3ListingPageBytes}
	return response, nil
}

type listingPageBody struct {
	io.ReadCloser
	remaining int64
}

func (b *listingPageBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.ReadCloser.Read(probe[:])
		if n > 0 {
			_ = b.Close()
			return 0, fmt.Errorf("%w: S3 listing page exceeds %d bytes", plugins.ErrBackupListingLimit, maxS3ListingPageBytes)
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// Keep ordinary SDK retries, but never refetch a response rejected by a budget.
type listingRetryer struct{ aws.Retryer }

func (r listingRetryer) IsErrorRetryable(err error) bool {
	return !errors.Is(err, plugins.ErrBackupListingLimit) && r.Retryer.IsErrorRetryable(err)
}
