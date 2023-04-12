/*******************************************************************************
*
* Copyright 2019 SAP SE
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You should have received a copy of the License along with this
* program. If not, you may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
*******************************************************************************/

package util

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/majewsky/schwift"
	"github.com/sapcc/go-bits/logg"

	//nolint:staticcheck // We cannot switch to the Protonmail fork because we need support for old v3 signatures as found in SLES12 packages.
	"golang.org/x/crypto/openpgp"
	//nolint:staticcheck // see above
	"golang.org/x/crypto/openpgp/armor"
	//nolint:staticcheck // see above
	"golang.org/x/crypto/openpgp/clearsign"
	//nolint:staticcheck // see above
	"golang.org/x/crypto/openpgp/packet"
)

// GPGKeyRing contains a list of openpgp Entities. It is used to verify different
// types of GPG signatures.
// If a new key is discovered/downloaded, it is uploaded to the SwiftContainer.
type GPGKeyRing struct {
	EntityList openpgp.EntityList
	Mux        sync.RWMutex

	KeyserverURLPatterns []string
	SwiftContainer       *schwift.Container
}

// NewGPGKeyRing creates a new GPGKeyRing instance.
func NewGPGKeyRing(cntr *schwift.Container, keyserverURLPatterns []string) *GPGKeyRing {
	ksURLPatterns := keyserverURLPatterns
	if len(ksURLPatterns) == 0 {
		ksURLPatterns = append(ksURLPatterns,
			"https://keyserver.ubuntu.com/pks/lookup?search=0x{keyid}&options=mr&op=get",
			"https://pgp.mit.edu/pks/lookup?search=0x{keyid}&options=mr&op=get")
	}

	//Get cached public keys from Swift container.
	var entityList openpgp.EntityList
	if cntr != nil {
		logg.Info("restoring GPG public keys from %s", cntr.Name())
		err := cntr.Objects().Foreach(func(obj *schwift.Object) error {
			r, err := obj.Download(nil).AsReadCloser()
			if err != nil {
				return err
			}
			el, err := openpgp.ReadArmoredKeyRing(r)
			if err != nil {
				return err
			}
			for _, e := range el {
				entityList = append(entityList, e)
				if LogIndividualTransfers {
					logg.Info("reusing cached GPG key: %s", obj.FullName())
				}
			}
			return nil
		})
		if err != nil {
			logg.Info("could not restore GPG public keys from container: %s: %s", cntr.Name(), err.Error())
		}
	}

	return &GPGKeyRing{
		EntityList:           entityList,
		KeyserverURLPatterns: ksURLPatterns,
		SwiftContainer:       cntr,
	}
}

// VerifyClearSignedGPGSignature takes a clear signed message and checks if the
// signature is valid.
// If the key ring does not contain the concerning public key then the key is downloaded
// from a pool server and added to the existing key ring.
// A non-nil error is returned, if signature verification was unsuccessful.
func (k *GPGKeyRing) VerifyClearSignedGPGSignature(messageWithSignature []byte) error {
	block, _ := clearsign.Decode(messageWithSignature)
	return k.verifyGPGSignature(block.Bytes, block.ArmoredSignature)
}

// VerifyDetachedGPGSignature takes a message along with its detached signature
// and checks if the signature is valid. The detached signature is expected to
// be armored.
// If the key ring does not contain the concerning public key then the key is downloaded
// from a pool server and added to the existing key ring.
// A non-nil error is returned, if signature verification was unsuccessful.
func (k *GPGKeyRing) VerifyDetachedGPGSignature(message, armoredSignature []byte) error {
	block, err := armor.Decode(bytes.NewReader(armoredSignature))
	if err != nil {
		return err
	}
	return k.verifyGPGSignature(message, block)
}

func (k *GPGKeyRing) verifyGPGSignature(message []byte, signature *armor.Block) error {
	if signature.Type != openpgp.SignatureType {
		return fmt.Errorf("invalid OpenPGP armored structure: expected %q, got %q", openpgp.SignatureType, signature.Type)
	}

	var publicKeyBytes []byte
	signatureBytes, err := io.ReadAll(signature.Body)
	if err != nil {
		return err
	}
	r := packet.NewReader(bytes.NewReader(signatureBytes))
	for {
		p, err := r.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}

		var issuerKeyID uint64
		switch sig := p.(type) {
		case *packet.Signature:
			issuerKeyID = *sig.IssuerKeyId
		case *packet.SignatureV3:
			issuerKeyID = sig.IssuerKeyId
		default:
			return fmt.Errorf("invalid OpenPGP packet type: expected %q, got %T", "*packet.Signature", p)
		}

		//only download the public key if not found in the existing key ring
		k.Mux.RLock()
		foundKeys := k.EntityList.KeysById(issuerKeyID)
		k.Mux.RUnlock()
		if len(foundKeys) == 0 {
			b, err := k.getPublicKey(fmt.Sprintf("%X", issuerKeyID))
			if err != nil {
				return err
			}
			publicKeyBytes = append(publicKeyBytes, b...)
		}
	}

	//add the downloaded keys to the existing key ring
	if len(publicKeyBytes) != 0 {
		el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(publicKeyBytes))
		if err != nil {
			return err
		}
		k.Mux.Lock()
		k.EntityList = append(k.EntityList, el...)
		k.Mux.Unlock()
	}

	k.Mux.RLock()
	_, err = openpgp.CheckDetachedSignature(k.EntityList, bytes.NewReader(message), bytes.NewReader(signatureBytes))
	k.Mux.RUnlock()

	return err
}

func (k *GPGKeyRing) getPublicKey(id string) ([]byte, error) {
	logg.Info("retrieving public key for ID %q", id)

	for i, v := range k.KeyserverURLPatterns {
		url := strings.Replace(v, "{keyid}", id, -1)
		buf, err := getPublicKeyFromServer(url)
		if err == nil {
			return uploadPublicKey(k.SwiftContainer, buf)
		}

		if i == len(k.KeyserverURLPatterns)-1 {
			logg.Error("could not retrieve public key for ID %q from %s: %s (no more servers to try)", id, url, err.Error())
		} else {
			logg.Error("could not retrieve public key for ID %q from %s: %s (will try next server)", id, url, err.Error())
		}
	}

	return nil, errNoSuchPublicKey
}

var (
	noPublicKeyFoundRx = regexp.MustCompile(`not?\b.*found`)
	errNoSuchPublicKey = errors.New("no such public key")
)

func getPublicKeyFromServer(uri string) ([]byte, error) {
	resp, err := http.Get(uri) //nolint:gosec // potential servers are hardcoded in configuration and URL needs to be variable
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if noPublicKeyFoundRx.MatchString(strings.ToLower(string(b))) {
		return nil, errNoSuchPublicKey
	}

	return b, nil
}

func uploadPublicKey(cntr *schwift.Container, b []byte) ([]byte, error) {
	if cntr == nil {
		return b, nil
	}
	n := fmt.Sprintf("%x.asc", sha256.Sum256(b))
	obj := cntr.Object(n)
	err := obj.Upload(bytes.NewReader(b), nil, nil)
	if err == nil && LogIndividualTransfers {
		logg.Info("transferring GPG key to cache at %s", obj.FullName())
	}
	return b, err
}
