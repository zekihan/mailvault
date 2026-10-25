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
	exists := false
	err := s.listMailboxesLocked(func(item *imap.ListData) {
		if item.Mailbox == mailbox {
			exists = true
		}
	})
	finish()
	if err != nil {
		return false, fmt.Errorf("list destination mailboxes: %w: %w", err, plugins.ErrMailboxUnchanged)
	}
	if exists {
		if allowExisting {
			return false, nil
		}
		return false, fmt.Errorf("destination mailbox %q already exists; choose a new folder or resume its existing restore job: %w", mailbox, plugins.ErrMailboxExists)
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

// MailboxUIDValidity returns the generation of an existing mailbox.
func (s *IMAPSource) MailboxUIDValidity(ctx context.Context, mailbox string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.connectLocked(ctx); err != nil {
		return "", err
	}
	return s.mailboxUIDValidityLocked(ctx, mailbox)
}

func (s *IMAPSource) mailboxUIDValidityLocked(ctx context.Context, mailbox string) (string, error) {
	finish := s.wire.beginIO(ctx)
	selected, err := s.client.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	finish()
	if err != nil {
		return "", fmt.Errorf("select destination mailbox %q: %w", mailbox, err)
	}
	if selected.UIDValidity == 0 {
		return "", fmt.Errorf("destination mailbox %q returned zero UIDVALIDITY", mailbox)
	}
	return fmt.Sprint(selected.UIDValidity), nil
}

func (s *IMAPSource) AppendMessage(ctx context.Context, mailbox string, msg *plugins.Message, expectedUIDValidity string) error {
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
	validity, err := s.mailboxUIDValidityLocked(ctx, mailbox)
	if err != nil {
		return fmt.Errorf("verify destination mailbox generation before APPEND: %w", err)
	}
	if expectedUIDValidity == "" || expectedUIDValidity == "0" || validity != expectedUIDValidity {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed from %s to %s before APPEND", expectedUIDValidity, validity)
	}
	finish := s.wire.beginIO(ctx)
	appendCmd := s.client.Append(mailbox, int64(len(msg.Raw)), &imap.AppendOptions{Flags: flags, Time: internalDate})
	_, writeErr := io.Copy(appendCmd, bytes.NewReader(msg.Raw))
	if writeErr == nil {
		writeErr = appendCmd.Close()
	} else {
		_ = appendCmd.Close()
	}
	var appendData *imap.AppendData
	if writeErr == nil {
		appendData, writeErr = appendCmd.Wait()
	}
	finish()
	if writeErr != nil {
		return fmt.Errorf("append message to %q: %w", mailbox, writeErr)
	}
	if appendData != nil && appendData.UIDValidity != 0 && expectedUIDValidity != fmt.Sprint(appendData.UIDValidity) {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed during APPEND from %s to %d", expectedUIDValidity, appendData.UIDValidity)
	}
	validity, err = s.mailboxUIDValidityLocked(ctx, mailbox)
	if err != nil {
		return fmt.Errorf("verify destination mailbox generation after APPEND: %w", err)
	}
	if validity != expectedUIDValidity {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed during APPEND from %s to %s", expectedUIDValidity, validity)
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
