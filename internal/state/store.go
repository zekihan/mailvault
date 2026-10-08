package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Store manages the SQLite database for sync state.
type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

// NewStore creates a new state store at the given path.
func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_fk=1&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	s := &Store{db: db, path: path}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	return s, nil
}

// migrate creates the schema if needed.
func (s *Store) migrate(ctx context.Context) error {
	schema := `
	CREATE TABLE IF NOT EXISTS sources (
		name TEXT PRIMARY KEY,
		type TEXT NOT NULL,
		host TEXT NOT NULL,
		username TEXT NOT NULL,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS folders (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_name TEXT NOT NULL REFERENCES sources(name) ON DELETE CASCADE,
		name TEXT NOT NULL,
		uidvalidity TEXT,
		uidnext TEXT,
		modseq TEXT,
		last_sync TIMESTAMP,
		UNIQUE(source_name, name)
	);

	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_name TEXT NOT NULL REFERENCES sources(name) ON DELETE CASCADE,
		folder TEXT NOT NULL,
		uid TEXT NOT NULL,
		message_id TEXT,
		content_hash TEXT,
		size INTEGER,
		internal_date TEXT,
		fetched_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(source_name, folder, uid)
	);

	CREATE TABLE IF NOT EXISTS oauth_tokens (
		source_name TEXT PRIMARY KEY REFERENCES sources(name) ON DELETE CASCADE,
		access_token TEXT,
		refresh_token TEXT,
		expiry TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_messages_source_folder ON messages(source_name, folder);
	CREATE INDEX IF NOT EXISTS idx_messages_message_id ON messages(message_id);
	CREATE INDEX IF NOT EXISTS idx_messages_content_hash ON messages(content_hash);
	CREATE INDEX IF NOT EXISTS idx_folders_source ON folders(source_name);
	`

	_, err := s.db.ExecContext(ctx, schema)
	return err
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// UpsertSource creates or updates a source record.
func (s *Store) UpsertSource(ctx context.Context, name, srcType, host, username string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sources (name, type, host, username, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(name) DO UPDATE SET
			type = excluded.type,
			host = excluded.host,
			username = excluded.username,
			updated_at = CURRENT_TIMESTAMP
	`, name, srcType, host, username)
	return err
}

// GetFolderState retrieves sync state for a folder.
func (s *Store) GetFolderState(ctx context.Context, sourceName, folder string) (*FolderState, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT uidvalidity, uidnext, modseq, last_sync
		FROM folders
		WHERE source_name = ? AND name = ?
	`, sourceName, folder)

	var fs FolderState
	err := row.Scan(&fs.UIDValidity, &fs.UIDNext, &fs.ModSeq, &fs.LastSync)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // Not found is not an error
	}
	if err != nil {
		return nil, err
	}
	return &fs, nil
}

// FolderState holds sync state for a single folder.
type FolderState struct {
	UIDValidity string
	UIDNext     string
	ModSeq      string
	LastSync    *time.Time
}

// UpsertFolderState creates or updates folder sync state.
func (s *Store) UpsertFolderState(ctx context.Context, sourceName, folder string, fs *FolderState) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO folders (source_name, name, uidvalidity, uidnext, modseq, last_sync)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(source_name, name) DO UPDATE SET
			uidvalidity = excluded.uidvalidity,
			uidnext = excluded.uidnext,
			modseq = excluded.modseq,
			last_sync = CURRENT_TIMESTAMP
	`, sourceName, folder, fs.UIDValidity, fs.UIDNext, fs.ModSeq)
	return err
}

// MarkMessageFetched records that a message has been fetched.
func (s *Store) MarkMessageFetched(ctx context.Context, sourceName, folder, uid, messageID, contentHash string, size int64, internalDate string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO messages (source_name, folder, uid, message_id, content_hash, size, internal_date, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(source_name, folder, uid) DO UPDATE SET
			message_id = excluded.message_id,
			content_hash = excluded.content_hash,
			size = excluded.size,
			internal_date = excluded.internal_date,
			fetched_at = CURRENT_TIMESTAMP
	`, sourceName, folder, uid, messageID, contentHash, size, internalDate)
	return err
}

// MessageExists checks if a message was already fetched (by UID).
func (s *Store) MessageExists(ctx context.Context, sourceName, folder, uid string) (bool, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM messages WHERE source_name = ? AND folder = ? AND uid = ?
	`, sourceName, folder, uid).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// FindByMessageID finds messages with the same Message-ID across all sources.
func (s *Store) FindByMessageID(ctx context.Context, messageID string) ([]MessageRecord, error) {
	if messageID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_name, folder, uid, message_id, content_hash, size, internal_date, fetched_at
		FROM messages WHERE message_id = ?
	`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMessages(rows)
}

// FindByContentHash finds messages with the same content hash.
func (s *Store) FindByContentHash(ctx context.Context, contentHash string) ([]MessageRecord, error) {
	if contentHash == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT source_name, folder, uid, message_id, content_hash, size, internal_date, fetched_at
		FROM messages WHERE content_hash = ?
	`, contentHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanMessages(rows)
}

// MessageRecord represents a stored message record.
type MessageRecord struct {
	SourceName   string
	Folder       string
	UID          string
	MessageID    string
	ContentHash  string
	Size         int64
	InternalDate string
	FetchedAt    time.Time
}

func scanMessages(rows *sql.Rows) ([]MessageRecord, error) {
	var results []MessageRecord
	for rows.Next() {
		var r MessageRecord
		if err := rows.Scan(&r.SourceName, &r.Folder, &r.UID, &r.MessageID, &r.ContentHash, &r.Size, &r.InternalDate, &r.FetchedAt); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// GetOAuthToken retrieves stored OAuth token for a source.
func (s *Store) GetOAuthToken(ctx context.Context, sourceName string) (*OAuthToken, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT access_token, refresh_token, expiry FROM oauth_tokens WHERE source_name = ?
	`, sourceName)

	var tok OAuthToken
	err := row.Scan(&tok.AccessToken, &tok.RefreshToken, &tok.Expiry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &tok, nil
}

// OAuthToken holds OAuth2 tokens.
type OAuthToken struct {
	AccessToken  string
	RefreshToken string
	Expiry       time.Time
}

// UpsertOAuthToken stores or updates OAuth tokens.
func (s *Store) UpsertOAuthToken(ctx context.Context, sourceName string, tok *OAuthToken) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO oauth_tokens (source_name, access_token, refresh_token, expiry, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(source_name) DO UPDATE SET
			access_token = excluded.access_token,
			refresh_token = excluded.refresh_token,
			expiry = excluded.expiry,
			updated_at = CURRENT_TIMESTAMP
	`, sourceName, tok.AccessToken, tok.RefreshToken, tok.Expiry)
	return err
}

// CleanOldMessages removes message records older than the given duration.
func (s *Store) CleanOldMessages(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM messages WHERE fetched_at < ?
	`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Stats returns database statistics.
func (s *Store) Stats(ctx context.Context) (map[string]int64, error) {
	stats := make(map[string]int64)

	queries := map[string]string{
		"sources":   "SELECT COUNT(*) FROM sources",
		"folders":   "SELECT COUNT(*) FROM folders",
		"messages":  "SELECT COUNT(*) FROM messages",
		"oauth":     "SELECT COUNT(*) FROM oauth_tokens",
	}

	for name, query := range queries {
		var count int64
		if err := s.db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return nil, err
		}
		stats[name] = count
	}

	return stats, nil
}