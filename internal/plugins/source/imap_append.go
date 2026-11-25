package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/zekihan/mailvault/internal/plugins"
)

func (src *IMAPSource) EnsureMailbox(ctx context.Context, mailbox string, allowExisting bool) (bool, error) {
	if strings.TrimSpace(mailbox) == "" {
		return false, errors.New("mailbox is required")
	}
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.connectLocked(ctx); err != nil {
		return false, fmt.Errorf("connect to destination: %w: %w", err, plugins.ErrMailboxUnchanged)
	}

	finish := src.wire.beginIO(ctx)
	exists := false
	err := src.listMailboxesLocked(func(item *imap.ListData) {
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

	finish = src.wire.beginIO(ctx)
	err = src.client.Create(mailbox, nil).Wait()
	finish()
	if err != nil {
		var response *imap.Error
		if errors.As(err, &response) && response.Type == imap.StatusResponseTypeNo && response.Code == imap.ResponseCodeAlreadyExists {
			return false, fmt.Errorf("create destination mailbox %q: %w: %w", mailbox, plugins.ErrMailboxExists, err)
		}
		return false, fmt.Errorf("create destination mailbox %q: %w", mailbox, err)
	}
	return true, nil
}

// MailboxUIDValidity returns the generation of an existing mailbox.
func (src *IMAPSource) MailboxUIDValidity(ctx context.Context, mailbox string) (string, error) {
	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.connectLocked(ctx); err != nil {
		return "", err
	}
	return src.mailboxUIDValidityLocked(ctx, mailbox)
}

func (src *IMAPSource) mailboxUIDValidityLocked(ctx context.Context, mailbox string) (string, error) {
	finish := src.wire.beginIO(ctx)
	selected, err := src.client.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
	finish()
	if err != nil {
		return "", fmt.Errorf("select destination mailbox %q: %w", mailbox, err)
	}
	if selected.UIDValidity == 0 {
		return "", fmt.Errorf("destination mailbox %q returned zero UIDVALIDITY", mailbox)
	}
	return strconv.FormatUint(uint64(selected.UIDValidity), 10), nil
}

func (src *IMAPSource) AppendMessage(ctx context.Context, mailbox string, msg *plugins.Message, expectedUIDValidity string) error {
	if strings.TrimSpace(mailbox) == "" {
		return errors.New("mailbox is required")
	}
	if msg == nil {
		return errors.New("message is required")
	}
	if len(msg.Raw) == 0 {
		return errors.New("message is empty")
	}
	if err := plugins.CheckMessageSize(int64(len(msg.Raw)), src.maxMessageBytes); err != nil {
		return err
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

	src.mu.Lock()
	defer src.mu.Unlock()
	if err := src.connectLocked(ctx); err != nil {
		return err
	}
	if err := src.checkAppendValidity(ctx, mailbox, expectedUIDValidity); err != nil {
		return err
	}
	if err := src.appendRawMessage(ctx, mailbox, msg, flags, internalDate, expectedUIDValidity); err != nil {
		return err
	}
	return src.checkAppendValidityAfter(ctx, mailbox, expectedUIDValidity)
}

func (src *IMAPSource) checkAppendValidity(ctx context.Context, mailbox, expectedUIDValidity string) error {
	validity, err := src.mailboxUIDValidityLocked(ctx, mailbox)
	if err != nil {
		return fmt.Errorf("verify destination mailbox generation before APPEND: %w", err)
	}
	if expectedUIDValidity == "" || expectedUIDValidity == "0" || validity != expectedUIDValidity {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed from %s to %s before APPEND", expectedUIDValidity, validity)
	}
	return nil
}

func (src *IMAPSource) appendRawMessage(ctx context.Context, mailbox string, msg *plugins.Message, flags []imap.Flag, internalDate time.Time, expectedUIDValidity string) error {
	finish := src.wire.beginIO(ctx)
	appendCmd := src.client.Append(mailbox, int64(len(msg.Raw)), &imap.AppendOptions{Flags: flags, Time: internalDate})
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
	if appendData != nil && appendData.UIDValidity != 0 && expectedUIDValidity != strconv.FormatUint(uint64(appendData.UIDValidity), 10) {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed during APPEND from %s to %d", expectedUIDValidity, appendData.UIDValidity)
	}
	return nil
}

func (src *IMAPSource) checkAppendValidityAfter(ctx context.Context, mailbox, expectedUIDValidity string) error {
	validity, err := src.mailboxUIDValidityLocked(ctx, mailbox)
	if err != nil {
		return fmt.Errorf("verify destination mailbox generation after APPEND: %w", err)
	}
	if validity != expectedUIDValidity {
		return fmt.Errorf("destination mailbox UIDVALIDITY changed during APPEND from %s to %s", expectedUIDValidity, validity)
	}
	return nil
}

func appendFlags(values []string) ([]imap.Flag, error) {
	values, err := plugins.NormalizeAppendFlags(values)
	if err != nil {
		return nil, err
	}
	flags := make([]imap.Flag, len(values))
	for i, value := range values {
		flags[i] = imap.Flag(value)
	}
	return flags, nil
}
