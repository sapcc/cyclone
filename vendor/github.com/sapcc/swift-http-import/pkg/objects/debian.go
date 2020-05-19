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

package objects

import (
	"bytes"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/majewsky/schwift"
	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/swift-http-import/pkg/util"
	"pault.ag/go/debian/control"
)

//'Packages' indices
//Reference:
//  For '$COMP/binary-$ARCH/Packages.(gz|xz)' or
//  '$COMP/debian-installer/binary-$ARCH/Packages.(gz|xz)'.
//
//  matchList[1] = "$COMP"
//  matchList[3] = "$ARCH"
var debReleasePackagesEntryRx = regexp.MustCompile(`^([a-zA-Z]+)/(debian-installer/)?binary-(\w+)/Packages(\.gz|\.xz)$`)

//DebianSource is a URLSource containing a Debian repository. This type reuses
//the Validate() and Connect() logic of URLSource, but adds a custom scraping
//implementation that reads the Debian repository metadata instead of relying
//on directory listings.
type DebianSource struct {
	//options from config file
	URLString                string   `yaml:"url"`
	ClientCertificatePath    string   `yaml:"cert"`
	ClientCertificateKeyPath string   `yaml:"key"`
	ServerCAPath             string   `yaml:"ca"`
	Distributions            []string `yaml:"dist"`
	Architectures            []string `yaml:"arch"`
	VerifySignature          *bool    `yaml:"verify_signature"`
	//compiled configuration
	urlSource       *URLSource       `yaml:"-"`
	gpgVerification bool             `yaml:"-"`
	gpgKeyRing      *util.GPGKeyRing `yaml:"-"`
}

//Validate implements the Source interface.
func (s *DebianSource) Validate(name string) []error {
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
func (s *DebianSource) Connect(name string) error {
	return s.urlSource.Connect(name)
}

//ListEntries implements the Source interface.
func (s *DebianSource) ListEntries(directoryPath string) ([]FileSpec, *ListEntriesError) {
	return nil, &ListEntriesError{
		Location: s.urlSource.getURLForPath(directoryPath).String(),
		Message:  "ListEntries is not implemented for DebianSource",
	}
}

//GetFile implements the Source interface.
func (s *DebianSource) GetFile(directoryPath string, requestHeaders schwift.ObjectHeaders) (body io.ReadCloser, sourceState FileState, err error) {
	return s.urlSource.GetFile(directoryPath, requestHeaders)
}

//ListAllFiles implements the Source interface.
func (s *DebianSource) ListAllFiles() ([]FileSpec, *ListEntriesError) {
	if len(s.Distributions) == 0 {
		return nil, &ListEntriesError{
			Location: s.URLString,
			Message:  "no distributions specified in the config file",
		}
	}

	cache := make(map[string]FileSpec)

	//since package and source files for different distributions are kept in
	//the common '$REPO_ROOT/pool' directory therefore a record is kept of
	//unique files in order to avoid duplicates in the allFiles slice
	var allFiles []string
	isDuplicate := make(map[string]bool)

	//index files for different distributions as specified in the config file
	for _, distName := range s.Distributions {
		distRootPath := filepath.Join("dists", distName)
		distFiles, lerr := s.listDistFiles(distRootPath, cache)
		if lerr != nil {
			return nil, lerr
		}

		for _, file := range distFiles {
			if !isDuplicate[file] {
				allFiles = append(allFiles, file)
				isDuplicate[file] = true
			}
		}
	}

	//for files that were already downloaded, pass the contents and HTTP headers
	//into the transfer phase to avoid double download
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

//Helper function for DebianSource.ListAllFiles().
func (s *DebianSource) listDistFiles(distRootPath string, cache map[string]FileSpec) ([]string, *ListEntriesError) {
	var distFiles []string

	//parse 'inRelease' file to find paths of other control files
	releasePath := filepath.Join(distRootPath, "InRelease")

	var release struct {
		Architectures []string                 `control:"Architectures" delim:" " strip:" "`
		Entries       []control.SHA256FileHash `control:"SHA256" delim:"\n" strip:"\n\r\t "`
	}

	releaseBytes, releaseURI, lerr := s.downloadAndParseDCF(releasePath, &release, cache)
	if lerr != nil {
		//some older distros only have the legacy 'Release' file
		releasePath = filepath.Join(distRootPath, "Release")
		releaseBytes, releaseURI, lerr = s.downloadAndParseDCF(releasePath, &release, cache)
		if lerr != nil {
			return nil, lerr
		}
	}

	//verify release file's GPG signature
	if s.gpgVerification {
		var signatureURI string
		var err error
		if filepath.Base(releasePath) == "Release" {
			var signatureBytes []byte
			signaturePath := filepath.Join(distRootPath, "Release.gpg")
			signatureBytes, signatureURI, lerr = s.urlSource.getFileContents(signaturePath, cache)
			if lerr != nil {
				return nil, lerr
			}
			err = util.VerifyDetachedGPGSignature(s.gpgKeyRing, releaseBytes, signatureBytes)
		} else {
			signatureURI = releaseURI
			err = util.VerifyClearSignedGPGSignature(s.gpgKeyRing, releaseBytes)
		}
		if err != nil {
			logg.Debug("could not verify GPG signature at %s for file %s", signatureURI, "-"+filepath.Base(releasePath))
			return nil, &ListEntriesError{
				Location: s.urlSource.getURLForPath("/").String(),
				Message:  ErrMessageGPGVerificationFailed,
				Inner:    err,
			}
		}
		logg.Debug("successfully verified GPG signature at %s for file %s", signatureURI, "-"+filepath.Base(releasePath))
	}

	//the architectures that we are interested in
	architectures := release.Architectures
	if len(s.Architectures) != 0 {
		architectures = s.Architectures
	}

	//some repos offer multiple compression types for the same 'Sources' and
	//'Packages' indices. These maps contain the indices with out their file
	//extension. This allows us to choose a compression type at the time of
	//parsing and avoids parsing the same index multiple times.
	sourceIndices := make(map[string]bool)
	packageIndices := make(map[string]bool)

	//note control files for transfer
	for _, entry := range release.Entries {
		//entry.Filename is relative to distRootPath therefore
		fileName := stripFileExtension(filepath.Join(distRootPath, entry.Filename))

		//note all 'Sources' indices as they are architecture independent
		if strings.HasSuffix(entry.Filename, "Sources.gz") || strings.HasSuffix(entry.Filename, "Sources.xz") {
			sourceIndices[fileName] = true
			continue // to next entry
		}

		//note architecture specific 'Packages' indices
		matchList := debReleasePackagesEntryRx.FindStringSubmatch(entry.Filename)
		if matchList != nil {
			for _, arch := range architectures {
				if matchList[3] == arch {
					packageIndices[fileName] = true
				}
			}
		}
	}

	//parse 'Packages' indices to find paths for package files (.deb)
	for pkgIndexPath := range packageIndices {
		var packageIndex []struct {
			Filename string `control:"Filename"`
		}
		//get package index from 'Packages.xz'
		_, _, lerr := s.downloadAndParseDCF(pkgIndexPath+".xz", &packageIndex, cache)
		if lerr != nil {
			//some older distros only have 'Packages.gz'
			_, _, lerr = s.downloadAndParseDCF(pkgIndexPath+".gz", &packageIndex, cache)
			if lerr != nil {
				return nil, lerr
			}
		}

		for _, pkg := range packageIndex {
			distFiles = append(distFiles, pkg.Filename)
		}
	}

	//parse 'Sources' indices to find paths for source files (.dsc, .tar.gz, etc.)
	for srcIndexPath := range sourceIndices {
		var sourceIndex []struct {
			Directory string                `control:"Directory"`
			Files     []control.MD5FileHash `control:"Files" delim:"\n" strip:"\n\r\t "`
		}

		//get source index from 'Sources.xz'
		_, _, lerr := s.downloadAndParseDCF(srcIndexPath+".xz", &sourceIndex, cache)
		if lerr != nil {
			//some older distros only have 'Sources.gz'
			_, _, lerr = s.downloadAndParseDCF(srcIndexPath+".gz", &sourceIndex, cache)
			if lerr != nil {
				return nil, lerr
			}
		}

		for _, src := range sourceIndex {
			for _, file := range src.Files {
				distFiles = append(distFiles, filepath.Join(src.Directory, file.Filename))
			}
		}
	}

	//transfer files in '$DIST_ROOT' at the very end, when package and source
	//files have already been uploaded (to avoid situations where a client
	//might see repository metadata without being able to see the referenced
	//packages)
	entries, lerr := s.recursivelyListEntries(distRootPath)
	if lerr != nil {
		if !strings.Contains(lerr.Message, "GET returned status 404") {
			return nil, lerr
		}
	}
	distFiles = append(distFiles, entries...)

	return distFiles, nil
}

//Helper function for DebianSource.ListAllFiles().
func (s *DebianSource) downloadAndParseDCF(path string, data interface{}, cache map[string]FileSpec) (contents []byte, uri string, e *ListEntriesError) {
	buf, uri, lerr := s.urlSource.getFileContents(path, cache)
	if lerr != nil {
		return nil, uri, lerr
	}

	//if `buf` has the magic number for XZ, decompress before parsing as DCF
	if bytes.HasPrefix(buf, xzMagicNumber) {
		var err error
		buf, err = decompressXZArchive(buf)
		if err != nil {
			return nil, uri, &ListEntriesError{Location: uri, Message: "cannot decompress xz stream", Inner: err}
		}
	}

	//if `buf` has the magic number for GZip, decompress before parsing as DCF
	if bytes.HasPrefix(buf, gzipMagicNumber) {
		var err error
		buf, err = decompressGZipArchive(buf)
		if err != nil {
			return nil, uri, &ListEntriesError{Location: uri, Message: "cannot decompress gzip stream", Inner: err}
		}
	}

	err := control.Unmarshal(data, bytes.NewReader(buf))
	if err != nil {
		return nil, uri, &ListEntriesError{
			Location: uri,
			Message:  "error while parsing Debian Control File",
			Inner:    err,
		}
	}

	return buf, uri, nil
}

//Helper function for DebianSource.ListAllFiles().
func stripFileExtension(fileName string) string {
	ext := filepath.Ext(fileName)
	if ext == "" {
		return fileName
	}

	return strings.TrimSuffix(fileName, ext)
}

//Helper function for DebianSource.ListAllFiles().
func (s *DebianSource) recursivelyListEntries(path string) ([]string, *ListEntriesError) {
	var files []string

	entries, lerr := s.urlSource.ListEntries(path)
	if lerr != nil {
		return nil, lerr
	}

	for _, entry := range entries {
		if entry.IsDirectory {
			tmpFiles, lerr := s.recursivelyListEntries(entry.Path)
			if lerr != nil {
				return nil, lerr
			}
			files = append(files, tmpFiles...)
		} else {
			files = append(files, entry.Path)
		}
	}

	return files, nil
}
