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
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/utils/client"
	"github.com/majewsky/schwift"
	"github.com/majewsky/schwift/gopherschwift"
	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/swift-http-import/pkg/util"
)

//SwiftLocation contains all parameters required to establish a Swift connection.
//It implements the Source interface, but is also used on the target side.
type SwiftLocation struct {
	AuthURL                     string       `yaml:"auth_url"`
	UserName                    string       `yaml:"user_name"`
	UserDomainName              string       `yaml:"user_domain_name"`
	ProjectName                 string       `yaml:"project_name"`
	ProjectDomainName           string       `yaml:"project_domain_name"`
	Password                    AuthPassword `yaml:"password"`
	ApplicationCredentialID     string       `yaml:"application_credential_id"`
	ApplicationCredentialName   string       `yaml:"application_credential_name"`
	ApplicationCredentialSecret AuthPassword `yaml:"application_credential_secret"`
	RegionName                  string       `yaml:"region_name"`
	ContainerName               string       `yaml:"container"`
	ObjectNamePrefix            string       `yaml:"object_prefix"`
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
		s.AuthURL,
		s.UserName,
		s.UserDomainName,
		s.ProjectName,
		s.ProjectDomainName,
		string(s.Password),
		s.ApplicationCredentialID,
		s.ApplicationCredentialName,
		string(s.ApplicationCredentialSecret),
		s.RegionName,
	}
	if logg.ShowDebug {
		v = append(v, name)
	}
	return strings.Join(v, "\000")
}

//Validate returns an empty list only if all required credentials are present.
func (s SwiftLocation) Validate(name string) []error {
	var result []error

	if s.AuthURL == "" {
		result = append(result, fmt.Errorf("missing value for %s.auth_url", name))
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
		if string(s.ApplicationCredentialSecret) == "" {
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

	if s.ObjectNamePrefix != "" && !strings.HasPrefix(s.ObjectNamePrefix, "/") {
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

//Connect implements the Source interface. It establishes the connection to Swift.
func (s *SwiftLocation) Connect(name string) error {
	if s.Account != nil {
		return nil
	}

	//connect to Swift account (but re-use connection if cached)
	key := s.cacheKey(name)
	s.Account = accountCache[key]
	if s.Account == nil {
		authOptions := gophercloud.AuthOptions{
			IdentityEndpoint:            s.AuthURL,
			Username:                    s.UserName,
			DomainName:                  s.UserDomainName,
			Password:                    string(s.Password),
			ApplicationCredentialID:     s.ApplicationCredentialID,
			ApplicationCredentialName:   s.ApplicationCredentialName,
			ApplicationCredentialSecret: string(s.ApplicationCredentialSecret),
			Scope: &gophercloud.AuthScope{
				ProjectName: s.ProjectName,
				DomainName:  s.ProjectDomainName,
			},
			AllowReauth: true,
		}

		provider, err := openstack.NewClient(authOptions.IdentityEndpoint)
		if err != nil {
			return fmt.Errorf("cannot create OpenStack client: %s", err.Error())
		}

		//use DefaultClient, esp. to pick up correct behavior with HTTP proxies
		provider.HTTPClient = *http.DefaultClient
		if logg.ShowDebug {
			transport := http.DefaultClient.Transport
			if transport == nil {
				transport = http.DefaultTransport
			}
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

		client, err := openstack.NewObjectStorageV1(provider, gophercloud.EndpointOpts{
			Region: s.RegionName,
		})
		if err != nil {
			return fmt.Errorf("cannot create Swift client: %s", err.Error())
		}
		s.Account, err = gopherschwift.Wrap(client, &gopherschwift.Options{
			UserAgent: "swift-http-import/" + util.Version,
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
	s.Container, err = s.Account.Container(s.ContainerName).EnsureExists()
	return err
}

//ObjectAtPath returns an Object instance for the object at the given path
//(below the ObjectNamePrefix, if any) in this container.
func (s *SwiftLocation) ObjectAtPath(path string) *schwift.Object {
	objectName := strings.TrimPrefix(path, "/")
	if s.ObjectNamePrefix != "" {
		var isPseudoDir bool
		if objectName == "" {
			//this means that the object refers to a pseudo-directory
			//with the same name as the specified ObjectNamePrefix
			isPseudoDir = true
		} else {
			isPseudoDir = strings.HasSuffix(objectName, "/")
		}
		objectName = filepath.Join(s.ObjectNamePrefix, objectName)
		if isPseudoDir {
			objectName += "/"
		}
	}
	return s.Container.Object(objectName)
}

//ListAllFiles implements the Source interface.
func (s *SwiftLocation) ListAllFiles() ([]FileSpec, *ListEntriesError) {
	return s.listFiles("", true)
}

//ListEntries implements the Source interface.
func (s *SwiftLocation) ListEntries(path string) ([]FileSpec, *ListEntriesError) {
	return s.listFiles(path, false)
}

func (s *SwiftLocation) listFiles(path string, recursively bool) ([]FileSpec, *ListEntriesError) {
	objectPath := filepath.Join(s.ObjectNamePrefix, strings.TrimPrefix(path, "/"))
	if objectPath != "" && !strings.HasSuffix(objectPath, "/") {
		objectPath += "/"
	}

	if recursively {
		logg.Debug("listing objects at %s/%s recursively", s.ContainerName, objectPath)
	} else {
		logg.Debug("listing objects at %s/%s", s.ContainerName, objectPath)
	}

	iter := s.Container.Objects()
	iter.Prefix = objectPath
	if !recursively {
		iter.Delimiter = "/"
	}
	objectInfos, err := iter.CollectDetailed()
	if err != nil {
		return nil, &ListEntriesError{
			Location: s.ContainerName + "/" + objectPath,
			Message:  "GET failed",
			Inner:    err,
		}
	}

	//strip ObjectNamePrefix from the resulting objects
	result := make([]FileSpec, len(objectInfos))
	for idx, info := range objectInfos {
		if info.SubDirectory != "" {
			result[idx].Path = strings.TrimPrefix(info.SubDirectory, s.ObjectNamePrefix)
			result[idx].IsDirectory = true
		} else {
			result[idx].Path = strings.TrimPrefix(info.Object.Name(), s.ObjectNamePrefix)
			lm := info.LastModified
			result[idx].LastModified = &lm

			if info.SymlinkTarget != nil && info.SymlinkTarget.Container().IsEqualTo(s.Container) {
				targetPath := info.SymlinkTarget.Name()
				if strings.HasPrefix(targetPath, s.ObjectNamePrefix) {
					result[idx].SymlinkTargetPath = strings.TrimPrefix(targetPath, s.ObjectNamePrefix)
				}
			}
		}
	}
	return result, nil
}

//GetFile implements the Source interface.
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

//DiscoverExistingFiles finds all objects that currently exist in this location
//(i.e. in this Swift container below the given object name prefix) and fills
//s.FileExists accordingly.
//
//The given Matcher is used to find out which files are to be considered as
//belonging to the transfer job in question.
func (s *SwiftLocation) DiscoverExistingFiles(matcher Matcher) error {
	prefix := s.ObjectNamePrefix
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
