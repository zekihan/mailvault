package auth

import (
	"context"
	"errors"
	"strings"

	"github.com/emersion/go-sasl"
)

// TokenProvider obtains a currently valid access token.
type TokenProvider interface {
	AccessToken(context.Context) (string, error)
}

// NewXOAUTH2 creates a SASL XOAUTH2 client for IMAP or POP.
func NewXOAUTH2(username, accessToken string) sasl.Client {
	return &xoauth2Client{username: username, accessToken: accessToken}
}

type xoauth2Client struct {
	username    string
	accessToken string
}

func (c *xoauth2Client) Start() (string, []byte, error) {
	if c.username == "" || c.accessToken == "" {
		return "", nil, errors.New("XOAUTH2 username and access token are required")
	}
	if strings.ContainsAny(c.username, "\x00\x01\r\n") || strings.ContainsAny(c.accessToken, "\x00\x01\r\n") {
		return "", nil, errors.New("XOAUTH2 username or access token contains invalid protocol characters")
	}
	return "XOAUTH2", []byte("user=" + c.username + "\x01auth=Bearer " + c.accessToken + "\x01\x01"), nil
}

func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	return []byte{}, nil
}
