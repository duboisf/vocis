package securestore

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	serviceName = "vocis"
	accountName = "openai_api_key"
)

type Store struct{}

func New() *Store {
	return &Store{}
}

func (s *Store) APIKey() (string, error) {
	if key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY")); key != "" {
		return key, nil
	}

	key, err := keyring.Get(serviceName, accountName)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", errors.New("OpenAI API key not found; run `vocis key set` or export OPENAI_API_KEY")
		}
		return "", err
	}
	if strings.TrimSpace(key) == "" {
		return "", errors.New("stored OpenAI API key is empty")
	}
	return key, nil
}

func (s *Store) SetAPIKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("OpenAI API key must not be empty")
	}
	return keyring.Set(serviceName, accountName, key)
}

func (s *Store) ClearAPIKey() error {
	if err := keyring.Delete(serviceName, accountName); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

func (s *Store) Source() (string, error) {
	if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) != "" {
		return "environment", nil
	}
	if _, err := keyring.Get(serviceName, accountName); err == nil {
		return "system-keyring", nil
	}
	return "", fmt.Errorf("no OpenAI API key configured")
}
