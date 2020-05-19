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
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/majewsky/schwift"
	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/swift-http-import/pkg/util"
)

//Source describes a place from which files can be fetched.
type Source interface {
	//Validate reports errors if this source is malspecified.
	Validate(name string) []error
	//Connect performs source-specific one-time setup.
	Connect(name string) error
	//ListAllFiles returns all files in the source (as paths relative to the
	//source's root). If this returns ErrListAllFilesNotSupported, ListEntries
	//must be used instead.
	ListAllFiles() ([]FileSpec, *ListEntriesError)
	//ListEntries returns all files and subdirectories at this path in the
	//source. Each result value must have a "/" prefix for subdirectories, or
	//none for files.
	ListEntries(directoryPath string) ([]FileSpec, *ListEntriesError)
	//GetFile retrieves the contents and metadata for the file at the given path
	//in the source. The `headers` map contains additional HTTP request headers
	//that shall be passed to the source in the GET request.
	GetFile(directoryPath string, headers schwift.ObjectHeaders) (body io.ReadCloser, sourceState FileState, err error)
}

//ListEntriesError is an error that occurs while scraping a directory.
type ListEntriesError struct {
	//the location of the directory (e.g. an URL)
	Location string
	//error message (either of those is optional, but at least one must be set)
	Message string
	Inner   error
}

//FullMessage returns the full error message for this error. This merges the
//.Messages with the .Inner.Error().
func (e ListEntriesError) FullMessage() string {
	if e.Inner == nil {
		return e.Message
	}
	msg := e.Inner.Error()
	if e.Message != "" {
		msg = e.Message + ": " + msg
	}
	return msg
}

//Some common values for ListEntriesError.Message that are always accompanied
//by an Inner error.
const (
	ErrMessageGPGVerificationFailed = "error while verifying GPG signature"
)

//ErrListAllFilesNotSupported is returned by ListAllFiles() for sources that do
//not support it.
var ErrListAllFilesNotSupported = &ListEntriesError{
	Message: "ListAllFiles not supported by this source",
}

//FileState is used by Source.GetFile() to describe the state of a file.
type FileState struct {
	Etag         string
	LastModified string
	SizeBytes    int64      //-1 if not known
	ExpiryTime   *time.Time //nil if not set
	//the following fields are only used in `sourceState`, not `targetState`
	SkipTransfer bool
	ContentType  string
}

////////////////////////////////////////////////////////////////////////////////

//URLSource describes a source that's accessible via HTTP.
type URLSource struct {
	URLString string   `yaml:"url"`
	URL       *url.URL `yaml:"-"`
	//auth options
	ClientCertificatePath    string       `yaml:"cert"`
	ClientCertificateKeyPath string       `yaml:"key"`
	ServerCAPath             string       `yaml:"ca"`
	HTTPClient               *http.Client `yaml:"-"`
	//transfer options
	SegmentingIn *bool  `yaml:"segmenting"`
	Segmenting   bool   `yaml:"-"`
	SegmentSize  uint64 `yaml:"segment_bytes"`
	//NOTE: All attributes that can be deserialized from YAML also need to be in
	//the custom source types (e.g. YumSource) with the same YAML field names.
}

//Validate implements the Source interface.
func (u *URLSource) Validate(name string) (result []error) {
	if u.URLString == "" {
		result = append(result, fmt.Errorf("missing value for %s.url", name))
	} else {
		//parse URL
		var err error
		u.URL, err = url.Parse(u.URLString)
		if err != nil {
			result = append(result, fmt.Errorf("invalid value for %s.url: %s", name, err.Error()))
		}

		//URL must refer to a directory, i.e. have a trailing slash
		if u.URL.Path == "" {
			u.URL.Path = "/"
			u.URL.RawPath = ""
		}
		if !strings.HasSuffix(u.URL.Path, "/") {
			logg.Error("source URL '%s' does not have a trailing slash (adding one for now; this will become a fatal error in future versions)", u.URLString)
			u.URL.Path += "/"
			if u.URL.RawPath != "" {
				u.URL.RawPath += "/"
			}
		}
	}

	// If one of the following is set, the other one needs also to be set
	if u.ClientCertificatePath != "" || u.ClientCertificateKeyPath != "" {
		if u.ClientCertificatePath == "" {
			result = append(result, fmt.Errorf("missing value for %s.cert", name))
		}
		if u.ClientCertificateKeyPath == "" {
			result = append(result, fmt.Errorf("missing value for %s.key", name))
		}
	}

	if u.SegmentingIn == nil {
		u.Segmenting = true
	} else {
		u.Segmenting = *u.SegmentingIn
	}
	if u.SegmentSize == 0 {
		u.SegmentSize = 512 << 20 //default: 512 MiB
	}

	return
}

//Connect implements the Source interface.
func (u *URLSource) Connect(name string) error {
	tlsConfig := &tls.Config{}

	if u.ClientCertificatePath != "" {
		// Load client cert
		clientCertificate, err := tls.LoadX509KeyPair(u.ClientCertificatePath, u.ClientCertificateKeyPath)
		if err != nil {
			return fmt.Errorf("cannot load client certificate from %s: %s", u.ClientCertificatePath, err.Error())
		}

		logg.Debug("Client certificate %s loaded", u.ClientCertificatePath)
		tlsConfig.Certificates = []tls.Certificate{clientCertificate}
	}

	if u.ServerCAPath != "" {
		// Load server CA cert
		serverCA, err := ioutil.ReadFile(u.ServerCAPath)
		if err != nil {
			return fmt.Errorf("cannot load CA certificate from %s: %s", u.ServerCAPath, err.Error())
		}

		certPool := x509.NewCertPool()
		certPool.AppendCertsFromPEM(serverCA)

		logg.Debug("Server CA %s loaded", u.ServerCAPath)
		tlsConfig.RootCAs = certPool
	}

	if u.ClientCertificatePath != "" || u.ServerCAPath != "" {
		tlsConfig.BuildNameToCertificate()
		// Overriding the transport for TLS, requires also Proxy to be set from ENV,
		// otherwise a set proxy will get lost
		transport := &http.Transport{TLSClientConfig: tlsConfig, Proxy: http.ProxyFromEnvironment}
		u.HTTPClient = &http.Client{Transport: transport}
	} else {
		u.HTTPClient = http.DefaultClient
	}

	return nil
}

//matches ".." path element
var dotdotRx = regexp.MustCompile(`(?:^|/)\.\.(?:$|/)`)

//ListAllFiles implements the Source interface.
func (u URLSource) ListAllFiles() ([]FileSpec, *ListEntriesError) {
	return nil, ErrListAllFilesNotSupported
}

//ListEntries implements the Source interface.
func (u URLSource) ListEntries(directoryPath string) ([]FileSpec, *ListEntriesError) {
	//get full URL of this subdirectory
	uri := u.getURLForPath(directoryPath)
	//to get a well-formatted directory listing, the directory URL must have a
	//trailing slash (most web servers automatically redirect from the URL
	//without trailing slash to the URL with trailing slash; others show a
	//slightly different directory listing that we cannot parse correctly)
	if !strings.HasSuffix(uri.Path, "/") {
		uri.Path += "/"
		if uri.RawPath != "" {
			uri.RawPath += "/"
		}
	}

	logg.Debug("scraping %s", uri)

	//retrieve directory listing
	//TODO: This should send "Accept: text/html", but at least Apache and nginx
	//don't care about the Accept header, anyway, as far as my testing showed.
	response, err := u.HTTPClient.Get(uri.String())
	if err != nil {
		return nil, &ListEntriesError{uri.String(), "GET failed", err}
	}
	defer response.Body.Close()

	//check that we actually got a directory listing
	if !strings.HasPrefix(response.Status, "2") {
		//DebianSource parses error message strings that end in "GET returned
		//status 404". Changes to this error format will break things on the
		//DebianSource end
		return nil, &ListEntriesError{uri.String(), "GET returned status " + response.Status, nil}
	}
	contentType := response.Header.Get("Content-Type")
	if !strings.HasPrefix(contentType, "text/html") {
		return nil, &ListEntriesError{uri.String(), "GET returned unexpected Content-Type: " + contentType, nil}
	}

	//find links inside the HTML document
	tokenizer := html.NewTokenizer(response.Body)
	var result []FileSpec
	for {
		tokenType := tokenizer.Next()

		switch tokenType {
		case html.ErrorToken:
			//end of document
			return result, nil
		case html.StartTagToken:
			token := tokenizer.Token()

			if token.DataAtom == atom.A {
				//found an <a> tag -- retrieve its href
				var href string
				for _, attr := range token.Attr {
					if attr.Key == "href" {
						href = attr.Val
						break
					}
				}
				if href == "" {
					continue
				}

				hrefURL, err := url.Parse(href)
				if err != nil {
					logg.Error("scrape %s: ignoring href attribute '%s' which is not a valid URL", uri.String(), href)
					continue
				}

				//filter external links with full URLs
				if hrefURL.Scheme != "" || hrefURL.Host != "" {
					continue
				}
				//ignore internal links, and links with a query part (Apache directory
				//listings use these for adjustable sorting)
				if hrefURL.RawQuery != "" || hrefURL.Fragment != "" {
					continue
				}
				//ignore absolute paths to the toplevel of this server, e.g. "/static/site.css")
				if strings.HasPrefix(hrefURL.Path, "/") {
					continue
				}

				//cleanup path, but retain trailing slash to tell directories and files apart
				linkPath := path.Clean(hrefURL.Path)
				if strings.HasSuffix(hrefURL.Path, "/") {
					linkPath += "/"
				}
				//ignore links leading outside the current directory
				if dotdotRx.MatchString(hrefURL.Path) {
					continue
				}

				result = append(result, FileSpec{
					Path:        filepath.Join(directoryPath, linkPath),
					IsDirectory: strings.HasSuffix(linkPath, "/"),
				})
			}
		}
	}
}

//GetFile implements the Source interface.
func (u URLSource) GetFile(directoryPath string, requestHeaders schwift.ObjectHeaders) (io.ReadCloser, FileState, error) {
	uri := u.getURLForPath(directoryPath).String()
	requestHeaders.Set("User-Agent", "swift-http-import/"+util.Version)

	//retrieve file from source
	var (
		response *http.Response
		err      error
	)
	if u.Segmenting {
		response, err = util.EnhancedGet(u.HTTPClient, uri, requestHeaders.ToHTTP(), u.SegmentSize)
	} else {
		var req *http.Request
		req, err := http.NewRequest("GET", uri, nil)
		if err == nil {
			for key, val := range requestHeaders.Headers {
				req.Header.Set(key, val)
			}
			response, err = u.HTTPClient.Do(req)
		}
	}
	if err != nil {
		return nil, FileState{}, fmt.Errorf("skipping %s: GET failed: %s", uri, err.Error())
	}

	if response.StatusCode != 200 && response.StatusCode != 304 {
		return nil, FileState{}, fmt.Errorf(
			"skipping %s: GET returned unexpected status code: expected 200 or 304, but got %d",
			uri, response.StatusCode,
		)
	}

	return response.Body, FileState{
		Etag:         response.Header.Get("Etag"),
		LastModified: response.Header.Get("Last-Modified"),
		SizeBytes:    response.ContentLength,
		ExpiryTime:   nil, //no way to get this information via HTTP only
		SkipTransfer: response.StatusCode == 304,
		ContentType:  response.Header.Get("Content-Type"),
	}, nil
}

//Return the URL for the given directoryPath below this URLSource.
func (u URLSource) getURLForPath(directoryPath string) *url.URL {
	return u.URL.ResolveReference(&url.URL{Path: strings.TrimPrefix(directoryPath, "/")})
}

//Helper function for custom source types.
func (u URLSource) getFileContents(path string, cache map[string]FileSpec) (contents []byte, uri string, e *ListEntriesError) {
	uri = u.getURLForPath(path).String()

	req, err := http.NewRequest("GET", uri, nil)
	if err != nil {
		return nil, uri, &ListEntriesError{uri, "GET failed", err}
	}

	resp, err := u.HTTPClient.Do(req)
	if err != nil {
		return nil, uri, &ListEntriesError{uri, "GET failed", err}
	}
	defer resp.Body.Close()

	result, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, uri, &ListEntriesError{uri, "GET failed", err}
	}
	if resp.StatusCode >= 400 {
		return nil, uri, &ListEntriesError{uri, fmt.Sprintf("GET returned status %d", resp.StatusCode), nil}
	}

	cache[path] = FileSpec{
		Path:     path,
		Contents: result,
		Headers:  resp.Header,
	}

	return result, uri, nil
}
