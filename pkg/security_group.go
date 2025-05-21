package pkg

import (
	"context"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/groups"
	"github.com/gophercloud/gophercloud/v2/openstack/networking/v2/extensions/security/rules"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	waitForSecurityGroupSec    float64
	syncedSrcDstSecurityGroups = make(map[string]string)
	disableDetection           bool
)

func securityGroupsIDFromName(ctx context.Context, client *gophercloud.ServiceClient, name string) (string, error) {
	pages, err := groups.List(client, groups.ListOpts{
		Name: name,
	}).AllPages(ctx)
	if err != nil {
		return "", err
	}

	all, err := groups.ExtractGroups(pages)
	if err != nil {
		return "", err
	}

	ids := make([]string, len(all))
	for i := range all {
		ids[i] = all[i].ID
	}

	switch count := len(ids); count {
	case 0:
		return "", gophercloud.ErrResourceNotFound{Name: name, ResourceType: "security-group"}
	case 1:
		return ids[0], nil
	default:
		return "", gophercloud.ErrMultipleResourcesFound{Name: name, Count: count, ResourceType: "security-group"}
	}
}

func retryRulesCreate(ctx context.Context, client *gophercloud.ServiceClient, opts rules.CreateOpts) (*rules.SecGroupRule, error) {
	var rule *rules.SecGroupRule
	var err error
	err = NewBackoff(int(waitForSecurityGroupSec), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		rule, err = rules.Create(ctx, client, opts).Extract()
		if gophercloud.ResponseCodeIs(err, http.StatusTooManyRequests) {
			// 429 is a rate limit error, we should retry
			return false, nil
		}
		return true, err
	})
	return rule, err
}

// SecurityGroupCmd represents the security group command.
var SecurityGroupCmd = &cobra.Command{
	Use:   "security-group <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a security group",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate security group
		securityGroup := args[0]
		toName, _ := cmd.Flags().GetString("to-security-group-name")
		disableDetection, _ = cmd.Flags().GetBool("disable-target-security-group-detection")

		// source and destination parameters
		loc, err := getSrcAndDst("")
		if err != nil {
			return err
		}

		srcProvider, err := newOpenStackClient(cmd.Context(), loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %v", err)
		}

		srcNetworkClient, err := newNetworkV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source network client: %v", err)
		}

		// resolve security group name to an ID
		if v, err := securityGroupsIDFromName(cmd.Context(), srcNetworkClient, securityGroup); err == nil {
			securityGroup = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		dstProvider, err := newOpenStackClient(cmd.Context(), loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %v", err)
		}

		dstNetworkClient, err := newNetworkV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination network client: %v", err)
		}

		defer measureTime()

		dstSecurityGroup, err := migrateSecurityGroup(cmd.Context(), srcNetworkClient, dstNetworkClient, securityGroup, toName)
		if err != nil {
			return err
		}

		log.Printf("Target security group name is %q (id: %q)", dstSecurityGroup.Name, dstSecurityGroup.ID)

		return nil
	},
}

func migrateSecurityGroup(ctx context.Context, srcNetworkClient *gophercloud.ServiceClient, dstNetworkClient *gophercloud.ServiceClient, securityGroup string, toSecurityGroupName string) (*groups.SecGroup, error) {
	sg, err := groups.Get(ctx, srcNetworkClient, securityGroup).Extract()
	if err != nil {
		return nil, err
	}

	if toSecurityGroupName == "" {
		toSecurityGroupName = sg.Name
	}
	if !disableDetection {
		// check if security group with the same name already exists in the destination
		if secGroupID, err := securityGroupsIDFromName(ctx, dstNetworkClient, toSecurityGroupName); err == nil {
			// security group already exists
			return groups.Get(ctx, dstNetworkClient, secGroupID).Extract()
		}
	}

	log.Printf("Creating security group %q", toSecurityGroupName)
	createOpts := groups.CreateOpts{
		Name:        toSecurityGroupName,
		Description: sg.Description,
	}
	newSecurityGroup, err := groups.Create(ctx, dstNetworkClient, createOpts).Extract()
	if err != nil {
		return nil, err
	}
	// store mapping of source and destination security group IDs in case we encounter a rule with a remote group
	syncedSrcDstSecurityGroups[securityGroup] = newSecurityGroup.ID

	// delete default egress rules to get a clean slate
	for _, rule := range newSecurityGroup.Rules {
		if rule.Direction == "egress" {
			if err = rules.Delete(ctx, dstNetworkClient, rule.ID).ExtractErr(); err != nil {
				return nil, err
			}
		}
	}

	for _, rule := range sg.Rules {
		var remoteGroupID string
		if rule.RemoteGroupID != "" {
			if rule.RemoteGroupID == rule.ID {
				remoteGroupID = newSecurityGroup.ID
			} else if targetRemoteID, ok := syncedSrcDstSecurityGroups[rule.RemoteGroupID]; ok {
				// remote group already exists
				remoteGroupID = targetRemoteID
			} else {
				// create remote security group if it doesn't exist
				remoteSecurityGroup, err := migrateSecurityGroup(ctx, srcNetworkClient, dstNetworkClient, rule.RemoteGroupID, "")
				if err != nil {
					return nil, err
				}
				// update rule with new remote group ID
				remoteGroupID = remoteSecurityGroup.ID
			}
		}

		ruleCreateOpts := rules.CreateOpts{
			Direction:      rules.RuleDirection(rule.Direction),
			Description:    rule.Description,
			EtherType:      rules.RuleEtherType(rule.EtherType),
			SecGroupID:     newSecurityGroup.ID,
			PortRangeMax:   rule.PortRangeMax,
			PortRangeMin:   rule.PortRangeMin,
			Protocol:       rules.RuleProtocol(rule.Protocol),
			RemoteGroupID:  remoteGroupID,
			RemoteIPPrefix: rule.RemoteIPPrefix,
		}

		// retry rule creation on 429 rate limit errors
		if _, err = retryRulesCreate(ctx, dstNetworkClient, ruleCreateOpts); err != nil {
			return nil, err
		}
	}

	return newSecurityGroup, nil
}

func init() {
	initSecurityGroupCmdFlags()
	RootCmd.AddCommand(SecurityGroupCmd)
}

func initSecurityGroupCmdFlags() {
	SecurityGroupCmd.Flags().StringP("to-security-group-name", "", "", "destination security group name")
	SecurityGroupCmd.Flags().BoolP("disable-target-security-group-detection", "", false, "disable automatic detection of existent target security groups")
}
