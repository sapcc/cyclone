/*******************************************************************************
*
* Copyright 2016-2017 SAP SE
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
	"io/ioutil"
	"regexp"
	"time"

	"github.com/majewsky/schwift"
	"github.com/sapcc/swift-http-import/pkg/util"
	"golang.org/x/crypto/openpgp"
	yaml "gopkg.in/yaml.v2"
)

//Configuration contains the contents of the configuration file.
type Configuration struct {
	Swift        SwiftLocation `yaml:"swift"`
	WorkerCounts struct {
		Transfer uint
	} `yaml:"workers"`
	Statsd     StatsdConfiguration `yaml:"statsd"`
	JobConfigs []JobConfiguration  `yaml:"jobs"`
	Jobs       []*Job              `yaml:"-"`
}

//ReadConfiguration reads the configuration file.
func ReadConfiguration(path string) (*Configuration, []error) {
	configBytes, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, []error{err}
	}

	var cfg Configuration
	err = yaml.Unmarshal(configBytes, &cfg)
	if err != nil {
		return nil, []error{err}
	}

	//set default values
	if cfg.WorkerCounts.Transfer == 0 {
		cfg.WorkerCounts.Transfer = 1
	}
	if cfg.Statsd.HostName != "" && cfg.Statsd.Port == 0 {
		cfg.Statsd.Port = 8125
	}
	if cfg.Statsd.Prefix == "" {
		cfg.Statsd.Prefix = "swift_http_import"
	}

	//gpgKeyRing is used to cache GPG public keys that are used by custom source
	//types (e.g. YumSource), and is passed on to the different Job(s)
	gpgKeyRing := &util.GPGKeyRing{EntityList: make(openpgp.EntityList, 0)}

	cfg.Swift.ValidateIgnoreEmptyContainer = true
	errors := cfg.Swift.Validate("swift")
	for idx, jobConfig := range cfg.JobConfigs {
		jobConfig.gpgKeyRing = gpgKeyRing
		job, jobErrors := jobConfig.Compile(
			fmt.Sprintf("swift.jobs[%d]", idx),
			cfg.Swift,
		)
		cfg.Jobs = append(cfg.Jobs, job)
		errors = append(errors, jobErrors...)
	}

	return &cfg, errors
}

//StatsdConfiguration contains the configuration options relating to StatsD
//metric emission.
type StatsdConfiguration struct {
	HostName string `yaml:"hostname"`
	Port     int    `yaml:"port"`
	Prefix   string `yaml:"prefix"`
}

//JobConfiguration describes a transfer job in the configuration file.
type JobConfiguration struct {
	//basic options
	Source SourceUnmarshaler `yaml:"from"`
	Target *SwiftLocation    `yaml:"to"`
	//behavior options
	ExcludePattern       string                   `yaml:"except"`
	IncludePattern       string                   `yaml:"only"`
	ImmutableFilePattern string                   `yaml:"immutable"`
	Match                MatchConfiguration       `yaml:"match"`
	Segmenting           *SegmentingConfiguration `yaml:"segmenting"`
	Expiration           ExpirationConfiguration  `yaml:"expiration"`
	Cleanup              CleanupConfiguration     `yaml:"cleanup"`
	//gpgKeyRing is the common key ring cache that is passed on to the
	//custom source type Job(s).
	gpgKeyRing *util.GPGKeyRing
}

//MatchConfiguration contains the "match" section of a JobConfiguration.
type MatchConfiguration struct {
	NotOlderThan         *AgeSpec `yaml:"not_older_than"`
	SimplisticComparison *bool    `yaml:"simplistic_comparison"`
}

//SegmentingConfiguration contains the "segmenting" section of a JobConfiguration.
type SegmentingConfiguration struct {
	MinObjectSize uint64 `yaml:"min_bytes"`
	SegmentSize   uint64 `yaml:"segment_bytes"`
	ContainerName string `yaml:"container"`
	//Container is initialized by JobConfiguration.Compile().
	Container *schwift.Container `yaml:"-"`
}

//ExpirationConfiguration contains the "expiration" section of a JobConfiguration.
type ExpirationConfiguration struct {
	EnabledIn    *bool  `yaml:"enabled"`
	Enabled      bool   `yaml:"-"`
	DelaySeconds uint32 `yaml:"delay_seconds"`
}

//CleanupStrategy is an enum of legal values for the jobs[].cleanup.strategy configuration option.
type CleanupStrategy string

const (
	//KeepUnknownFiles is the default cleanup strategy.
	KeepUnknownFiles CleanupStrategy = ""
	//DeleteUnknownFiles is another strategy.
	DeleteUnknownFiles CleanupStrategy = "delete"
	//ReportUnknownFiles is another strategy.
	ReportUnknownFiles CleanupStrategy = "report"
)

//CleanupConfiguration contains the "cleanup" section of a JobConfiguration.
type CleanupConfiguration struct {
	Strategy CleanupStrategy `yaml:"strategy"`
}

//SourceUnmarshaler provides a yaml.Unmarshaler implementation for the Source interface.
type SourceUnmarshaler struct {
	Source
}

//UnmarshalYAML implements the yaml.Unmarshaler interface.
func (u *SourceUnmarshaler) UnmarshalYAML(unmarshal func(interface{}) error) error {
	//unmarshal a few indicative fields
	var probe struct {
		URL  string `yaml:"url"`
		Type string `yaml:"type"`
	}
	err := unmarshal(&probe)
	if err != nil {
		return err
	}

	//look at keys to determine whether this is a URLSource or a SwiftSource
	if probe.URL == "" {
		u.Source = &SwiftLocation{}
	} else {
		switch probe.Type {
		case "":
			u.Source = &URLSource{}
		case "yum":
			u.Source = &YumSource{}
		case "debian":
			u.Source = &DebianSource{}
		default:
			return fmt.Errorf("unexpected value: type = %q", probe.Type)
		}
	}
	return unmarshal(u.Source)
}

//Job describes a transfer job at runtime.
type Job struct {
	Source     Source
	Target     *SwiftLocation
	Matcher    Matcher
	Segmenting *SegmentingConfiguration
	Expiration ExpirationConfiguration
	Cleanup    CleanupConfiguration
}

//Compile validates the given JobConfiguration, then creates and prepares a Job from it.
func (cfg JobConfiguration) Compile(name string, swift SwiftLocation) (job *Job, errors []error) {
	if cfg.Source.Source == nil {
		errors = append(errors, fmt.Errorf("missing value for %s.from", name))
	} else {
		errors = append(errors, cfg.Source.Source.Validate(name+".from")...)
	}
	if cfg.Target == nil {
		errors = append(errors, fmt.Errorf("missing value for %s.to", name))
	} else {
		//target inherits connection parameters from global Swift credentials
		cfg.Target.AuthURL = swift.AuthURL
		cfg.Target.UserName = swift.UserName
		cfg.Target.UserDomainName = swift.UserDomainName
		cfg.Target.ProjectName = swift.ProjectName
		cfg.Target.ProjectDomainName = swift.ProjectDomainName
		cfg.Target.Password = swift.Password
		cfg.Target.ApplicationCredentialID = swift.ApplicationCredentialID
		cfg.Target.ApplicationCredentialName = swift.ApplicationCredentialName
		cfg.Target.ApplicationCredentialSecret = swift.ApplicationCredentialSecret
		cfg.Target.RegionName = swift.RegionName
		errors = append(errors, cfg.Target.Validate(name+".to")...)
	}

	if cfg.Match.NotOlderThan != nil {
		_, isSwiftSource := cfg.Source.Source.(*SwiftLocation)
		if !isSwiftSource {
			errors = append(errors, fmt.Errorf("invalid value for %s.match.not_older_than: this option is not supported for source type %T", name, cfg.Source.Source))
		}
	}

	if cfg.Match.SimplisticComparison != nil {
		_, isURLSource := cfg.Source.Source.(*URLSource)
		_, isSwiftSource := cfg.Source.Source.(*SwiftLocation)
		if !isURLSource && !isSwiftSource {
			errors = append(errors, fmt.Errorf("invalid value for %s.match.simplistic_comparsion: this option is not supported for source type %T", name, cfg.Source.Source))
		}
	}

	_, isYumSource := cfg.Source.Source.(*YumSource)
	if isYumSource {
		cfg.Source.Source.(*YumSource).gpgKeyRing = cfg.gpgKeyRing
	}
	_, isDebianSource := cfg.Source.Source.(*DebianSource)
	if isDebianSource {
		cfg.Source.Source.(*DebianSource).gpgKeyRing = cfg.gpgKeyRing
	}

	if cfg.Segmenting != nil {
		if cfg.Segmenting.MinObjectSize == 0 {
			errors = append(errors, fmt.Errorf("missing value for %s.segmenting.min_bytes", name))
		}
		if cfg.Segmenting.SegmentSize == 0 {
			errors = append(errors, fmt.Errorf("missing value for %s.segmenting.segment_bytes", name))
		}
		if cfg.Segmenting.ContainerName == "" {
			cfg.Segmenting.ContainerName = cfg.Target.ContainerName + "_segments"
		}
	}

	if cfg.Expiration.EnabledIn == nil {
		cfg.Expiration.Enabled = true
	} else {
		cfg.Expiration.Enabled = *cfg.Expiration.EnabledIn
	}

	ufs := cfg.Cleanup.Strategy
	if ufs != KeepUnknownFiles && ufs != DeleteUnknownFiles && ufs != ReportUnknownFiles {
		errors = append(errors, fmt.Errorf("invalid value for %s.cleanup.strategy: %q", name, ufs))
	}

	job = &Job{
		Source:     cfg.Source.Source,
		Target:     cfg.Target,
		Segmenting: cfg.Segmenting,
		Expiration: cfg.Expiration,
		Cleanup:    cfg.Cleanup,
	}

	//compile patterns into regexes
	compileOptionalRegex := func(key, pattern string) *regexp.Regexp {
		if pattern == "" {
			return nil
		}
		rx, err := regexp.Compile(pattern)
		if err != nil {
			errors = append(errors, fmt.Errorf("malformed regex in %s.%s: %s", name, key, err.Error()))
		}
		return rx
	}
	job.Matcher.ExcludeRx = compileOptionalRegex("except", cfg.ExcludePattern)
	job.Matcher.IncludeRx = compileOptionalRegex("only", cfg.IncludePattern)
	job.Matcher.ImmutableFileRx = compileOptionalRegex("immutable", cfg.ImmutableFilePattern)
	if cfg.Match.NotOlderThan != nil {
		age := time.Duration(*cfg.Match.NotOlderThan)
		cutoff := time.Now().Add(-age)
		job.Matcher.NotOlderThan = &cutoff
	}
	job.Matcher.SimplisticComparison = cfg.Match.SimplisticComparison

	//do not try connecting to Swift if credentials are invalid etc.
	if len(errors) > 0 {
		return
	}

	//ensure that connection to Swift exists and that target container(s) is/are available
	err := job.Source.Connect(name + ".from")
	if err != nil {
		errors = append(errors, err)
	}
	err = job.Target.Connect(name + ".to")
	if err != nil {
		errors = append(errors, err)
	}
	if job.Target.Account != nil && job.Segmenting != nil {
		job.Segmenting.Container, err = job.Target.Account.Container(job.Segmenting.ContainerName).EnsureExists()
		if err != nil {
			errors = append(errors, err)
		}
	}

	err = job.Target.DiscoverExistingFiles(job.Matcher)
	if err != nil {
		errors = append(errors, err)
	}

	return
}
