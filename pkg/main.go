package pkg

import (
	"fmt"
	"io"
	llog "log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/availabilityzones"
	"github.com/gophercloud/utils/client"
	"github.com/gophercloud/utils/openstack/clientconfig"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type logger struct {
	Prefix string
}

// RootCmd represents the base command when called without any subcommands
var RootCmd = &cobra.Command{
	Use:          "cyclone",
	Short:        "Clone OpenStack entities easily",
	SilenceUsage: true,
}

var (
	start   time.Time = time.Now()
	logFile *os.File
	l       *llog.Logger
	log     = llog.New(llog.Writer(), llog.Prefix(), llog.Flags())
)

func (lg logger) Printf(format string, args ...interface{}) {
	for _, v := range strings.Split(fmt.Sprintf(format, args...), "\n") {
		l.Printf("[%s] %s", lg.Prefix, v)
	}
}

func measureTime() {
	log.Printf("Total execution time: %s", time.Now().Sub(start))
}

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	initRootCmdFlags()
	if err := RootCmd.Execute(); err != nil {
		fmt.Fprintf(logFile, "Error: %s", err)
		os.Exit(1)
	}
}

func initLogger() {
	if l == nil {
		dir := filepath.Join(os.TempDir(), "cyclone")
		err := os.MkdirAll(dir, os.ModePerm)
		if err != nil {
			log.Fatal(err)
		}
		logFile, err = os.Create(filepath.Join(dir, time.Now().Format("20060102150405")+".log"))
		if err != nil {
			log.Fatal(err)
		}
		// no need to close the log: https://golang.org/pkg/runtime/#SetFinalizer
		l = llog.New(logFile, log.Prefix(), log.Flags())

		if viper.GetBool("debug") {
			// write log into stderr and log file
			l.SetOutput(io.MultiWriter(log.Writer(), l.Writer()))
		}

		// write stderr logs into the log file
		log.SetOutput(io.MultiWriter(log.Writer(), logFile))
	}
}

func initRootCmdFlags() {
	// debug flag
	RootCmd.PersistentFlags().BoolP("debug", "d", false, "print out request and response objects")
	RootCmd.PersistentFlags().StringP("to-auth-url", "", "", "destination auth URL (if not provided, detected automatically from the source auth URL and destination region)")
	RootCmd.PersistentFlags().StringP("to-region", "", "", "destination region")
	RootCmd.PersistentFlags().StringP("to-domain", "", "", "destination domain")
	RootCmd.PersistentFlags().StringP("to-project", "", "", "destination project")
	viper.BindPFlag("debug", RootCmd.PersistentFlags().Lookup("debug"))
	viper.BindPFlag("to-auth-url", RootCmd.PersistentFlags().Lookup("to-auth-url"))
	viper.BindPFlag("to-region", RootCmd.PersistentFlags().Lookup("to-region"))
	viper.BindPFlag("to-domain", RootCmd.PersistentFlags().Lookup("to-domain"))
	viper.BindPFlag("to-project", RootCmd.PersistentFlags().Lookup("to-project"))
}

type Locations struct {
	Src         Location
	Dst         Location
	SameRegion  bool
	SameAZ      bool
	SameProject bool
}

type Location struct {
	AuthURL string
	Region  string
	Domain  string
	Project string
	Origin  string
}

func getSrcAndDst(az string) (Locations, error) {
	initLogger()

	var loc Locations

	// source and destination parameters
	loc.Src.Origin = "src"
	loc.Src.Region = os.Getenv("OS_REGION_NAME")
	loc.Src.AuthURL = os.Getenv("OS_AUTH_URL")
	loc.Src.Domain = os.Getenv("OS_PROJECT_DOMAIN_NAME")
	if loc.Src.Domain == "" {
		loc.Src.Domain = os.Getenv("OS_USER_DOMAIN_NAME")
	}
	loc.Src.Project = os.Getenv("OS_PROJECT_NAME")

	loc.Dst.Origin = "dst"
	loc.Dst.Region = viper.GetString("to-region")
	loc.Dst.AuthURL = viper.GetString("to-auth-url")
	loc.Dst.Domain = viper.GetString("to-domain")
	loc.Dst.Project = viper.GetString("to-project")

	if loc.Dst.Project == "" {
		loc.Dst.Project = loc.Src.Project
	}

	if loc.Dst.Region == "" {
		loc.Dst.Region = loc.Src.Region
	}

	if loc.Dst.Domain == "" {
		loc.Dst.Domain = loc.Src.Domain
	}

	if loc.Dst.AuthURL == "" {
		// try to transform a source auth URL to a destination source URL
		s := strings.Replace(loc.Src.AuthURL, loc.Src.Region, loc.Dst.Region, 1)
		if strings.Contains(s, loc.Dst.Region) {
			loc.Dst.AuthURL = s
			log.Printf("Detected %q destination auth URL", loc.Dst.AuthURL)
		} else {
			return loc, fmt.Errorf("Failed to detect destination auth URL")
		}
	}

	loc.SameRegion = false
	if loc.Src.Region == loc.Dst.Region {
		if loc.Src.Domain == loc.Dst.Domain {
			if loc.Src.Project == loc.Dst.Project {
				loc.SameProject = true
			}
		}
		loc.SameRegion = true
	}

	return loc, nil
}

func NewOpenStackClient(loc Location) (*gophercloud.ProviderClient, error) {
	ao, err := clientconfig.AuthOptions(&clientconfig.ClientOpts{
		AuthInfo: &clientconfig.AuthInfo{
			AuthURL:     loc.AuthURL,
			DomainName:  loc.Domain,
			ProjectName: loc.Project,
		},
		RegionName: loc.Region,
	})
	if err != nil {
		return nil, err
	}

	// Could be long-running, therefore we need to be able to renew a token
	ao.AllowReauth = true

	/* TODO: Introduce auth by CLI parameters
	   ao := gophercloud.AuthOptions{
	           IdentityEndpoint:            authURL,
	           UserID:                      userID,
	           Username:                    username,
	           Password:                    password,
	           TenantID:                    tenantID,
	           TenantName:                  tenantName,
	           DomainID:                    domainID,
	           DomainName:                  domainName,
	           ApplicationCredentialID:     applicationCredentialID,
	           ApplicationCredentialName:   applicationCredentialName,
	           ApplicationCredentialSecret: applicationCredentialSecret,
	   }
	*/

	provider, err := openstack.NewClient(ao.IdentityEndpoint)
	if err != nil {
		return nil, err
	}

	// debug logger is enabled by default and writes logs into a cyclone temp dir
	provider.HTTPClient = http.Client{
		Transport: &client.RoundTripper{
			Rt:     &http.Transport{},
			Logger: &logger{Prefix: loc.Origin},
		},
	}

	err = openstack.Authenticate(provider, *ao)
	if err != nil {
		return nil, err
	}

	return provider, nil
}

func NewGlanceV2Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewImageServiceV2(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func NewBlockStorageV3Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewBlockStorageV3(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func NewObjectStorageV1Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewObjectStorageV1(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func NewComputeV2Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func NewNetworkV2Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewNetworkV2(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func checkAvailabilityZone(client *gophercloud.ServiceClient, srcAZ string, dstAZ *string, loc *Locations) error {
	if *dstAZ == "" {
		if strings.HasPrefix(srcAZ, loc.Dst.Region) {
			*dstAZ = srcAZ
			loc.SameAZ = true
			return nil
		}
		// use as a default
		return nil
	}

	// check availability zone name
	allPages, err := availabilityzones.List(client).AllPages()
	if err != nil {
		return fmt.Errorf("error retrieving availability zones: %s", err)
	}
	zones, err := availabilityzones.ExtractAvailabilityZones(allPages)
	if err != nil {
		return fmt.Errorf("error extracting availability zones from response: %s", err)
	}

	var zonesNames []string
	var found bool
	for _, z := range zones {
		if z.ZoneState.Available == true {
			zonesNames = append(zonesNames, z.ZoneName)
		}
		if z.ZoneName == *dstAZ {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("failed to find %q availability zone, supported availability zones: %q", *dstAZ, zonesNames)
	}

	if srcAZ == *dstAZ {
		loc.SameAZ = true
	}

	return nil
}

// isSliceContainsStr returns true if the string exists in given slice
func isSliceContainsStr(sl []string, str string) bool {
	for _, s := range sl {
		if s == str {
			return true
		}
	}
	return false
}
