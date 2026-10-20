package auth

import (
	"context"
	"errors"

	"github.com/zalando/go-keyring"
)

const KeyringService = "mailvault"

type KeyringStore struct{ Service string }

func (s KeyringStore) Get(_ context.Context, account string) (string, error) {
	service := s.Service
	if service == "" {
		service = KeyringService
	}
	value, err := keyring.Get(service, account)
	if err != nil {
		return "", err
	}
	return value, nil
}

func (s KeyringStore) Set(_ context.Context, account, value string) error {
	if account == "" || value == "" {
		return errors.New("keyring account and value are required")
	}
	service := s.Service
	if service == "" {
		service = KeyringService
	}
	return keyring.Set(service, account, value)
}
