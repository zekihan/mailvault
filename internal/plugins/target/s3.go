package target

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv2config "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

// S3Target implements the Target interface for S3-compatible storage.
type S3Target struct {
	name           string
	bucket         string
	region         string
	endpoint       string
	prefix         string
	credentialsRef string
	client         *s3.Client
	uploader       *transfermanager.Client
	mu             sync.Mutex
}

func NewS3Target(configMap map[string]interface{}) (plugins.Target, error) {
	bucket, _ := config.ParseString(configMap, config.TargetConfigBucket, "")
	region, _ := config.ParseString(configMap, config.TargetConfigRegion, "")
	endpoint, _ := config.ParseString(configMap, config.TargetConfigEndpoint, "")
	prefix, _ := config.ParseString(configMap, config.TargetConfigPrefix, "")
	credentialsRef, _ := config.ParseString(configMap, config.TargetConfigCredentialsRef, "")

	if bucket == "" {
		return nil, errors.New("bucket is required")
	}
	if region == "" {
		return nil, errors.New("region is required")
	}
	if credentialsRef == "" {
		return nil, errors.New("credentials_ref is required")
	}

	// Ensure prefix ends with /
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	return &S3Target{
		name:           "s3:" + bucket, // Will be overridden by registry
		bucket:         bucket,
		region:         region,
		endpoint:       endpoint,
		prefix:         prefix,
		credentialsRef: credentialsRef,
	}, nil
}

// SetName sets the target name (called by registry).
func (t *S3Target) SetName(name string) {
	t.name = name
}

func (t *S3Target) Name() string {
	return t.name
}

func (t *S3Target) Type() string {
	return "s3"
}

func (t *S3Target) Initialize(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Load AWS config
	cfgOpts := []func(*awsv2config.LoadOptions) error{
		awsv2config.WithRegion(t.region),
	}

	cfg, err := awsv2config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}

	// Create S3 client
	t.client = s3.NewFromConfig(cfg, func(opts *s3.Options) {
		if t.endpoint != "" {
			opts.BaseEndpoint = aws.String(t.endpoint)
		}
	})
	t.uploader = transfermanager.New(t.client)

	// Verify bucket exists
	_, err = t.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(t.bucket),
	})
	if err != nil {
		return fmt.Errorf("bucket %q not accessible: %w", t.bucket, err)
	}

	return nil
}

func (t *S3Target) WriteMessage(ctx context.Context, msg *plugins.Message) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.uploader == nil {
		return errors.New("S3 target not initialized")
	}

	// Generate a stable key from the raw content, folder, and UID.
	hash := sha256.Sum256(msg.Raw)
	key := fmt.Sprintf("%s%s-%s-%s.eml", t.prefix, hex.EncodeToString(hash[:]), hex.EncodeToString([]byte(msg.Folder)), url.PathEscape(msg.UID))

	// Upload message
	_, err := t.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:      aws.String(t.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(string(msg.Raw)),
		ContentType: aws.String("message/rfc822"),
		Metadata: map[string]string{
			"source-folder": msg.Folder,
			"source-uid":    msg.UID,
			"message-id":    msg.MessageID,
			"content-hash":  hex.EncodeToString(hash[:]),
			"internal-date": msg.InternalDate,
			"size":          fmt.Sprintf("%d", msg.Size),
		},
	})
	if err != nil {
		return fmt.Errorf("upload to S3: %w", err)
	}

	return nil
}

func (t *S3Target) Close() error {
	// Nothing to close for S3 client
	return nil
}
