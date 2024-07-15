package pkg

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/xhit/go-str2duration/v2"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/acls"
	"github.com/gophercloud/gophercloud/v2/openstack/keymanager/v1/secrets"
)

var (
	waitForSecretSec   float64
	secretWaitStatuses = []string{
		"PENDING",
	}
)

func secretsIDFromName(ctx context.Context, client *gophercloud.ServiceClient, name string) (string, error) {
	pages, err := secrets.List(client, secrets.ListOpts{
		Name: name,
	}).AllPages(ctx)
	if err != nil {
		return "", err
	}

	all, err := secrets.ExtractSecrets(pages)
	if err != nil {
		return "", err
	}

	ids := make([]string, len(all))
	for i := range all {
		ids[i] = all[i].SecretRef
	}

	switch count := len(ids); count {
	case 0:
		return "", gophercloud.ErrResourceNotFound{Name: name, ResourceType: "secret"}
	case 1:
		return uuidFromSecretRef(ids[0]), nil
	default:
		return "", gophercloud.ErrMultipleResourcesFound{Name: name, Count: count, ResourceType: "secret"}
	}
}

func waitForSecret(ctx context.Context, client *gophercloud.ServiceClient, id string, secs float64) (*secrets.Secret, error) {
	var secret *secrets.Secret
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		secret, err = secrets.Get(ctx, client, id).Extract()
		if err != nil {
			return false, err
		}

		// show user friendly status
		log.Printf("secret status: %s", secret.Status)

		if secret.Status == "ACTIVE" {
			return true, nil
		}

		if !isSliceContainsStr(secretWaitStatuses, secret.Status) {
			return false, fmt.Errorf("secret status is %q", secret.Status)
		}

		// continue status checks
		return false, nil
	})

	return secret, err
}

func secretPayload(ctx context.Context, kmClient *gophercloud.ServiceClient, id, contentType string) (string, error) {
	opts := secrets.GetPayloadOpts{
		PayloadContentType: contentType,
	}
	payload, err := secrets.GetPayload(ctx, kmClient, id, opts).Extract()
	if err != nil {
		return "", fmt.Errorf("could not retrieve payload for secret with id %s: %v", id, err)
	}

	if !strings.HasPrefix(contentType, "text/") {
		return base64.StdEncoding.EncodeToString(payload), nil
	}

	return string(payload), nil
}

func migrateSecret(ctx context.Context, srcSecretClient, dstSecretClient *gophercloud.ServiceClient, srcSecret *secrets.Secret, toSecretName string, toExpiration time.Duration) (*secrets.Secret, error) {
	id := uuidFromSecretRef(srcSecret.SecretRef)
	contentType := srcSecret.ContentTypes["default"]
	payload, err := secretPayload(ctx, srcSecretClient, id, contentType)
	if err != nil {
		return nil, err
	}

	acl, err := acls.GetSecretACL(ctx, srcSecretClient, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("unable to get %s secret acls: %v", id, err)
	}

	metadataMap, err := secrets.GetMetadata(ctx, srcSecretClient, id).Extract()
	if err != nil {
		return nil, fmt.Errorf("unable to get %s secret metadata: %v", id, err)
	}

	// WRITE
	name := srcSecret.Name
	if toSecretName != "" {
		name = toSecretName
	}
	now := time.Now().UTC()
	// default to one month
	expiration := now.AddDate(0, 1, 0)
	// if source expiration is not expired, use its expiration date
	if srcSecret.Expiration.After(now) {
		log.Printf("The expiration date for the source secret (%s) has already passed", srcSecret.Expiration)
		expiration = srcSecret.Expiration
	}
	// if custom expiration is set, enforce it
	if toExpiration > 0 {
		expiration = now.Add(toExpiration)
	}
	log.Printf("setting destination secret expiration date to %s", expiration)
	createOpts := secrets.CreateOpts{
		Name:       name,
		Algorithm:  srcSecret.Algorithm,
		BitLength:  srcSecret.BitLength,
		Mode:       srcSecret.Mode,
		Expiration: &expiration,
		SecretType: secrets.SecretType(srcSecret.SecretType),
	}
	dstSecret, err := secrets.Create(ctx, dstSecretClient, createOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("error creating the destination secret: %v", err)
	}

	dstID := uuidFromSecretRef(dstSecret.SecretRef)
	cleanup := func() {
		if err == nil {
			return
		}

		// cleanup partially created secret
		log.Printf("cleanup partially created %s destination secret", dstID)
		err := secrets.Delete(ctx, dstSecretClient, dstID).ExtractErr()
		if err != nil {
			log.Printf("failed to delete partially created %s destination secret", dstID)
		}
	}
	defer cleanup()

	// populate the "dstSecret", since Create method returns only the SecretRef
	dstSecret, err = waitForSecret(ctx, dstSecretClient, dstID, waitForSecretSec)
	if err != nil {
		return nil, fmt.Errorf("error waiting for the destination secret: %v", err)
	}

	// set the acl first before uploading the payload
	if acl != nil {
		acl, ok := (*acl)["read"]
		if ok {
			var setOpts acls.SetOpts
			var users *[]string
			if len(acl.Users) > 0 {
				users = &acl.Users
			}
			setOpts = []acls.SetOpt{
				{
					Type:          "read",
					Users:         users,
					ProjectAccess: &acl.ProjectAccess,
				},
			}
			_, err = acls.SetSecretACL(ctx, dstSecretClient, dstID, setOpts).Extract()
			if err != nil {
				return nil, fmt.Errorf("error settings ACLs for the destination secret: %v", err)
			}
		}
	}

	encoding := ""
	if !strings.HasPrefix(contentType, "text/") {
		encoding = "base64"
	}
	updateOpts := secrets.UpdateOpts{
		Payload:         payload,
		ContentType:     contentType,
		ContentEncoding: encoding,
	}
	err = secrets.Update(ctx, dstSecretClient, dstID, updateOpts).Err
	if err != nil {
		return nil, fmt.Errorf("error setting the destination secret payload: %v", err)
	}

	_, err = waitForSecret(ctx, dstSecretClient, dstID, waitForSecretSec)
	if err != nil {
		return nil, fmt.Errorf("error waiting for the destination secret: %v", err)
	}

	if len(metadataMap) == 0 {
		return dstSecret, nil
	}

	_, err = secrets.CreateMetadata(ctx, dstSecretClient, dstID, secrets.MetadataOpts(metadataMap)).Extract()
	if err != nil {
		return nil, fmt.Errorf("error creating metadata for the destination secret: %v", err)
	}

	_, err = waitForSecret(ctx, dstSecretClient, dstID, waitForSecretSec)
	if err != nil {
		return nil, fmt.Errorf("error waiting for the destination secret: %v", err)
	}

	return dstSecret, nil
}

func uuidFromSecretRef(ref string) string {
	// secret ref has form https://{barbican_host}/v1/secrets/{secret_uuid}
	// so we are only interested in the last part
	return ref[strings.LastIndex(ref, "/")+1:]
}

// SecretCmd represents the secret command.
var SecretCmd = &cobra.Command{
	Use:   "secret <name|id|url>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a secret",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate image
		secret := args[0]
		toName := viper.GetString("to-secret-name")
		toExp := viper.GetString("to-secret-expiration")
		var toExpiration time.Duration

		if toExp != "" {
			var err error
			toExpiration, err = str2duration.ParseDuration(toExp)
			if err != nil {
				return fmt.Errorf("failed to parse --to-add-expiration-duration value: %v", err)
			}
		}

		// source and destination parameters
		loc, err := getSrcAndDst("")
		if err != nil {
			return err
		}

		srcProvider, err := newOpenStackClient(cmd.Context(), loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %v", err)
		}

		srcSecretClient, err := newSecretManagerV1Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source keymanager client: %v", err)
		}

		// resolve secret name to an ID
		if v, err := secretsIDFromName(cmd.Context(), srcSecretClient, secret); err == nil {
			secret = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		srcSecret, err := waitForSecret(cmd.Context(), srcSecretClient, secret, waitForSecretSec)
		if err != nil {
			// try to get secret uuid from URL
			srcSecret, err = waitForSecret(cmd.Context(), srcSecretClient, uuidFromSecretRef(secret), waitForSecretSec)
			if err != nil {
				return fmt.Errorf("failed to wait for %q source image: %v", secret, err)
			}
		}

		dstProvider, err := newOpenStackClient(cmd.Context(), loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %v", err)
		}

		dstSecretClient, err := newSecretManagerV1Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination image client: %v", err)
		}

		defer measureTime()

		dstSecret, err := migrateSecret(cmd.Context(), srcSecretClient, dstSecretClient, srcSecret, toName, toExpiration)
		if err != nil {
			return err
		}

		log.Printf("Target secret name is %q (id: %q)", dstSecret.Name, dstSecret.SecretRef)

		return nil
	},
}

func init() {
	initSecretCmdFlags()
	RootCmd.AddCommand(SecretCmd)
}

func initSecretCmdFlags() {
	SecretCmd.Flags().StringP("to-secret-name", "", "", "destination secret name")
	SecretCmd.Flags().StringP("to-secret-expiration", "", "", "destination secret expiration duration from now (if not set defaults to source expiration; if source expiration is also not set or already expired, automatically add one month from now)")
}
