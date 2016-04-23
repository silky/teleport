/*
Copyright 2016 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package client

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/gravitational/teleport/lib/sshutils"
	"github.com/gravitational/teleport/lib/utils"

	log "github.com/Sirupsen/logrus"
	"github.com/gravitational/trace"

	"golang.org/x/crypto/ssh"
)

const (
	defaultKeyDir      = ".tsh"
	sessionKeyDir      = "sessions"
	fileNameCert       = "cert"
	fileNameKey        = "key"
	fileNamePub        = "pub"
	fileNameKnownHosts = "known_hosts"
)

// FSLocalKeyStore implements LocalKeyStore interface using the filesystem
// Here's the file layout for the FS store:
// ~/.tsh/
// ├── known_hosts   --> trusted certificate authorities (their keys) in a format similar to known_hosts
// └── sessions      --> server-signed session keys
//     └── host-a
//     |   ├── cert
//     |   ├── key
//     |   └── pub
//     └── host-b
//         ├── cert
//         ├── key
//         └── pub
type FSLocalKeyStore struct {
	LocalKeyStore

	// KeyDir is the directory where all keys are stored
	KeyDir string
}

// NewFSLocalKeyStore creates a new filesystem-based local keystore object
// and initializes it.
//
// if dirPath is empty, sets it to ~/.tsh
func NewFSLocalKeyStore(dirPath string) (s *FSLocalKeyStore, err error) {
	log.Infof("using FSLocalKeyStore")
	dirPath, err = initKeysDir(dirPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return &FSLocalKeyStore{
		KeyDir: dirPath,
	}, nil
}

// GetKeys returns all user session keys stored in the store
func (fs *FSLocalKeyStore) GetKeys() (keys []Key, err error) {
	dirPath := filepath.Join(fs.KeyDir, sessionKeyDir)
	if !utils.IsDir(dirPath) {
		return make([]Key, 0), nil
	}
	dirEntries, err := ioutil.ReadDir(dirPath)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	for _, fi := range dirEntries {
		if !fi.IsDir() {
			continue
		}
		k, err := fs.GetKey(fi.Name())
		if err != nil {
			// if a key is reported as 'not found' it's probably because it expired
			if !trace.IsNotFound(err) {
				return nil, trace.Wrap(err)
			}
			continue
		}
		keys = append(keys, *k)
	}
	return keys, nil
}

// AddKey adds a new key to the session store. If a key for the host is already
// stored, overwrites it.
func (fs *FSLocalKeyStore) AddKey(host string, key *Key) error {
	dirPath, err := fs.dirFor(host)
	if err != nil {
		return trace.Wrap(err)
	}
	writeBytes := func(fname string, data []byte) error {
		fp := filepath.Join(dirPath, fname)
		err := ioutil.WriteFile(fp, data, 0640)
		if err != nil {
			log.Error(err)
		}
		return err
	}
	if err = writeBytes(fileNameCert, key.Cert); err != nil {
		return trace.Wrap(err)
	}
	if err = writeBytes(fileNamePub, key.Pub); err != nil {
		return trace.Wrap(err)
	}
	if err = writeBytes(fileNameKey, key.Priv); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// GetKey returns a key for a given host. If the key is not found,
// returns trace.NotFound error.
func (fs *FSLocalKeyStore) GetKey(host string) (*Key, error) {
	dirPath, err := fs.dirFor(host)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	certFile := filepath.Join(dirPath, fileNameCert)
	cert, err := ioutil.ReadFile(certFile)
	if err != nil {
		log.Error(err)
		return nil, trace.Wrap(err)
	}
	pub, err := ioutil.ReadFile(filepath.Join(dirPath, fileNamePub))
	if err != nil {
		log.Error(err)
		return nil, trace.Wrap(err)
	}
	priv, err := ioutil.ReadFile(filepath.Join(dirPath, fileNameKey))
	if err != nil {
		log.Error(err)
		return nil, trace.Wrap(err)
	}

	key := &Key{Pub: pub, Priv: priv, Cert: cert}

	// expired certificate? this key won't be accepted anymore, lets delete it:
	certExpiration, err := key.CertValidBefore()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	log.Infof("returning cert %v valid until %v", certFile, certExpiration)
	if certExpiration.Before(time.Now().UTC()) {
		log.Infof("TTL expired (%v) for session key %v", certExpiration, dirPath)
		os.RemoveAll(dirPath)
		return nil, trace.NotFound("session keys for %s are not found", host)
	}
	return key, nil
}

// AddKnownHost adds a new entry to 'known_CAs' file
func (fs *FSLocalKeyStore) AddKnownCA(domainName string, hostKeys []ssh.PublicKey) error {
	fp, err := os.OpenFile(filepath.Join(fs.KeyDir, fileNameKnownHosts), os.O_CREATE|os.O_APPEND|os.O_RDWR, 0640)
	if err != nil {
		return trace.Wrap(err)
	}
	defer fp.Close()
	for i := range hostKeys {
		bytes := ssh.MarshalAuthorizedKey(hostKeys[i])
		log.Infof("adding known CA %v %v", domainName, sshutils.Fingerprint(hostKeys[i]))
		fmt.Fprintf(fp, "%s %s\n", domainName, bytes)
	}
	return nil
}

// GetKnownHost returns public keys of all trusted CAs
func (fs *FSLocalKeyStore) GetKnownCAs() ([]ssh.PublicKey, error) {
	bytes, err := ioutil.ReadFile(filepath.Join(fs.KeyDir, fileNameKnownHosts))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, trace.Wrap(err)
	}

	var (
		pubKey ssh.PublicKey
		retval []ssh.PublicKey = make([]ssh.PublicKey, 0)
	)
	for err == nil {
		_, _, pubKey, _, bytes, err = ssh.ParseKnownHosts(bytes)
		if err == nil {
			retval = append(retval, pubKey)
		}
	}
	if err != io.EOF {
		return nil, trace.Wrap(err)
	}
	return retval, nil
}

// dirFor is a helper function. It returns a directory where session keys
// for a given host are stored
func (fs *FSLocalKeyStore) dirFor(hostname string) (string, error) {
	dirPath := filepath.Join(fs.KeyDir, sessionKeyDir, hostname)
	if err := os.MkdirAll(dirPath, 0777); err != nil {
		log.Error(err)
		return "", trace.Wrap(err)
	}
	return dirPath, nil
}

// initKeysDir initializes the keystore root directory. Usually it is ~/.tsh
func initKeysDir(dirPath string) (string, error) {
	var err error
	// not specified? use `~/.tsh`
	if dirPath == "" {
		u, err := user.Current()
		if err != nil {
			dirPath = os.TempDir()
		} else {
			dirPath = u.HomeDir
		}
		dirPath = filepath.Join(dirPath, defaultKeyDir)
	}
	// create if doesn't exist:
	_, err = os.Stat(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			err = os.MkdirAll(dirPath, os.ModeDir|0777)
			if err != nil {
				return "", trace.Wrap(err)
			}
		} else {
			return "", trace.Wrap(err)
		}
	}

	return dirPath, nil
}
