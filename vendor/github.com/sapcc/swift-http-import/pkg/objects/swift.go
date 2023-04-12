/*******************************************************************************
*
* Copyright 2016-2018 SAP SE
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
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/utils/client"
	"github.com/majewsky/schwift"
	"github.com/majewsky/schwift/gopherschwift"
	"github.com/sapcc/go-api-declarations/bininfo"
	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/go-bits/secrets"
)

// SwiftLocation contains all parameters required to establish a Swift connection.
// It implements the Source interface, but is also used on the target side.
type SwiftLocation struct {
	AuthURL                     secrets.FromEnv `yaml:"auth_url"`
	UserName                    secrets.FromEnv `yaml:"user_name"`
	UserDomainName              secrets.FromEnv `yaml:"user_domain_name"`
	ProjectName                 secrets.FromEnv `yaml:"project_name"`
	ProjectDomainName           secrets.FromEnv `yaml:"project_domain_name"`
	Password                    secrets.FromEnv `yaml:"password"`
	ApplicationCredentialID     secrets.FromEnv `yaml:"application_credential_id"`
	ApplicationCredentialName   secrets.FromEnv `yaml:"application_credential_name"`
	ApplicationCredentialSecret secrets.FromEnv `yaml:"application_credential_secret"`
	TLSClientCertificateFile    secrets.FromEnv `yaml:"tls_client_certificate_file"`
	TLSClientKeyFile            secrets.FromEnv `yaml:"tls_client_key_file"`
	RegionName                  secrets.FromEnv `yaml:"region_name"`
	ContainerName               secrets.FromEnv `yaml:"container"`
	ObjectNamePrefix            secrets.FromEnv `yaml:"object_prefix"`
	//configuration for Validate()
	ValidateIgnoreEmptyContainer bool `yaml:"-"`
	//Account and Container is filled by Connect(). Container will be nil if ContainerName is empty.
	Account   *schwift.Account   `yaml:"-"`
	Container *schwift.Container `yaml:"-"`
	//FileExists is filled by DiscoverExistingFiles(). The keys are object names
	//including the ObjectNamePrefix, if any.
	FileExists map[string]bool `yaml:"-"`
}

func (s SwiftLocation) cacheKey(name string) string {
	v := []string{
		string(s.AuthURL),
		string(s.UserName),
		string(s.UserDomainName),
		string(s.ProjectName),
		string(s.ProjectDomainName),
		string(s.Password),
		string(s.ApplicationCredentialID),
		string(s.ApplicationCredentialName),
		string(s.ApplicationCredentialSecret),
		string(s.RegionName),
	}
	if logg.ShowDebug {
		v = append(v, name)
	}
	return strings.Join(v, "\000")
}

// Validate returns an empty list only if all required credentials are present.
func (s *SwiftLocation) Validate(name string) []error {
	var result []error

	if s.AuthURL == "" {
		result = append(result, fmt.Errorf("missing value for %s.auth_url", name))
	}

	if s.TLSClientCertificateFile != "" || s.TLSClientKeyFile != "" {
		if s.TLSClientCertificateFile == "" {
			result = append(result, fmt.Errorf("missing value for %s.tls_client_certificate_file", name))
		}
		if s.TLSClientKeyFile == "" {
			result = append(result, fmt.Errorf("missing value for %s.tls_client_key_file", name))
		}
	}

	if s.ApplicationCredentialID != "" || s.ApplicationCredentialName != "" {
		//checking application credential requirements
		if s.ApplicationCredentialID == "" {
			//if application_credential_id is not set, then we need to know user_name and user_domain_name
			if s.UserName == "" {
				result = append(result, fmt.Errorf("missing value for %s.user_name", name))
			}
			if s.UserDomainName == "" {
				result = append(result, fmt.Errorf("missing value for %s.user_domain_name", name))
			}
		}
		if s.ApplicationCredentialSecret == "" {
			result = append(result, fmt.Errorf("missing value for %s.application_credential_secret", name))
		}
	} else {
		if s.UserName == "" {
			result = append(result, fmt.Errorf("missing value for %s.user_name", name))
		}
		if s.UserDomainName == "" {
			result = append(result, fmt.Errorf("missing value for %s.user_domain_name", name))
		}
		if s.ProjectName == "" {
			result = append(result, fmt.Errorf("missing value for %s.project_name", name))
		}
		if s.ProjectDomainName == "" {
			result = append(result, fmt.Errorf("missing value for %s.project_domain_name", name))
		}
		if s.Password == "" {
			result = append(result, fmt.Errorf("missing value for %s.password", name))
		}
	}

	if !s.ValidateIgnoreEmptyContainer && s.ContainerName == "" {
		result = append(result, fmt.Errorf("missing value for %s.container", name))
	}

	if s.ObjectNamePrefix != "" && !strings.HasSuffix(string(s.ObjectNamePrefix), "/") {
		s.ObjectNamePrefix += "/"
	}

	return result
}

var accountCache = map[string]*schwift.Account{}

type logger struct {
	Prefix string
}

func (l logger) Printf(format string, args ...interface{}) {
	for _, v := range strings.Split(fmt.Sprintf(format, args...), "\n") {
		logg.Debug("[%s] %s", l.Prefix, v)
	}
}

// Connect implements the Source interface. It establishes the connection to Swift.
func (s *SwiftLocation) Connect(name string) error {
	if s.Account != nil {
		return nil
	}

	//connect to Swift account (but re-use connection if cached)
	key := s.cacheKey(name)
	s.Account = accountCache[key]
	if s.Account == nil {
		authOptions := gophercloud.AuthOptions{
			IdentityEndpoint:            string(s.AuthURL),
			Username:                    string(s.UserName),
			DomainName:                  string(s.UserDomainName),
			Password:                    string(s.Password),
			ApplicationCredentialID:     string(s.ApplicationCredentialID),
			ApplicationCredentialName:   string(s.ApplicationCredentialName),
			ApplicationCredentialSecret: string(s.ApplicationCredentialSecret),
			Scope: &gophercloud.AuthScope{
				ProjectName: string(s.ProjectName),
				DomainName:  string(s.ProjectDomainName),
			},
			AllowReauth: true,
		}

		provider, err := openstack.NewClient(authOptions.IdentityEndpoint)
		if err != nil {
			return fmt.Errorf("cannot create OpenStack client: %s", err.Error())
		}

		transport := &http.Transport{}
		if s.TLSClientCertificateFile != "" && s.TLSClientKeyFile != "" {
			cert, err := tls.LoadX509KeyPair(string(s.TLSClientCertificateFile), string(s.TLSClientKeyFile))
			if err != nil {
				return fmt.Errorf("failed to load x509 key pair: %s", err.Error())
			}
			transport.TLSClientConfig = &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12,
			}
			provider.HTTPClient.Transport = transport
		}

		if logg.ShowDebug {
			provider.HTTPClient.Transport = &client.RoundTripper{
				Rt:     transport,
				Logger: &logger{Prefix: name},
			}
		}

		err = openstack.Authenticate(provider, authOptions)
		if err != nil {
			if authOptions.ApplicationCredentialSecret != "" {
				return fmt.Errorf("cannot authenticate to %s using application credential: %s",
					s.AuthURL,
					err.Error(),
				)
			}
			return fmt.Errorf("cannot authenticate to %s in %s@%s as %s@%s: %s",
				s.AuthURL,
				s.ProjectName,
				s.ProjectDomainName,
				s.UserName,
				s.UserDomainName,
				err.Error(),
			)
		}

		serviceClient, err := openstack.NewObjectStorageV1(provider, gophercloud.EndpointOpts{
			Region: string(s.RegionName),
		})
		if err != nil {
			return fmt.Errorf("cannot create Swift client: %s", err.Error())
		}
		s.Account, err = gopherschwift.Wrap(serviceClient, &gopherschwift.Options{
			UserAgent: "swift-http-import/" + bininfo.VersionOr("dev"),
		})
		if err != nil {
			return fmt.Errorf("cannot wrap Swift client: %s", err.Error())
		}

		accountCache[key] = s.Account
	}

	//create target container if missing
	if s.ContainerName == "" {
		s.Container = nil
		return nil
	}
	var err error
	s.Container, err = s.Account.Container(string(s.ContainerName)).EnsureExists()
	return err
}

// ObjectAtPath returns an Object instance for the object at the given path
// (below the ObjectNamePrefix, if any) in this container.
func (s *SwiftLocation) ObjectAtPath(path string) *schwift.Object {
	objectName := strings.TrimPrefix(path, "/")
	return s.Container.Object(string(s.ObjectNamePrefix) + objectName)
}

// ListAllFiles implements the Source interface.
func (s *SwiftLocation) ListAllFiles(out chan<- FileSpec) *ListEntriesError {
	objectPath := string(s.ObjectNamePrefix)
	if objectPath != "" && !strings.HasSuffix(objectPath, "/") {
		objectPath += "/"
	}
	logg.Debug("listing objects at %s/%s recursively", s.ContainerName, objectPath)

	iter := s.Container.Objects()
	iter.Prefix = objectPath
	err := iter.ForeachDetailed(func(info schwift.ObjectInfo) error {
		out <- s.getFileSpec(info)
		return nil
	})
	if err != nil {
		return &ListEntriesError{
			Location: string(s.ContainerName) + "/" + objectPath,
			Message:  "GET failed",
			Inner:    err,
		}
	}

	return nil
}

// ListEntries implements the Source interface.
func (s *SwiftLocation) ListEntries(directoryPath string) ([]FileSpec, *ListEntriesError) {
	return nil, ErrListEntriesNotSupported
}

func (s *SwiftLocation) getFileSpec(info schwift.ObjectInfo) FileSpec {
	var f FileSpec
	//strip ObjectNamePrefix from the resulting objects
	if info.SubDirectory != "" {
		f.Path = strings.TrimPrefix(info.SubDirectory, string(s.ObjectNamePrefix))
		f.IsDirectory = true
	} else {
		f.Path = strings.TrimPrefix(info.Object.Name(), string(s.ObjectNamePrefix))
		lm := info.LastModified
		f.LastModified = &lm

		if info.SymlinkTarget != nil && info.SymlinkTarget.Container().IsEqualTo(s.Container) {
			targetPath := info.SymlinkTarget.Name()
			if strings.HasPrefix(targetPath, string(s.ObjectNamePrefix)) {
				f.SymlinkTargetPath = strings.TrimPrefix(targetPath, string(s.ObjectNamePrefix))
			}
		}
	}
	return f
}

// GetFile implements the Source interface.
func (s *SwiftLocation) GetFile(path string, requestHeaders schwift.ObjectHeaders) (io.ReadCloser, FileState, error) {
	object := s.ObjectAtPath(path)

	body, err := object.Download(requestHeaders.ToOpts()).AsReadCloser()
	if schwift.Is(err, http.StatusNotModified) {
		return nil, FileState{SkipTransfer: true}, nil
	}
	if err != nil {
		return nil, FileState{}, err
	}
	//NOTE: Download() uses a GET request, so object metadata has already been
	//received and cached, so Headers() is cheap now and will never fail.
	hdr, err := object.Headers()
	if err != nil {
		body.Close()
		return nil, FileState{}, err
	}

	var expiryTime *time.Time
	if hdr.ExpiresAt().Exists() {
		t := hdr.ExpiresAt().Get()
		expiryTime = &t
	}

	return body, FileState{
		Etag:         hdr.Etag().Get(),
		LastModified: hdr.Get("Last-Modified"),
		SizeBytes:    int64(hdr.SizeBytes().Get()),
		ExpiryTime:   expiryTime,
		ContentType:  hdr.ContentType().Get(),
	}, nil
}

// DiscoverExistingFiles finds all objects that currently exist in this location
// (i.e. in this Swift container below the given object name prefix) and fills
// s.FileExists accordingly.
//
// The given Matcher is used to find out which files are to be considered as
// belonging to the transfer job in question.
func (s *SwiftLocation) DiscoverExistingFiles(matcher Matcher) error {
	prefix := string(s.ObjectNamePrefix)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	if s.Container == nil {
		return fmt.Errorf(
			"could not list objects in Swift at %s/%s: not connected to Swift",
			s.ContainerName, prefix,
		)
	}

	iter := s.Container.Objects()
	iter.Prefix = prefix
	s.FileExists = make(map[string]bool)
	err := iter.Foreach(func(object *schwift.Object) error {
		s.FileExists[object.Name()] = true
		return nil
	})
	if err != nil {
		return fmt.Errorf(
			"could not list objects in Swift at %s/%s: %s",
			s.ContainerName, prefix, err.Error(),
		)
	}

	return nil
}
