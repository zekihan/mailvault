package auth

import (
	"errors"
	"strings"

	"github.com/emersion/go-sasl"
)

// NewXOAUTH2 creates a SASL XOAUTH2 client for IMAP or POP.
func NewXOAUTH2(username, accessToken string) sasl.Client { //nolint:ireturn // SASL client is the plugin interface; concrete type is internal
	return &xoauth2Client{username: username, accessToken: accessToken}
}

type xoauth2Client struct {
	username    string
	accessToken string
}

func (client *xoauth2Client) Start() (string, []byte, error) {
	if client.username == "" || client.accessToken == "" {
		return "", nil, errors.New("XOAUTH2 username and access token are required")
	}
	if strings.ContainsAny(client.username, "\x00\x01\r\n") || strings.ContainsAny(client.accessToken, "\x00\x01\r\n") {
		return "", nil, errors.New("XOAUTH2 username or access token contains invalid protocol characters")
	}
	return "XOAUTH2", []byte("user=" + client.username + "\x01auth=Bearer " + client.accessToken + "\x01\x01"), nil
}

func (client *xoauth2Client) Next(challenge []byte) ([]byte, error) {
	return []byte{}, nil
}
