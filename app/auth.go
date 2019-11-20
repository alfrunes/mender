// Copyright 2019 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package app

import (
	"os"
	"regexp"

	"github.com/mendersoftware/log"
	"github.com/mendersoftware/mender/client"
	"github.com/mendersoftware/mender/datastore"
	dev "github.com/mendersoftware/mender/device"
	"github.com/mendersoftware/mender/store"
	"github.com/pkg/errors"
)

type AuthManager interface {
	// returns true if authorization data is current and valid
	IsAuthorized() bool
	// returns device's authorization token
	AuthToken(serverIndex int) (client.AuthToken, error)
	// removes authentication token
	RemoveAuthToken() error
	// check if device key is available
	HasKey() bool
	// generate device key (will overwrite an already existing key)
	GenerateKey() error

	client.AuthDataMessenger
}

const (
	noAuthToken = client.EmptyAuthToken
)

type MenderAuthManager struct {
	store    store.Store
	keyStore *store.Keystore
	idSrc    dev.IdentityDataGetter
	servers  []conf.MenderServer
}

type AuthManagerConfig struct {
	AuthDataStore  store.Store            // authorization data store
	KeyStore       *store.Keystore        // key storage
	IdentitySource dev.IdentityDataGetter // provider of identity data
	Servers        []conf.MenderServer    // []{URL, TenantToken}
}

func NewAuthManager(conf AuthManagerConfig) AuthManager {

	if conf.KeyStore == nil || conf.IdentitySource == nil ||
		conf.AuthDataStore == nil {
		return nil
	}

	mgr := &MenderAuthManager{
		store:    conf.AuthDataStore,
		keyStore: conf.KeyStore,
		idSrc:    conf.IdentitySource,
		servers:  conf.MenderServers,
	}

	if err := mgr.keyStore.Load(); err != nil && !store.IsNoKeys(err) {
		log.Errorf("failed to load device keys: %v", err)
		// Otherwise ignore error returned from Load() call. It will
		// just result in an empty keyStore which in turn will cause
		// regeneration of keys.
	}

	if _, err := m.store.ReadAll(datastore.AuthToken); err != nil {
		// Delete key if it exists
		m.store.Remove(datastore.AuthToken)
	}

	return mgr
}

func (m *MenderAuthManager) IsAuthorized() bool {
	adata, err := m.AuthToken()
	if err != nil {
		return false
	}

	if adata == noAuthToken {
		return false
	}

	// TODO check if JWT is valid?

	return true
}

func (m *MenderAuthManager) MakeAuthRequest(
	serverIndex int) (*client.AuthRequest, error) {

	var err error
	authd := client.AuthReqData{}

	idata, err := m.idSrc.Get()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to obtain identity data")
	}

	authd.IdData = idata

	// fill device public key
	authd.Pubkey, err = m.keyStore.PublicPEM()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to obtain device public key")
	}

	tentok := strings.TrimSpace(string(m.servers[serverIndex].TenantToken))

	log.Debugf("tenant token: %s", tentok)

	// fill tenant token
	authd.TenantToken = string(tentok)

	log.Debugf("authorization data: %v", authd)

	reqdata, err := authd.ToBytes()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to convert auth request data")
	}

	// generate signature
	sig, err := m.keyStore.Sign(reqdata)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to sign auth request")
	}

	return &client.AuthRequest{
		Data:      reqdata,
		Token:     client.AuthToken(tentok),
		Signature: sig,
	}, nil
}

// Creates authtoken lmdb key from server url
func prepareAuthTokenKey(serverURL string) string {
	storeKey := fmt.Sprintf(datastore.AuthTokenNameF, serverURL)
	hostRegex, err := regexp.Compile(`(https?://)([^/]+)/?`)
	if err != nil {
		// Should never occur - return default token
		return datastore.AuthToken
	}
	storeKey = hostRegex.ReplaceAllString(storeKey, `\2`)
	return storeKey
}

func (m *MenderAuthManager) RecvAuthResponse(
	serverIndex int, data []byte) error {
	if len(data) == 0 {
		return errors.New("empty auth response data")
	}

	storeKey := prepareAuthTokenKey(m.servers[serverIndex].ServerURL)

	if err := m.store.WriteAll(storeKey, data); err != nil {
		return errors.Wrapf(err, "failed to save auth token")
	}
	return nil
}

func (m *MenderAuthManager) AuthToken(
	serverIndex int) (client.AuthToken, error) {
	// Prepare storage key: prefixed with server host name
	storeKey := prepareAuthTokenKey(m.servers[serverIndex].ServerURL)
	data, err := m.store.ReadAll(storeKey)
	if err != nil {
		if os.IsNotExist(err) {
			if err != nil {
				return noAuthToken, nil
			}
		}
		return noAuthToken, errors.Wrapf(
			err, "failed to read auth token data")
	}

	return client.AuthToken(data), nil
}

func (m *MenderAuthManager) RemoveAuthToken(serverIndex int) error {
	// remove token only if we have one
	storeKey := prepareAuthTokenKey(m.servers[serverIndex].ServerURL)
	if _, err := m.store.ReadAll(storeKey); err != nil {
		return m.store.Remove(datastore.AuthTokenName)
	}
	return nil
}

func (m *MenderAuthManager) HasKey() bool {
	return m.keyStore.Private() != nil
}

func (m *MenderAuthManager) GenerateKey() error {
	if err := m.keyStore.Generate(); err != nil {
		log.Errorf("failed to generate device key: %v", err)
		return errors.Wrapf(err, "failed to generate device key")
	}

	if err := m.keyStore.Save(); err != nil {
		log.Errorf("failed to save device key: %s", err)
		return NewFatalError(err)
	}
	return nil
}
