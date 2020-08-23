package pkg

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

type Locations struct {
	Src         Location
	Dst         Location
	SameRegion  bool
	SameAZ      bool
	SameProject bool
}

type Location struct {
	AuthURL                     string
	Region                      string
	Domain                      string
	Project                     string
	Username                    string
	Password                    string
	ApplicationCredentialName   string
	ApplicationCredentialID     string
	ApplicationCredentialSecret string
	Token                       string
	Origin                      string
}

var (
	// RootCmd represents the base command when called without any subcommands
	RootCmd = &cobra.Command{
		Use:          "cyclone",
		Short:        "Clone OpenStack entities easily",
		SilenceUsage: true,
	}
	cleanupFuncs []func(*sync.WaitGroup)
)

// Execute adds all child commands to the root command sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	initRootCmdFlags()

	cleanupFunc := func() {
		var wg = &sync.WaitGroup{}
		for _, f := range cleanupFuncs {
			wg.Add(1)
			go f(wg)
		}
		wg.Wait()
	}

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		select {
		case <-c:
			log.Printf("Interrupted")
			cleanupFunc()
			os.Exit(1)
		}
	}()

	err := RootCmd.Execute()
	if err != nil {
		log.Printf("Error: %s", err)
	}

	cleanupFunc()

	if err != nil {
		os.Exit(1)
	}
}

func initRootCmdFlags() {
	// debug flag
	RootCmd.PersistentFlags().BoolP("debug", "d", false, "print out request and response objects")
	RootCmd.PersistentFlags().StringP("to-auth-url", "", "", "destination auth URL (if not provided, detected automatically from the source auth URL and destination region)")
	RootCmd.PersistentFlags().StringP("to-region", "", "", "destination region")
	RootCmd.PersistentFlags().StringP("to-domain", "", "", "destination domain")
	RootCmd.PersistentFlags().StringP("to-project", "", "", "destination project")
	RootCmd.PersistentFlags().StringP("to-username", "", "", "destination username")
	RootCmd.PersistentFlags().StringP("to-password", "", "", "destination username password")
	RootCmd.PersistentFlags().StringP("to-application-credential-name", "", "", "destination application credential name")
	RootCmd.PersistentFlags().StringP("to-application-credential-id", "", "", "destination application credential ID")
	RootCmd.PersistentFlags().StringP("to-application-credential-secret", "", "", "destination application credential secret")
	RootCmd.PersistentFlags().StringP("timeout-image", "", "24h", "timeout to wait for an image status")
	RootCmd.PersistentFlags().StringP("timeout-volume", "", "24h", "timeout to wait for a volume status")
	RootCmd.PersistentFlags().StringP("timeout-server", "", "24h", "timeout to wait for a server status")
	RootCmd.PersistentFlags().StringP("timeout-snapshot", "", "24h", "timeout to wait for a snapshot status")
	RootCmd.PersistentFlags().StringP("timeout-backup", "", "24h", "timeout to wait for a backup status")
	RootCmd.PersistentFlags().BoolP("image-web-download", "", true, "use Glance web-download image import method")
	viper.BindPFlag("debug", RootCmd.PersistentFlags().Lookup("debug"))
	viper.BindPFlag("to-auth-url", RootCmd.PersistentFlags().Lookup("to-auth-url"))
	viper.BindPFlag("to-region", RootCmd.PersistentFlags().Lookup("to-region"))
	viper.BindPFlag("to-domain", RootCmd.PersistentFlags().Lookup("to-domain"))
	viper.BindPFlag("to-project", RootCmd.PersistentFlags().Lookup("to-project"))
	viper.BindPFlag("to-username", RootCmd.PersistentFlags().Lookup("to-username"))
	viper.BindPFlag("to-password", RootCmd.PersistentFlags().Lookup("to-password"))
	viper.BindPFlag("to-application-credential-name", RootCmd.PersistentFlags().Lookup("to-application-credential-name"))
	viper.BindPFlag("to-application-credential-id", RootCmd.PersistentFlags().Lookup("to-application-credential-id"))
	viper.BindPFlag("to-application-credential-secret", RootCmd.PersistentFlags().Lookup("to-application-credential-secret"))
	viper.BindPFlag("timeout-image", RootCmd.PersistentFlags().Lookup("timeout-image"))
	viper.BindPFlag("timeout-volume", RootCmd.PersistentFlags().Lookup("timeout-volume"))
	viper.BindPFlag("timeout-server", RootCmd.PersistentFlags().Lookup("timeout-server"))
	viper.BindPFlag("timeout-snapshot", RootCmd.PersistentFlags().Lookup("timeout-snapshot"))
	viper.BindPFlag("timeout-backup", RootCmd.PersistentFlags().Lookup("timeout-backup"))
	viper.BindPFlag("image-web-download", RootCmd.PersistentFlags().Lookup("image-web-download"))
}

func parseTimeoutArg(arg string, dst *float64, errors *[]error) {
	s := viper.GetString(arg)
	v, err := time.ParseDuration(s)
	if err != nil {
		*errors = append(*errors, fmt.Errorf("failed to parse --%s value: %q", arg, s))
		return
	}
	t := int(v.Seconds())
	if t == 0 {
		*errors = append(*errors, fmt.Errorf("--%s value cannot be zero: %d", arg, t))
	}
	if t < 0 {
		*errors = append(*errors, fmt.Errorf("--%s value cannot be negative: %d", arg, t))
	}
	*dst = float64(t)
}

func parseTimeoutArgs() error {
	var errors []error
	parseTimeoutArg("timeout-image", &waitForImageSec, &errors)
	parseTimeoutArg("timeout-volume", &waitForVolumeSec, &errors)
	parseTimeoutArg("timeout-server", &waitForServerSec, &errors)
	parseTimeoutArg("timeout-snapshot", &waitForSnapshotSec, &errors)
	parseTimeoutArg("timeout-backup", &waitForBackupSec, &errors)
	if len(errors) > 0 {
		return fmt.Errorf("%q", errors)
	}
	return nil
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
	loc.Src.Username = os.Getenv("OS_USERNAME")
	loc.Src.Password = os.Getenv("OS_PASSWORD")
	loc.Src.ApplicationCredentialName = os.Getenv("OS_APPLICATION_CREDENTIAL_NAME")
	loc.Src.ApplicationCredentialID = os.Getenv("OS_APPLICATION_CREDENTIAL_ID")
	loc.Src.ApplicationCredentialSecret = os.Getenv("OS_APPLICATION_CREDENTIAL_SECRET")
	loc.Src.Token = os.Getenv("OS_AUTH_TOKEN")

	loc.Dst.Origin = "dst"
	loc.Dst.Region = viper.GetString("to-region")
	loc.Dst.AuthURL = viper.GetString("to-auth-url")
	loc.Dst.Domain = viper.GetString("to-domain")
	loc.Dst.Project = viper.GetString("to-project")
	loc.Dst.Username = viper.GetString("to-username")
	loc.Dst.Password = viper.GetString("to-password")
	loc.Dst.ApplicationCredentialName = viper.GetString("to-application-credential-name")
	loc.Dst.ApplicationCredentialID = viper.GetString("to-application-credential-id")
	loc.Dst.ApplicationCredentialSecret = viper.GetString("to-application-credential-secret")

	if loc.Dst.Project == "" {
		loc.Dst.Project = loc.Src.Project
	}

	if loc.Dst.Region == "" {
		loc.Dst.Region = loc.Src.Region
	}

	if loc.Dst.Domain == "" {
		loc.Dst.Domain = loc.Src.Domain
	}

	if loc.Dst.Username == "" {
		loc.Dst.Username = loc.Src.Username
	}

	if loc.Dst.Password == "" {
		loc.Dst.Password = loc.Src.Password
	}

	if loc.Dst.AuthURL == "" {
		// try to transform a source auth URL to a destination source URL
		s := strings.Replace(loc.Src.AuthURL, loc.Src.Region, loc.Dst.Region, 1)
		if strings.Contains(s, loc.Dst.Region) {
			loc.Dst.AuthURL = s
			log.Printf("Detected %q destination auth URL", loc.Dst.AuthURL)
		} else {
			return loc, fmt.Errorf("failed to detect destination auth URL, please specify --to-auth-url explicitly")
		}
	}

	loc.SameRegion = false
	if loc.Src.Region == loc.Dst.Region {
		if loc.Src.Domain == loc.Dst.Domain {
			if loc.Src.Project == loc.Dst.Project {
				loc.SameProject = true
				// share the same token
				loc.Dst.Token = loc.Src.Token
			}
		}
		loc.SameRegion = true
	}

	return loc, nil
}
