package pkg

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/availabilityzones"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/applicationcredentials"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/tokens"
	"github.com/gophercloud/utils/client"
	"github.com/gophercloud/utils/openstack/clientconfig"
	"github.com/howeyc/gopass"
)

var (
	startTime          time.Time = time.Now()
	backoffFactor                = 2
	backoffMaxInterval           = 10 * time.Second
)

func measureTime(caption ...string) {
	if len(caption) == 0 {
		log.Printf("Total execution time: %s", time.Since(startTime))
	} else {
		log.Printf(caption[0], time.Since(startTime))
	}
}

func newOpenStackClient(loc Location) (*gophercloud.ProviderClient, error) {
	envPrefix := "OS_"
	if loc.Origin == "dst" {
		envPrefix = "TO_OS_"
	}
	ao, err := clientconfig.AuthOptions(&clientconfig.ClientOpts{
		EnvPrefix: envPrefix,
		AuthInfo: &clientconfig.AuthInfo{
			AuthURL:                     loc.AuthURL,
			Username:                    loc.Username,
			Password:                    loc.Password,
			DomainName:                  loc.Domain,
			ProjectName:                 loc.Project,
			ApplicationCredentialID:     loc.ApplicationCredentialID,
			ApplicationCredentialName:   loc.ApplicationCredentialName,
			ApplicationCredentialSecret: loc.ApplicationCredentialSecret,
			Token:                       loc.Token,
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
	provider.UserAgent.Prepend("cyclone/" + Version)

	// debug logger is enabled by default and writes logs into a cyclone temp dir
	provider.HTTPClient = http.Client{
		Transport: &client.RoundTripper{
			MaxRetries: 5,
			Rt:         &http.Transport{},
			Logger:     &logger{Prefix: loc.Origin},
		},
	}

	if ao.ApplicationCredentialSecret == "" && ao.TokenID == "" &&
		ao.Username != "" && ao.Password == "" {
		fmt.Printf("Enter the %s password: ", loc.Origin)
		v, err := gopass.GetPasswd()
		if err != nil {
			return nil, err
		}
		ao.Password = string(v)
	}

	err = openstack.Authenticate(provider, *ao)
	if err != nil {
		return nil, err
	}

	if ao.TokenID != "" {
		// force application credential creation to allow further reauth
		log.Printf("Force %s application credential creation due to OpenStack Keystone token auth", loc.Origin)
		userID, err := getAuthUserID(provider)
		if err != nil {
			return nil, err
		}

		identityClient, err := openstack.NewIdentityV3(provider, gophercloud.EndpointOpts{
			Region: loc.Region,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create OpenStack Identity V3 client: %s", err)
		}

		acName := fmt.Sprintf("cyclone_%s_%s", time.Now().Format("20060102150405"), loc.Origin)
		createOpts := applicationcredentials.CreateOpts{
			Name:        acName,
			Description: "temp credentials for cyclone",
			// we need to be able to delete AC token within its own scope
			Unrestricted: true,
		}

		acWait := &sync.WaitGroup{}
		acWait.Add(1)
		var ac *applicationcredentials.ApplicationCredential
		cleanupFuncs = append(cleanupFuncs, func(wg *sync.WaitGroup) {
			defer wg.Done()
			log.Printf("Cleaning up %q application credential", acName)

			// wait for ac Create response
			acWait.Wait()
			if ac == nil {
				// nothing to delete
				return
			}

			if err := applicationcredentials.Delete(identityClient, userID, ac.ID).ExtractErr(); err != nil {
				if _, ok := err.(gophercloud.ErrDefault404); !ok {
					log.Printf("Failed to delete a %q temp application credential: %s", acName, err)
				}
			}
		})
		ac, err = applicationcredentials.Create(identityClient, userID, createOpts).Extract()
		acWait.Done()
		if err != nil {
			if v, ok := err.(gophercloud.ErrDefault404); ok {
				return nil, fmt.Errorf("failed to create a temp application credential: %s", v.ErrUnexpectedResponseCode.Body)
			}
			return nil, fmt.Errorf("failed to create a temp application credential: %s", err)
		}

		// set new auth options
		ao = &gophercloud.AuthOptions{
			IdentityEndpoint:            loc.AuthURL,
			ApplicationCredentialID:     ac.ID,
			ApplicationCredentialSecret: ac.Secret,
			AllowReauth:                 true,
		}

		err = openstack.Authenticate(provider, *ao)
		if err != nil {
			return nil, fmt.Errorf("failed to auth using just created application credentials: %s", err)
		}
	}

	return provider, nil
}

func reauthClient(client *gophercloud.ServiceClient, funcName string) {
	// reauth the client before the long running action to avoid openstack internal auth issues
	if client.ProviderClient.ReauthFunc != nil {
		if err := client.ProviderClient.Reauthenticate(client.ProviderClient.TokenID); err != nil {
			log.Printf("Failed to re-authenticate the provider client in the %s func: %v", err, funcName)
		}
	}
}

func newGlanceV2Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewImageServiceV2(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func newBlockStorageV3Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewBlockStorageV3(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func newObjectStorageV1Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewObjectStorageV1(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func newComputeV2Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
	return openstack.NewComputeV2(provider, gophercloud.EndpointOpts{
		Region: region,
	})
}

func newNetworkV2Client(provider *gophercloud.ProviderClient, region string) (*gophercloud.ServiceClient, error) {
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

	if client == nil {
		return fmt.Errorf("no service client provided")
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
		if z.ZoneState.Available {
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

func getAuthUserID(client *gophercloud.ProviderClient) (string, error) {
	if client == nil {
		return "", fmt.Errorf("provider client is nil")
	}
	r := client.GetAuthResult()
	if r == nil {
		return "", fmt.Errorf("provider client auth result is nil")
	}
	switch r := r.(type) {
	case tokens.CreateResult:
		v, err := r.ExtractUser()
		if err != nil {
			return "", err
		}
		return v.ID, nil
	case tokens.GetResult:
		v, err := r.ExtractUser()
		if err != nil {
			return "", err
		}
		return v.ID, nil
	default:
		return "", fmt.Errorf("got unexpected AuthResult type %t", r)
	}
}

func getAuthProjectID(client *gophercloud.ProviderClient) (string, error) {
	if client == nil {
		return "", fmt.Errorf("provider client is nil")
	}
	r := client.GetAuthResult()
	if r == nil {
		return "", fmt.Errorf("provider client auth result is nil")
	}
	switch r := r.(type) {
	case tokens.CreateResult:
		v, err := r.ExtractProject()
		if err != nil {
			return "", err
		}
		return v.ID, nil
	case tokens.GetResult:
		v, err := r.ExtractProject()
		if err != nil {
			return "", err
		}
		return v.ID, nil
	default:
		return "", fmt.Errorf("got unexpected AuthResult type %t", r)
	}
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

// Backoff options.
type Backoff struct {
	Timeout     int
	Factor      int
	MaxInterval time.Duration
}

func NewBackoff(timeout int, factor int, maxInterval time.Duration) *Backoff {
	return &Backoff{
		Timeout:     timeout,
		Factor:      factor,
		MaxInterval: maxInterval,
	}
}

// WaitFor method polls a predicate function, once per interval with an
// arithmetic backoff, up to a timeout limit. This is an enhanced
// gophercloud.WaitFor function with a logic from
// https://github.com/sapcc/go-bits/blob/master/retry/pkg.go
func (eb *Backoff) WaitFor(predicate func() (bool, error)) error {
	type WaitForResult struct {
		Success bool
		Error   error
	}

	start := time.Now().Unix()
	duration := time.Second

	for {
		// If a timeout is set, and that's been exceeded, shut it down.
		if eb.Timeout >= 0 && time.Now().Unix()-start >= int64(eb.Timeout) {
			return fmt.Errorf("a timeout occurred")
		}

		duration += time.Second * time.Duration(eb.Factor)
		if duration > eb.MaxInterval {
			duration = eb.MaxInterval
		}
		time.Sleep(duration)

		var result WaitForResult
		ch := make(chan bool, 1)
		go func() {
			defer close(ch)
			satisfied, err := predicate()
			result.Success = satisfied
			result.Error = err
		}()

		select {
		case <-ch:
			if result.Error != nil {
				return result.Error
			}
			if result.Success {
				return nil
			}
		// If the predicate has not finished by the timeout, cancel it.
		case <-time.After(time.Duration(eb.Timeout) * time.Second):
			return fmt.Errorf("a timeout occurred")
		}
	}
}
