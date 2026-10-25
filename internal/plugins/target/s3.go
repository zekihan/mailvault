package target

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv2config "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/zekihan/mailvault/internal/plugins"
	"github.com/zekihan/mailvault/internal/plugins/config"
)

// S3Target implements the Target interface for S3-compatible storage.
type S3Target struct {
	listingLimits    backupListingLimits
	maxMessageBytes  int64
	name             string
	bucket           string
	region           string
	endpoint         string
	prefix           string
	credentials      aws.CredentialsProvider
	operationTimeout time.Duration
	client           *s3.Client
	uploader         *transfermanager.Client
	lastLocator      string
	mu               sync.Mutex
}

const maxS3ObjectKeyBytes = 1024
const boundedS3ObjectNameBytes = sha256.Size*4 + len("-.eml")
const defaultS3OperationTimeout = 5 * time.Minute

func NewS3Target(configMap map[string]interface{}) (plugins.Target, error) {
	limits, err := parseBackupListingLimits(configMap)
	if err != nil {
		return nil, err
	}
	maxMessageBytes, limitErr := config.ParseMessageLimit(configMap)
	if limitErr != nil {
		return nil, limitErr
	}
	operationTimeout, err := config.ParseDuration(configMap, config.TargetConfigOperationTimeout, defaultS3OperationTimeout)
	if err != nil || operationTimeout <= 0 {
		return nil, errors.New("invalid operation_timeout: expected a positive duration")
	}
	parseOption := func(key string) (string, error) {
		value, err := config.ParseString(configMap, key, "")
		if err != nil {
			return "", fmt.Errorf("invalid %s: expected string", key)
		}
		return value, nil
	}
	bucket, err := parseOption(config.TargetConfigBucket)
	if err != nil {
		return nil, err
	}
	region, err := parseOption(config.TargetConfigRegion)
	if err != nil {
		return nil, err
	}
	endpoint, err := parseOption(config.TargetConfigEndpoint)
	if err != nil {
		return nil, err
	}
	prefix, err := parseOption(config.TargetConfigPrefix)
	if err != nil {
		return nil, err
	}
	credentialsRef, err := config.ParseString(configMap, config.TargetConfigCredentialsRef, "")
	if err != nil {
		return nil, errS3Credentials
	}
	if _, explicit := configMap[config.TargetConfigCredentialsRef]; explicit && credentialsRef == "" {
		return nil, errS3Credentials
	}
	provider, err := s3CredentialProvider(credentialsRef)
	if err != nil {
		return nil, err
	}

	if bucket == "" {
		return nil, errors.New("bucket is required")
	}
	if region == "" {
		return nil, errors.New("region is required")
	}

	// Ensure prefix ends with /
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	if len(prefix) > maxS3ObjectKeyBytes-boundedS3ObjectNameBytes {
		return nil, errors.New("prefix leaves insufficient space for a bounded S3 object key")
	}

	return &S3Target{
		listingLimits:    limits,
		maxMessageBytes:  maxMessageBytes,
		name:             "s3:" + bucket, // Will be overridden by registry
		bucket:           bucket,
		region:           region,
		endpoint:         endpoint,
		prefix:           prefix,
		credentials:      provider,
		operationTimeout: operationTimeout,
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

func (t *S3Target) DestinationIdentity() string {
	return destinationFingerprint(t.Type(), t.bucket, t.region, canonicalEndpoint(t.endpoint), t.prefix)
}

func (t *S3Target) Initialize(ctx context.Context) error {
	if err := t.InitializeForRead(ctx); err != nil {
		return err
	}
	ctx, cancel := t.operationContext(ctx)
	defer cancel()
	t.mu.Lock()
	defer t.mu.Unlock()

	// Verify bucket exists
	_, err := t.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(t.bucket),
	})
	if err != nil {
		return fmt.Errorf("bucket %q not accessible: %w", t.bucket, err)
	}

	return nil
}

func (t *S3Target) InitializeForRead(ctx context.Context) error {
	ctx, cancel := t.operationContext(ctx)
	defer cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.client != nil {
		return nil
	}
	cfgOpts := []func(*awsv2config.LoadOptions) error{awsv2config.WithRegion(t.region)}
	if t.credentials != nil {
		cfgOpts = append(cfgOpts, awsv2config.WithCredentialsProvider(t.credentials))
	}
	cfg, err := awsv2config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	t.client = s3.NewFromConfig(cfg, func(opts *s3.Options) {
		if t.endpoint != "" {
			opts.BaseEndpoint = aws.String(t.endpoint)
		}
	})
	t.uploader = transfermanager.New(t.client)
	return nil
}

func (t *S3Target) WriteMessage(ctx context.Context, msg *plugins.Message) error {
	_, err := t.WriteBackupMessage(ctx, msg)
	return err
}

func (t *S3Target) LastBackupLocator() string { t.mu.Lock(); defer t.mu.Unlock(); return t.lastLocator }

func (t *S3Target) WriteBackupMessage(ctx context.Context, msg *plugins.Message) (string, error) {
	ctx, cancel := t.operationContext(ctx)
	defer cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastLocator = ""
	if err := plugins.CheckMessageSize(int64(len(msg.Raw)), t.maxMessageBytes); err != nil {
		return "", err
	}

	if t.uploader == nil {
		return "", errors.New("S3 target not initialized")
	}

	// Generate a stable key from the raw content, folder, and UID.
	hash := sha256.Sum256(msg.Raw)
	key := s3ObjectKey(t.prefix, hash, msg.Folder, msg.UID)
	if len(key) > maxS3ObjectKeyBytes {
		return "", errors.New("S3 object key exceeds 1024 bytes")
	}

	// Upload message
	_, err := t.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket:      aws.String(t.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(msg.Raw),
		ContentType: aws.String("message/rfc822"),
		Metadata:    s3ObjectMetadata(msg, hash),
	})
	if err != nil {
		// Multipart failure cleanup can flatten the SDK's cancellation cause.
		// Preserve our operation deadline/cancellation alongside that error.
		return "", fmt.Errorf("upload to S3: %w", errors.Join(err, ctx.Err()))
	}

	t.lastLocator = key
	return key, nil
}

func (t *S3Target) WriteRepairMessage(ctx context.Context, msg *plugins.Message) (string, error) {
	if err := plugins.CheckMessageSize(int64(len(msg.Raw)), t.maxMessageBytes); err != nil {
		return "", err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("create repair key: %w", err)
	}
	hash := sha256.Sum256(msg.Raw)
	key := fmt.Sprintf("%s%s-repair-%x.eml", t.prefix, hex.EncodeToString(hash[:]), nonce[:])
	if len(key) > maxS3ObjectKeyBytes {
		return "", errors.New("S3 repair object key exceeds 1024 bytes")
	}
	ctx, cancel := t.operationContext(ctx)
	defer cancel()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastLocator = ""
	if t.uploader == nil {
		return "", errors.New("S3 target not initialized")
	}
	_, err := t.uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: aws.String(t.bucket), Key: aws.String(key), Body: bytes.NewReader(msg.Raw),
		ContentType: aws.String("message/rfc822"), Metadata: s3ObjectMetadata(msg, hash),
	})
	if err != nil {
		return "", fmt.Errorf("upload repaired message to S3: %w", errors.Join(err, ctx.Err()))
	}
	t.lastLocator = key
	return key, nil
}

func (t *S3Target) Close() error {
	// Nothing to close for S3 client
	return nil
}

// Bound the entire SDK operation, including configuration, retries and body reads.
// WithTimeout preserves any earlier caller deadline.
func (t *S3Target) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := t.operationTimeout
	if timeout <= 0 {
		timeout = defaultS3OperationTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// Keep ordinary keys unchanged; bound unusual folder/UID components by hashing
// their combined identity. The content digest remains first for retry stability.
func s3ObjectKey(prefix string, contentHash [32]byte, folder, uid string) string {
	folderPart := hex.EncodeToString([]byte(folder))
	uidPart := url.PathEscape(uid)
	key := fmt.Sprintf("%s%s-%s-%s.eml", prefix, hex.EncodeToString(contentHash[:]), folderPart, uidPart)
	if len(key) <= maxS3ObjectKeyBytes {
		return key
	}
	identity := sha256.Sum256([]byte(folder + "\x00" + uid))
	return fmt.Sprintf("%s%s-%s.eml", prefix, hex.EncodeToString(contentHash[:]), hex.EncodeToString(identity[:]))
}
