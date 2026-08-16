//go:build !android

package main

type unavailableCredentialStore struct{}

func newPlatformCredentialStore() remoteCredentialStore {
	return unavailableCredentialStore{}
}

func (unavailableCredentialStore) Load(string) (string, error) { return "", nil }
func (unavailableCredentialStore) Save(string, string) error   { return nil }
func (unavailableCredentialStore) Delete(string) error         { return nil }
