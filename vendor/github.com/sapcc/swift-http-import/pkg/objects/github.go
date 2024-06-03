/*******************************************************************************
*
* Copyright 2022 SAP SE
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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/google/go-github/v62/github"
	"github.com/majewsky/schwift"
	"github.com/sapcc/go-api-declarations/bininfo"
	"github.com/sapcc/go-bits/secrets"
	"golang.org/x/oauth2"
)

type GithubReleaseSource struct {
	// Options from config file.
	URLString         string          `yaml:"url"`
	Token             secrets.FromEnv `yaml:"token"`
	TagNamePattern    string          `yaml:"tag_name_pattern"`
	IncludeDraft      bool            `yaml:"include_draft"`
	IncludePrerelease bool            `yaml:"include_prerelease"`

	// Compiled configuration.
	url       *url.URL       `yaml:"-"`
	client    *github.Client `yaml:"-"`
	owner     string         `yaml:"-"` // repository owner
	repo      string         `yaml:"-"`
	tagNameRx *regexp.Regexp `yaml:"-"`
	// notOlderThan is used to limit release listing to prevent excess API requests.
	notOlderThan *time.Time `yaml:"-"`
}

// githubRepoRx is used to extract repository owner and name from a url.URL.Path field.
//
// Example:
//
//	Input: /sapcc/swift-http-import
//	Match groups: ["sapcc", "swift-http-import"]
var githubRepoRx = regexp.MustCompile(`^/([^\s/]+)/([^\s/]+)/?$`)

// Validate implements the Source interface.
func (s *GithubReleaseSource) Validate(name string) []error {
	var err error
	s.url, err = url.Parse(s.URLString)
	if err != nil {
		return []error{fmt.Errorf("could not parse %s.url: %s", name, err.Error())}
	}

	// Validate URL.
	errInvalidURL := fmt.Errorf("invalid value for %s.url: expected a url in the format %q, got: %q",
		name, "http(s)://<hostname>/<owner>/<repo>", s.URLString)
	if s.url.Scheme != "http" && s.url.Scheme != "https" {
		return []error{errInvalidURL}
	}
	if s.url.RawQuery != "" || s.url.Fragment != "" {
		return []error{errInvalidURL}
	}
	mL := githubRepoRx.FindStringSubmatch(s.url.Path)
	if mL == nil {
		return []error{errInvalidURL}
	}
	s.owner = mL[1]
	s.repo = mL[2]

	if s.url.Hostname() != "github.com" {
		if s.Token == "" {
			return []error{fmt.Errorf("%s.token is required for repositories hosted on GitHub Enterprise", name)}
		}
	}

	if s.TagNamePattern != "" {
		s.tagNameRx, err = regexp.Compile(s.TagNamePattern)
		if err != nil {
			return []error{fmt.Errorf("could not parse %s.tag_name_pattern: %s", name, err.Error())}
		}
	}

	return nil
}

// Connect implements the Source interface.
func (s *GithubReleaseSource) Connect(name string) error {
	c := http.DefaultClient
	if s.Token != "" {
		src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: string(s.Token)})
		c = oauth2.NewClient(context.Background(), src)
	}
	if s.url.Hostname() != "github.com" {
		// baseURL is s.url without the Path (/<owner>/<repo>).
		baseURL := *s.url
		baseURL.Path = ""
		baseURL.RawPath = ""
		baseURLStr := baseURL.String()
		var err error
		s.client, err = github.NewClient(c).WithEnterpriseURLs(baseURLStr, baseURLStr)
		if err != nil {
			return err
		}
	} else {
		s.client = github.NewClient(c)
	}
	return nil
}

// ListEntries implements the Source interface.
func (s *GithubReleaseSource) ListEntries(_ context.Context, directoryPath string) ([]FileSpec, *ListEntriesError) {
	return nil, ErrListEntriesNotSupported
}

// ListAllFiles implements the Source interface.
func (s *GithubReleaseSource) ListAllFiles(_ context.Context, out chan<- FileSpec) *ListEntriesError {
	releases, err := s.getReleases()
	if err != nil {
		return &ListEntriesError{
			Location: s.url.String(),
			Message:  "could not list releases",
			Inner:    err,
		}
	}

	for _, r := range releases {
		if !s.IncludeDraft && r.GetDraft() {
			continue
		}
		if !s.IncludePrerelease && r.GetPrerelease() {
			continue
		}
		tagName := r.GetTagName()
		if s.TagNamePattern != "" && !s.tagNameRx.MatchString(tagName) {
			continue
		}

		for _, a := range r.Assets {
			fs := FileSpec{
				Path: fmt.Sprintf("%s/%s", tagName, a.GetName()),
				// Technically, DownloadPath is supposed to be a file path but since we
				// only need asset ID to download the asset in GetFile() therefore we can
				// simply use the asset ID here instead.
				DownloadPath: strconv.FormatInt(a.GetID(), 10),
			}
			if a.UpdatedAt != nil {
				updatedAtTime := a.UpdatedAt.Time
				fs.LastModified = &updatedAtTime
			}
			out <- fs
		}
	}

	return nil
}

// GetFile implements the Source interface.
func (s *GithubReleaseSource) GetFile(_ context.Context, path string, requestHeaders schwift.ObjectHeaders) (io.ReadCloser, FileState, error) {
	u := fmt.Sprintf("repos/%s/%s/releases/assets/%s", s.owner, s.repo, path)
	req, err := s.client.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, FileState{}, fmt.Errorf("skipping: could not create URL for %s: %s", u, err.Error())
	}

	for key, val := range requestHeaders.Headers {
		req.Header.Set(key, val)
	}
	req.Header.Set("User-Agent", "swift-http-import/"+bininfo.VersionOr("dev"))
	req.Header.Set("Accept", "application/octet-stream")
	if s.Token != "" {
		req.Header.Set("Authorization", fmt.Sprintf("token %s", s.Token))
	}

	// We use http.DefaultClient explicitly instead of retrieving (s.client.Client()) the
	// same http.Client that was passed to github.Client because that http.Client, when
	// obtained using oauth2.NewClient(), does not return all headers in the request
	// response.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, FileState{}, fmt.Errorf("skipping %s: GET failed: %s", req.URL.String(), err.Error())
	}

	if resp.StatusCode != http.StatusOK &&
		resp.StatusCode != http.StatusNotModified {
		return nil, FileState{}, fmt.Errorf(
			"skipping %s: GET returned unexpected status code: expected 200 or 304, but got %d",
			req.URL.String(), resp.StatusCode,
		)
	}

	return resp.Body, FileState{
		Etag:         resp.Header.Get("Etag"),
		LastModified: resp.Header.Get("Last-Modified"),
		SizeBytes:    resp.ContentLength,
		ExpiryTime:   nil, // no way to get this information via HTTP only
		SkipTransfer: resp.StatusCode == http.StatusNotModified,
		ContentType:  resp.Header.Get("Content-Type"),
	}, nil
}

func (s *GithubReleaseSource) getReleases() ([]*github.RepositoryRelease, error) {
	var result []*github.RepositoryRelease

	// Set higher value than default (30) for results per page to avoid exceeding API rate limit.
	listOpts := &github.ListOptions{PerPage: 50}
	resp := &github.Response{NextPage: 1}
	for resp.NextPage != 0 {
		var (
			rL  []*github.RepositoryRelease
			err error
		)
		listOpts.Page = resp.NextPage
		rL, resp, err = s.client.Repositories.ListReleases(context.Background(), s.owner, s.repo, listOpts)
		if err != nil {
			return nil, err
		}
		result = append(result, rL...)

		// Check if the last release in the result slice is newer than the notOlderThan
		// time. If not, then we don't need to get further releases.
		if s.notOlderThan != nil {
			lastRelease := result[len(result)-1]
			if s.notOlderThan.After(lastRelease.PublishedAt.Time) {
				break
			}
		}
	}

	return result, nil
}
