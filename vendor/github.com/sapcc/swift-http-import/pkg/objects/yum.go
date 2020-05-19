/*******************************************************************************
*
* Copyright 2017 SAP SE
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

package objects

import (
	"bytes"
	"encoding/xml"
	"io"
	"path/filepath"
	"strings"

	"github.com/majewsky/schwift"
	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/swift-http-import/pkg/util"
)

//YumSource is a URLSource containing a Yum repository. This type reuses the
//Validate() and Connect() logic of URLSource, but adds a custom scraping
//implementation that reads the Yum repository metadata instead of relying on
//directory listings.
type YumSource struct {
	//options from config file
	URLString                string   `yaml:"url"`
	ClientCertificatePath    string   `yaml:"cert"`
	ClientCertificateKeyPath string   `yaml:"key"`
	ServerCAPath             string   `yaml:"ca"`
	Architectures            []string `yaml:"arch"`
	VerifySignature          *bool    `yaml:"verify_signature"`
	//compiled configuration
	urlSource       *URLSource       `yaml:"-"`
	gpgVerification bool             `yaml:"-"`
	gpgKeyRing      *util.GPGKeyRing `yaml:"-"`
}

//Validate implements the Source interface.
func (s *YumSource) Validate(name string) []error {
	s.urlSource = &URLSource{
		URLString:                s.URLString,
		ClientCertificatePath:    s.ClientCertificatePath,
		ClientCertificateKeyPath: s.ClientCertificateKeyPath,
		ServerCAPath:             s.ServerCAPath,
	}
	s.gpgVerification = true
	if s.VerifySignature != nil {
		s.gpgVerification = *s.VerifySignature
	}
	return s.urlSource.Validate(name)
}

//Connect implements the Source interface.
func (s *YumSource) Connect(name string) error {
	return s.urlSource.Connect(name)
}

//ListEntries implements the Source interface.
func (s *YumSource) ListEntries(directoryPath string) ([]FileSpec, *ListEntriesError) {
	return nil, &ListEntriesError{
		Location: s.urlSource.getURLForPath(directoryPath).String(),
		Message:  "ListEntries is not implemented for YumSource",
	}
}

//GetFile implements the Source interface.
func (s *YumSource) GetFile(directoryPath string, requestHeaders schwift.ObjectHeaders) (body io.ReadCloser, sourceState FileState, err error) {
	return s.urlSource.GetFile(directoryPath, requestHeaders)
}

//ListAllFiles implements the Source interface.
func (s *YumSource) ListAllFiles() ([]FileSpec, *ListEntriesError) {
	cache := make(map[string]FileSpec)
	var allFiles []string

	repomdPath := "repodata/repomd.xml"
	//parse repomd.xml to find paths of all other metadata files
	var repomd struct {
		Entries []struct {
			Type     string `xml:"type,attr"`
			Location struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"data"`
	}
	repomdBytes, repomdURL, lerr := s.downloadAndParseXML(repomdPath, &repomd, cache)
	if lerr != nil {
		return nil, lerr
	}

	//verify repomd's GPG signature
	if s.gpgVerification {
		signaturePath := repomdPath + ".asc"
		signatureBytes, signatureURI, lerr := s.urlSource.getFileContents(signaturePath, cache)
		if lerr == nil {
			err := util.VerifyDetachedGPGSignature(s.gpgKeyRing, repomdBytes, signatureBytes)
			if err != nil {
				logg.Debug("could not verify GPG signature at %s for file %s", signatureURI, "-"+filepath.Base(repomdPath))
				return nil, &ListEntriesError{
					Location: s.urlSource.getURLForPath("/").String(),
					Message:  ErrMessageGPGVerificationFailed,
					Inner:    err,
				}
			}
			allFiles = append(allFiles, signaturePath)
		} else {
			if !strings.Contains(lerr.Message, "GET returned status 404") {
				return nil, lerr
			}
		}
		logg.Debug("successfully verified GPG signature at %s for file %s", signatureURI, "-"+filepath.Base(repomdPath))
	}

	//note metadata files for transfer
	hrefsByType := make(map[string]string)
	for _, entry := range repomd.Entries {
		allFiles = append(allFiles, entry.Location.Href)
		hrefsByType[entry.Type] = entry.Location.Href
	}

	//parse primary.xml.gz to find paths of RPMs
	href, exists := hrefsByType["primary"]
	if !exists {
		return nil, &ListEntriesError{
			Location: repomdURL,
			Message:  "cannot find link to primary.xml.gz in repomd.xml",
		}
	}
	var primary struct {
		Packages []struct {
			Architecture string `xml:"arch"`
			Location     struct {
				Href string `xml:"href,attr"`
			} `xml:"location"`
		} `xml:"package"`
	}
	_, _, lerr = s.downloadAndParseXML(href, &primary, cache)
	if lerr != nil {
		return nil, lerr
	}
	for _, pkg := range primary.Packages {
		if s.handlesArchitecture(pkg.Architecture) {
			allFiles = append(allFiles, pkg.Location.Href)
		}
	}

	//parse prestodelta.xml.gz (if present) to find paths of DRPMs
	//(NOTE: this is called "deltainfo.xml.gz" on Suse)
	href, exists = hrefsByType["prestodelta"]
	if !exists {
		href, exists = hrefsByType["deltainfo"]
	}
	if exists {
		var prestodelta struct {
			Packages []struct {
				Architecture string `xml:"arch,attr"`
				Deltas       []struct {
					Href string `xml:"filename"`
				} `xml:"delta"`
			} `xml:"newpackage"`
		}
		_, _, lerr = s.downloadAndParseXML(href, &prestodelta, cache)
		if lerr != nil {
			return nil, lerr
		}
		for _, pkg := range prestodelta.Packages {
			if s.handlesArchitecture(pkg.Architecture) {
				for _, d := range pkg.Deltas {
					allFiles = append(allFiles, d.Href)
				}
			}
		}
	}

	//transfer repomd.xml.* files at the very end, when everything else has already been
	//uploaded (to avoid situations where a client might see repository metadata
	//without being able to see the referenced packages)
	repomdKeyPath := repomdPath + ".key"
	_, _, lerr = s.urlSource.getFileContents(repomdKeyPath, cache)
	if lerr == nil {
		allFiles = append(allFiles, repomdKeyPath)
	} else {
		if !strings.Contains(lerr.Message, "GET returned status 404") {
			return nil, lerr
		}
	}
	allFiles = append(allFiles, repomdPath)

	//for files that were already downloaded, pass the contents and HTTP headers
	//into the transfer phase to avoid double download
	//
	//This also ensures that the transferred set of packages is consistent with
	//the transferred repo metadata. If we were to download repomd.xml et al
	//again during the transfer step, there is a chance that new metadata has
	//been uploaded to the source in the meantime. In this case, we would be
	//missing the packages referenced only in the new metadata.
	result := make([]FileSpec, len(allFiles))
	for idx, path := range allFiles {
		var exists bool
		result[idx], exists = cache[path]
		if !exists {
			result[idx] = FileSpec{Path: path}
		}
	}
	return result, nil
}

//Helper function for YumSource.ListAllFiles().
func (s *YumSource) handlesArchitecture(arch string) bool {
	if len(s.Architectures) == 0 || arch == "" {
		return true
	}
	for _, val := range s.Architectures {
		if val == arch {
			return true
		}
	}
	return false
}

//Helper function for YumSource.ListAllFiles().
func (s *YumSource) downloadAndParseXML(path string, data interface{}, cache map[string]FileSpec) (contents []byte, uri string, e *ListEntriesError) {
	buf, uri, lerr := s.urlSource.getFileContents(path, cache)
	if lerr != nil {
		return nil, uri, lerr
	}

	//if `buf` has the magic number for GZip, decompress before parsing as XML
	if bytes.HasPrefix(buf, gzipMagicNumber) {
		var err error
		buf, err = decompressGZipArchive(buf)
		if err != nil {
			return nil, uri, &ListEntriesError{Location: uri, Message: "cannot decompress gzip stream", Inner: err}
		}
	}

	//if `buf` has the magic number for XZ, decompress before parsing as XML
	if bytes.HasPrefix(buf, xzMagicNumber) {
		var err error
		buf, err = decompressXZArchive(buf)
		if err != nil {
			return nil, uri, &ListEntriesError{Location: uri, Message: "cannot decompress xz stream", Inner: err}
		}
	}

	err := xml.Unmarshal(buf, data)
	if err != nil {
		return nil, uri, &ListEntriesError{
			Location: uri,
			Message:  "error while parsing XML",
			Inner:    err,
		}
	}

	return buf, uri, nil
}
