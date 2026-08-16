//go:build android

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	wails "github.com/wailsapp/wails/v3/pkg/application"
)

const remoteCredentialKeyPrefix = "pi.remote.token.v1."

type androidCredentialStore struct{}

func newPlatformCredentialStore() remoteCredentialStore {
	return androidCredentialStore{}
}

func (androidCredentialStore) Load(endpoint string) (string, error) {
	return wails.Android.SecureGet(remoteCredentialKey(endpoint)), nil
}

func (androidCredentialStore) Save(endpoint, token string) error {
	payload, err := json.Marshal(struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}{Key: remoteCredentialKey(endpoint), Value: token})
	if err != nil {
		return err
	}
	wails.Android.SecureSet(string(payload))
	return nil
}

func (androidCredentialStore) Delete(endpoint string) error {
	wails.Android.SecureDelete(remoteCredentialKey(endpoint))
	return nil
}

func remoteCredentialKey(endpoint string) string {
	digest := sha256.Sum256([]byte(endpoint))
	return remoteCredentialKeyPrefix + hex.EncodeToString(digest[:])
}
