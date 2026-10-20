package plugins

import (
	"context"
	"errors"
	"io"
)

var (
	ErrBackupMessageMissing = errors.New("backup message is missing")
	ErrBackupMessageCorrupt = errors.New("backup message is corrupt")
)

// Message represents a single email message.
type Message struct {
	// UIDScope identifies the account and, for IMAP, its UIDVALIDITY generation.
	UIDScope string

	// UID is the server-side unique identifier (IMAP UID or POP3 UIDL).
	UID string

	// Folder is the source folder/mailbox name.
	Folder string

	// Flags are the source-provided message flags (for example, IMAP system flags).
	Flags []string

	// Raw is the full RFC822 message bytes.
	Raw []byte

	// MessageID is the Message-ID header (for deduplication).
	MessageID string

	// ContentHash is SHA-256 of Raw (for deduplication fallback).
	ContentHash string

	// Size is the message size in bytes.
	Size int64

	// InternalDate is the message's internal date from the server.
	InternalDate string
}

// Source defines the interface for mail sources (IMAP, POP3, etc.).
type Source interface {
	// Name returns the source's configured name.
	Name() string

	// Type returns the source type (e.g., "imap", "pop3").
	Type() string

	// Connect establishes a connection to the source.
	Connect(ctx context.Context) error

	// ListFolders returns available folders/mailboxes.
	ListFolders(ctx context.Context) ([]string, error)

	// FetchMessages retrieves messages from the given folders.
	// If since is non-zero, only messages at or after that time are fetched.
	// If until is non-zero, only messages before that time are fetched.
	FetchMessages(ctx context.Context, folders []string, since, until int64) (<-chan *Message, <-chan error)

	// Close closes the connection.
	Close() error
}

// Target defines the interface for mail targets (Maildir, mbox, S3, etc.).
type Target interface {
	// Name returns the target's configured name.
	Name() string

	// Type returns the target type (e.g., "maildir", "mbox", "s3").
	Type() string

	// Initialize prepares the target for writing (creates directories, etc.).
	Initialize(ctx context.Context) error

	// WriteMessage stores a single message.
	WriteMessage(ctx context.Context, msg *Message) error

	// Close finalizes and closes the target.
	Close() error
}

// BackupMessage identifies a stored message and the source delivery that owns it.
// Locator is opaque to callers and interpreted by the target that produced it.
type BackupMessage struct {
	Source       string   `json:"source"`
	Target       string   `json:"target"`
	Scope        string   `json:"scope"`
	Folder       string   `json:"folder"`
	UID          string   `json:"uid"`
	Locator      string   `json:"locator"`
	RawSHA256    string   `json:"raw_sha256"`
	Flags        []string `json:"flags,omitempty"`
	InternalDate string   `json:"internal_date,omitempty"`
}

// TargetBackupReader exposes stable locations for verification and restore.
type TargetBackupReader interface {
	ListBackupMessages(context.Context) ([]BackupMessage, error)
	ReadBackupMessage(context.Context, string) (*Message, error)
}

// TargetBackupReaderInitializer prepares only read capabilities, without creating destination state.
type TargetBackupReaderInitializer interface{ InitializeForRead(context.Context) error }

// BackupWriter returns the stable target locator after writing a message.
type BackupWriter interface {
	WriteBackupMessage(context.Context, *Message) (string, error)
}

// RepairBackupWriter stores a corrected copy while retaining the damaged copy.
type RepairBackupWriter interface {
	WriteRepairMessage(context.Context, *Message) (string, error)
}

// BackupLocatorProvider exposes the location from the most recent successful WriteMessage call.
type BackupLocatorProvider interface{ LastBackupLocator() string }

// SourceFactory creates Source instances from configuration.
type SourceFactory func(config map[string]interface{}) (Source, error)

// TargetFactory creates Target instances from configuration.
type TargetFactory func(config map[string]interface{}) (Target, error)

// Registry holds registered source and target factories.
type Registry struct {
	sources map[string]SourceFactory
	targets map[string]TargetFactory
}

// NewRegistry creates a new plugin registry.
func NewRegistry() *Registry {
	return &Registry{
		sources: make(map[string]SourceFactory),
		targets: make(map[string]TargetFactory),
	}
}

// RegisterSource registers a source factory.
func (r *Registry) RegisterSource(name string, factory SourceFactory) {
	r.sources[name] = factory
}

// RegisterTarget registers a target factory.
func (r *Registry) RegisterTarget(name string, factory TargetFactory) {
	r.targets[name] = factory
}

// CreateSource creates a source from configuration.
func (r *Registry) CreateSource(name string, config map[string]interface{}) (Source, error) {
	factory, ok := r.sources[name]
	if !ok {
		return nil, ErrUnknownSource(name)
	}
	src, err := factory(config)
	if err != nil {
		return nil, err
	}
	applyConfiguredName(src, config)
	return src, nil
}

// CreateTarget creates a target from configuration.
func (r *Registry) CreateTarget(name string, config map[string]interface{}) (Target, error) {
	factory, ok := r.targets[name]
	if !ok {
		return nil, ErrUnknownTarget(name)
	}
	tgt, err := factory(config)
	if err != nil {
		return nil, err
	}
	applyConfiguredName(tgt, config)
	return tgt, nil
}

func applyConfiguredName(plugin interface{}, config map[string]interface{}) {
	name, _ := config["name"].(string)
	if named, ok := plugin.(interface{ SetName(string) }); ok && name != "" {
		named.SetName(name)
	}
}

// SourceTypes returns registered source type names.
func (r *Registry) SourceTypes() []string {
	types := make([]string, 0, len(r.sources))
	for k := range r.sources {
		types = append(types, k)
	}
	return types
}

// TargetTypes returns registered target type names.
func (r *Registry) TargetTypes() []string {
	types := make([]string, 0, len(r.targets))
	for k := range r.targets {
		types = append(types, k)
	}
	return types
}

// ErrUnknownSource indicates an unknown source type.
type ErrUnknownSource string

func (e ErrUnknownSource) Error() string {
	return "unknown source type: " + string(e)
}

// ErrUnknownTarget indicates an unknown target type.
type ErrUnknownTarget string

func (e ErrUnknownTarget) Error() string {
	return "unknown target type: " + string(e)
}

// MessageWriter is a helper interface for targets that support streaming writes.
type MessageWriter interface {
	WriteMessage(ctx context.Context, msg *Message) error
	Close() error
}

// MessageReader is a helper interface for sources that support streaming reads.
type MessageReader interface {
	ReadMessage(ctx context.Context) (*Message, error)
	Close() error
}

// CopyMessages copies messages from a reader to a writer with context support.
func CopyMessages(ctx context.Context, reader MessageReader, writer MessageWriter) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		msg, err := reader.ReadMessage(ctx)
		if err == io.EOF {
			return writer.Close()
		}
		if err != nil {
			return err
		}
		if err := writer.WriteMessage(ctx, msg); err != nil {
			return err
		}
	}
}
