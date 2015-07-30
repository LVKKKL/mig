// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
//
// Contributor: Aaron Meihm ameihm@mozilla.com [:alm]

package mig /* import "mig.ninja/mig" */

// This file contains structures and functions related to the handling of
// manifests and state bundles by the MIG loader and API.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mig.ninja/mig/pgp"
	"os"
	"path"
	"runtime"
	"time"
)

// Describes a manifest record stored within the MIG database
type ManifestRecord struct {
	ID         float64   `json:"id"`                // Manifest record ID
	Name       string    `json:"name"`              // The name of the manifest record
	Content    string    `json:"content,omitempty"` // Full data contents of record
	Timestamp  time.Time `json:"timestamp"`         // Record timestamp
	Status     string    `json:"status"`            // Record status
	Target     string    `json:"target"`            // Targetting parameters for record
	Signatures []string  `json:"signatures"`        // Signatures applied to the record
}

func (m *ManifestRecord) Sign(keyid string, secring io.Reader) (sig string, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("Sign() -> %v", e)
		}
	}()

	// Convert the record into entry format, and strip existing signatures
	// before signing.
	me, err := m.ManifestResponse()
	if err != nil {
		panic(err)
	}
	me.Signatures = me.Signatures[:0]
	buf, err := json.Marshal(me)
	if err != nil {
		panic(err)
	}
	sig, err = pgp.Sign(string(buf), keyid, secring)
	if err != nil {
		panic(err)
	}
	return
}

// Convert a manifest record into a manifest response
func (m *ManifestRecord) ManifestResponse() (ManifestResponse, error) {
	ret := ManifestResponse{}

	if len(m.Content) == 0 {
		return ret, fmt.Errorf("manifest record has no content")
	}

	buf := bytes.NewBufferString(m.Content)
	b64r := base64.NewDecoder(base64.StdEncoding, buf)
	gzr, err := gzip.NewReader(b64r)
	if err != nil {
		return ret, err
	}
	tr := tar.NewReader(gzr)
	for {
		h, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return ret, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}

		hash := sha256.New()
		rbuf := make([]byte, 4096)
		for {
			n, err := tr.Read(rbuf)
			if err != nil {
				if err == io.EOF {
					break
				}
				return ret, err
			}
			if n > 0 {
				hash.Write(rbuf[:n])
			}
		}

		_, entname := path.Split(h.Name)

		newEntry := ManifestEntry{}
		newEntry.Name = entname
		newEntry.SHA256 = fmt.Sprintf("%x", hash.Sum(nil))
		ret.Entries = append(ret.Entries, newEntry)
	}
	ret.Signatures = m.Signatures

	return ret, nil
}

// Returns the requested file object as a gzip compressed byte slice
// from the manifest record
func (m *ManifestRecord) ManifestObject(obj string) ([]byte, error) {
	var bufw bytes.Buffer
	var ret []byte

	bufr := bytes.NewBufferString(m.Content)
	b64r := base64.NewDecoder(base64.StdEncoding, bufr)
	gzr, err := gzip.NewReader(b64r)
	if err != nil {
		return ret, err
	}
	tr := tar.NewReader(gzr)
	found := false
	for {
		h, err := tr.Next()
		if err != nil {
			if err == io.EOF {
				break
			}
			return ret, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		_, thisf := path.Split(h.Name)
		if thisf != obj {
			continue
		}
		found = true
		gzw := gzip.NewWriter(&bufw)
		buftemp := make([]byte, 4096)
		for {
			n, err := tr.Read(buftemp)
			if err != nil {
				if err == io.EOF {
					break
				}
				return ret, err
			}
			if n > 0 {
				_, err = gzw.Write(buftemp[:n])
				if err != nil {
					return ret, err
				}
			}
		}
		gzw.Close()
		break
	}
	if !found {
		return ret, fmt.Errorf("object %v not found in manifest", obj)
	}

	ret = bufw.Bytes()
	return ret, nil
}

// Manifest parameters are sent from the loader to the API as part of
// a manifest request.
type ManifestParameters struct {
	AgentIdentifier Agent  `json:"agent"`     // Agent context information
	LoaderKey       string `json:"loaderkey"` // Loader authorization key
	Object          string `json:"object"`    // Object being requested
}

// Validate parameters included in a manifest request
func (m *ManifestParameters) Validate() error {
	if m.LoaderKey == "" {
		return fmt.Errorf("manifest parameters with no loader key")
	}
	return nil
}

// Validate parameters included in a manifest request with an object fetch
// component
func (m *ManifestParameters) ValidateFetch() error {
	err := m.Validate()
	if err != nil {
		return err
	}
	if m.Object == "" {
		return fmt.Errorf("manifest fetch with no object")
	}
	return m.Validate()
}

// The response to a manifest object fetch
type ManifestFetchResponse struct {
	Data []byte `json:"data"`
}

// The response to a standard manifest request
type ManifestResponse struct {
	Entries    []ManifestEntry `json:"entries"`
	Signatures []string        `json:"signatures"`
}

// Describes individual file elements within a manifest
type ManifestEntry struct {
	Name   string `json:"name"`   // Corresponds to a bundle name
	SHA256 string `json:"sha256"` // SHA256 of entry
}

// The bundle dictionary is used to map tokens within the loader manifest to
// objects on the file system. We don't allow specification of an exact path
// for interrogation or manipulation in the manifest. This results in some
// restrictions but hardens the loader against making unauthorized changes
// to the file system.
type BundleDictionaryEntry struct {
	Name   string
	Path   string
	SHA256 string
}

var bundleEntryLinux = []BundleDictionaryEntry{
	{"mig-agent", "/sbin/mig-agent", ""},
	{"configuration", "/etc/mig/mig-agent.cfg", ""},
}

var BundleDictionary = map[string][]BundleDictionaryEntry{
	"linux": bundleEntryLinux,
}

func GetHostBundle() ([]BundleDictionaryEntry, error) {
	switch runtime.GOOS {
	case "linux":
		return bundleEntryLinux, nil
	}
	return nil, fmt.Errorf("no entry for %v in bundle dictionary", runtime.GOOS)
}

func HashBundle(b []BundleDictionaryEntry) ([]BundleDictionaryEntry, error) {
	ret := b
	for i := range ret {
		fd, err := os.Open(ret[i].Path)
		if err != nil {
			// If the file does not exist we don't treat this as as
			// an error. This is likely in cases with embedded
			// configurations. In this case we leave the SHA256 as
			// an empty string.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		h := sha256.New()
		buf := make([]byte, 4096)
		for {
			n, err := fd.Read(buf)
			if err != nil {
				if err == io.EOF {
					break
				}
				fd.Close()
				return nil, err
			}
			if n > 0 {
				h.Write(buf[:n])
			}
		}
		fd.Close()
		ret[i].SHA256 = fmt.Sprintf("%x", h.Sum(nil))
	}
	return ret, nil
}
