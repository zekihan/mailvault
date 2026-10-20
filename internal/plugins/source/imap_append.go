package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func (s *IMAPSource) EnsureMailbox(ctx context.Context, mailbox string, allowExisting bool) (bool, error) {
	if strings.TrimSpace(mailbox) == "" {
		return false, errors.New("mailbox is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.connectLocked(ctx); err != nil {
		return false, fmt.Errorf("connect to destination: %w: %w", err, plugins.ErrMailboxUnchanged)
	}

	finish := s.wire.beginIO(ctx)
	mailboxes, err := s.client.List("", "*", &imap.ListOptions{}).Collect()
	finish()
	if err != nil {
		return false, fmt.Errorf("list destination mailboxes: %w: %w", err, plugins.ErrMailboxUnchanged)
	}
	for _, item := range mailboxes {
		if item.Mailbox == mailbox {
			if allowExisting {
				return false, nil
			}
			return false, fmt.Errorf("destination mailbox %q already exists; choose a new folder or resume its existing restore job: %w", mailbox, plugins.ErrMailboxExists)
		}
	}
	if allowExisting {
		return false, fmt.Errorf("resumable destination mailbox %q is missing: %w", mailbox, plugins.ErrMailboxMissing)
	}

	finish = s.wire.beginIO(ctx)
	err = s.client.Create(mailbox, nil).Wait()
	finish()
	if err != nil {
		return false, fmt.Errorf("create destination mailbox %q: %w", mailbox, err)
	}
	return true, nil
}

func (s *IMAPSource) AppendMessage(ctx context.Context, mailbox string, msg *plugins.Message) error {
	if strings.TrimSpace(mailbox) == "" {
		return errors.New("mailbox is required")
	}
	if msg == nil {
		return errors.New("message is required")
	}
	if len(msg.Raw) == 0 {
		return errors.New("message is empty")
	}
	var internalDate time.Time
	if msg.InternalDate != "" {
		var err error
		internalDate, err = time.Parse(time.RFC3339, msg.InternalDate)
		if err != nil {
			return fmt.Errorf("parse internal date: %w", err)
		}
	}
	flags, err := appendFlags(msg.Flags)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.connectLocked(ctx); err != nil {
		return err
	}
	finish := s.wire.beginIO(ctx)
	appendCmd := s.client.Append(mailbox, int64(len(msg.Raw)), &imap.AppendOptions{Flags: flags, Time: internalDate})
	_, writeErr := io.Copy(appendCmd, bytes.NewReader(msg.Raw))
	if writeErr == nil {
		writeErr = appendCmd.Close()
	} else {
		_ = appendCmd.Close()
	}
	if writeErr == nil {
		_, writeErr = appendCmd.Wait()
	}
	finish()
	if writeErr != nil {
		return fmt.Errorf("append message to %q: %w", mailbox, writeErr)
	}
	return nil
}

func appendFlags(values []string) ([]imap.Flag, error) {
	flags := make([]imap.Flag, 0, len(values))
	for _, value := range values {
		switch strings.ToLower(value) {
		case "\\recent":
			continue
		case "\\seen":
			flags = append(flags, imap.FlagSeen)
			continue
		case "\\answered":
			flags = append(flags, imap.FlagAnswered)
			continue
		case "\\flagged":
			flags = append(flags, imap.FlagFlagged)
			continue
		case "\\deleted":
			flags = append(flags, imap.FlagDeleted)
			continue
		case "\\draft":
			flags = append(flags, imap.FlagDraft)
			continue
		}
		if value == "" || strings.HasPrefix(value, "\\") {
			return nil, fmt.Errorf("unsupported IMAP system flag %q", value)
		}
		for _, char := range []byte(value) {
			if char < 0x21 || char > 0x7e || strings.ContainsRune("(){%*\\\"]", rune(char)) {
				return nil, fmt.Errorf("invalid IMAP keyword flag %q", value)
			}
		}
		flags = append(flags, imap.Flag(value))
	}
	return flags, nil
}
