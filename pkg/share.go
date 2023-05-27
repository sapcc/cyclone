package pkg

import (
	"fmt"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/sharedfilesystems/v2/replicas"
	"github.com/gophercloud/gophercloud/openstack/sharedfilesystems/v2/shares"
	"github.com/gophercloud/gophercloud/openstack/sharedfilesystems/v2/snapshots"
	shares_utils "github.com/gophercloud/utils/openstack/sharedfilesystems/v2/shares"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	waitForShareSec         float64
	waitForShareSnapshotSec float64
	waitForShareReplicaSec  float64
)

var shareNormalStatuses = []string{
	"available",
	"in-use",
}

var shareSnapshotNormalStatuses = []string{
	"available",
}

var replicaNormalStatuses = []string{
	"available",
}

func waitForShareSnapshot(client *gophercloud.ServiceClient, id string, secs float64) (*snapshots.Snapshot, error) {
	var snapshot *snapshots.Snapshot
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		snapshot, err = snapshots.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Intermediate snapshot status: %s", snapshot.Status)
		if isSliceContainsStr(shareSnapshotNormalStatuses, snapshot.Status) {
			return true, nil
		}

		if strings.Contains(snapshot.Status, "error") {
			return false, fmt.Errorf("intermediate snapshot status is %q", snapshot.Status)
		}

		// continue status checks
		return false, nil
	})

	return snapshot, err
}

func waitForShareReplica(client *gophercloud.ServiceClient, id string, secs float64) (*replicas.Replica, error) {
	var replica *replicas.Replica
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		replica, err = replicas.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Intermediate replica status and state: %s/%s", replica.Status, replica.State)
		if isSliceContainsStr(replicaNormalStatuses, replica.Status) {
			return true, nil
		}

		if strings.Contains(replica.Status, "error") {
			return false, fmt.Errorf("intermediate replica status and state is %s/%s", replica.Status, replica.State)
		}

		// continue status checks
		return false, nil
	})

	return replica, err
}

func waitForShareReplicaState(client *gophercloud.ServiceClient, id, state string, secs float64) (*replicas.Replica, error) {
	var replica *replicas.Replica
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		replica, err = replicas.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Intermediate replica status and state: %s/%s", replica.Status, replica.State)
		if isSliceContainsStr(replicaNormalStatuses, replica.Status) &&
			replica.State == state {
			return true, nil
		}

		if strings.Contains(replica.Status, "error") {
			return false, fmt.Errorf("intermediate replica status and state is %s/%s", replica.Status, replica.State)
		}

		// continue status checks
		return false, nil
	})

	return replica, err
}

func waitForShare(client *gophercloud.ServiceClient, id string, secs float64) (*shares.Share, error) {
	var share *shares.Share
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		share, err = shares.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Share status: %s", share.Status)
		// TODO: specify target states in func params
		if isSliceContainsStr(shareNormalStatuses, share.Status) {
			return true, nil
		}

		if strings.Contains(share.Status, "error") {
			return false, fmt.Errorf("share status is %q", share.Status)
		}

		// continue status checks
		return false, nil
	})

	return share, err
}

func createShareSpeed(share *shares.Share) {
	// cinder doesn't update the UpdatedAt attribute, when the share status is updated
	t := time.Since(share.CreatedAt)
	log.Printf("Time to create a share: %s", t)
	size := float64(share.Size * 1024)
	log.Printf("Size of the share: %.2f Mb", size)
	log.Printf("Speed of the share creation: %.2f Mb/sec", size/t.Seconds())
}

// findOrCreateShareReplica returns the new or existing inactive replica as the first return value and old active replica as the second one
func findOrCreateShareReplica(srcShareClient *gophercloud.ServiceClient, srcShare *shares.Share, netID, az string) (*replicas.Replica, *replicas.Replica, error) {
	curReplica, allReplicas, err := findShareActiveReplica(srcShareClient, srcShare.ID)
	if err != nil {
		return nil, nil, err
	}
	if curReplica.AvailabilityZone == az {
		return nil, nil, fmt.Errorf("the current %q share replica is already in the desired destination zone", curReplica.ID)
	}

	if netID == "" {
		netID = srcShare.ShareNetworkID
	}

	// check whether there is an existing replica in the destination AZ
	for _, v := range allReplicas {
		if v.AvailabilityZone == az && v.Status == "available" {
			if v.ShareNetworkID != netID {
				return nil, nil, fmt.Errorf("the replica was found, but it's created in a different shared network: %s", v.ShareNetworkID)
			}
			// found an existing replica in the destination AZ
			return &v, curReplica, nil
		}
	}

	// create replica in a new AZ
	replicaOpts := &replicas.CreateOpts{
		ShareID:          srcShare.ID,
		AvailabilityZone: az,
		ShareNetworkID:   netID,
	}
	replica, err := replicas.Create(srcShareClient, replicaOpts).Extract()
	if err != nil {
		return nil, curReplica, fmt.Errorf("failed to create a new replica for a %q share: %s", srcShare.ID, err)
	}
	replica, err = waitForShareReplica(srcShareClient, replica.ID, waitForShareReplicaSec)
	if err != nil {
		return nil, curReplica, fmt.Errorf("failed to wait for a %q share replica status: %s", replica.ID, err)
	}

	return replica, curReplica, nil
}

// findShareActiveReplica returns the current active replica if found and the list
// of all replicas associated with a share.
func findShareActiveReplica(srcShareClient *gophercloud.ServiceClient, shareID string) (*replicas.Replica,
	[]replicas.Replica, error) {
	listReplicasOpts := replicas.ListOpts{
		ShareID: shareID,
	}
	pages, err := replicas.ListDetail(srcShareClient, listReplicasOpts).AllPages()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to list %s share replicas: %s", shareID, err)
	}
	allReplicas, err := replicas.ExtractReplicas(pages)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to extract %s share replicas: %s", shareID, err)
	}
	if len(allReplicas) == 0 {
		return nil, nil, fmt.Errorf("failed to find a replica for a %q share", shareID)
	}
	for _, v := range allReplicas {
		if v.Status == "available" && v.State == "active" {
			return &v, allReplicas, nil
		}
	}
	return nil, allReplicas, fmt.Errorf("failed to find a replica for a %q share", shareID)
}

func cloneShare(srcShareClient *gophercloud.ServiceClient, srcShare *shares.Share, name, shareType, proto, netID, az string) (*shares.Share, error) {
	snapshotOpts := snapshots.CreateOpts{
		ShareID:     srcShare.ID,
		Description: fmt.Sprintf("Transition snapshot to clone a %q share", srcShare.ID),
	}
	srcSnapshot, err := snapshots.Create(srcShareClient, snapshotOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a source share snapshot: %s", err)
	}
	log.Printf("Intermediate snapshot %q created", srcSnapshot.ID)

	delSnapshot := func() {
		if err := snapshots.Delete(srcShareClient, srcSnapshot.ID).ExtractErr(); err != nil {
			// it is fine, when the volume was already removed.
			if _, ok := err.(gophercloud.ErrDefault404); !ok {
				log.Printf("failed to delete a cloned volume: %s", err)
			}
		}
	}
	defer delSnapshot()

	srcSnapshot, err = waitForShareSnapshot(srcShareClient, srcSnapshot.ID, waitForShareSnapshotSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a snapshot: %s", err)
	}

	// TODO
	//createShareSnapshotSpeed(srcSnapshot)

	if shareType == "" {
		shareType = srcShare.ShareType
	}
	if proto == "" {
		proto = srcShare.ShareProto
	}
	if name == "" {
		name = fmt.Sprintf("%s clone (%s)", srcShare.Name, srcShare.ID)
	}
	if netID == "" {
		netID = srcShare.ShareNetworkID
	}
	shareOpts := &shares.CreateOpts{
		Name:             name,
		SnapshotID:       srcSnapshot.ID,
		ShareNetworkID:   netID,
		ShareProto:       proto,
		Size:             srcShare.Size,
		ShareType:        shareType,
		Metadata:         srcShare.Metadata,
		AvailabilityZone: srcShare.AvailabilityZone,
	}

	reauthClient(srcShareClient, "cloneShare")

	// create a share clone in the source AZ
	newShare, err := shares.Create(srcShareClient, shareOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a source share from a snapshot: %s", err)
	}
	newShare, err = waitForShare(srcShareClient, newShare.ID, waitForShareSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q share status: %s", newShare.ID, err)
	}

	// delete intermediate snapshot right away
	go delSnapshot()

	return moveShare(srcShareClient, newShare, netID, az, true)
}

func moveShare(srcShareClient *gophercloud.ServiceClient, srcShare *shares.Share, netID, az string, deleteOldReplica bool) (*shares.Share, error) {
	srcShareClient.Microversion = "2.60"
	// detect current share replica
	replica, oldReplica, err := findOrCreateShareReplica(srcShareClient, srcShare, netID, az)
	if err != nil {
		return nil, fmt.Errorf("failed to obtain a replica for a %q share: %s", srcShare.ID, err)
	}

	// resync replica in a new AZ
	err = replicas.Resync(srcShareClient, replica.ID).ExtractErr()
	if err != nil {
		return nil, fmt.Errorf("failed to resync a %q share replica: %s", replica.ID, err)
	}
	replica, err = waitForShareReplicaState(srcShareClient, replica.ID, "in_sync", waitForShareReplicaSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q share replica state: %s", replica.ID, err)
	}

	// promote replica in a new AZ
	err = replicas.Promote(srcShareClient, replica.ID, replicas.PromoteOpts{}).ExtractErr()
	if err != nil {
		return nil, fmt.Errorf("failed to promote a %q share replica: %s", replica.ID, err)
	}
	replica, err = waitForShareReplicaState(srcShareClient, replica.ID, "active", waitForShareReplicaSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q share replica state: %s", replica.ID, err)
	}

	// checking the expected share AZ
	newShare, err := waitForShare(srcShareClient, srcShare.ID, waitForShareSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a share: %s", err)
	}
	if newShare.AvailabilityZone != az {
		return nil, fmt.Errorf("the expected availability zone was not set")
	}

	// remove old replica
	if deleteOldReplica && oldReplica != nil {
		err = replicas.Delete(srcShareClient, oldReplica.ID).ExtractErr()
		if err != nil {
			return nil, fmt.Errorf("failed to delete an old %q replica: %s", oldReplica.ID, err)
		}
	}

	createShareSpeed(newShare)

	return newShare, nil
}

// ShareCmd represents the share command
var ShareCmd = &cobra.Command{
	Use:   "share <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a share",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate share

		share := args[0]

		toAZ := viper.GetString("to-az")
		toShareName := viper.GetString("to-share-name")
		toShareType := viper.GetString("to-share-type")
		toShareProto := viper.GetString("to-share-proto")
		toShareNetworkID := viper.GetString("to-share-network-id")

		// source and destination parameters
		loc, err := getSrcAndDst(toAZ)
		if err != nil {
			return err
		}

		// check the source and destination projects/regions
		if !loc.SameRegion || !loc.SameProject {
			return fmt.Errorf("shares can be copied only within the same OpenStack region and project")
		}

		srcProvider, err := newOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
		}

		srcShareClient, err := newSharedFileSystemV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source share client: %s", err)
		}

		// resolve share name to an ID
		if v, err := shares_utils.IDFromName(srcShareClient, share); err == nil {
			share = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		srcShare, err := waitForShare(srcShareClient, share, waitForShareSec)
		if err != nil {
			return fmt.Errorf("failed to wait for a %q share: %s", share, err)
		}

		err = checkShareAvailabilityZone(srcShareClient, srcShare.AvailabilityZone, &toAZ, &loc)
		if err != nil {
			return err
		}

		defer measureTime()

		dstShare, err := cloneShare(srcShareClient, srcShare, toShareName, toShareType, toShareProto, toShareNetworkID, toAZ)
		if err != nil {
			return err
		}

		log.Printf("Migrated target share name is %q (id: %q) to %q availability zone", dstShare.Name, dstShare.ID, dstShare.AvailabilityZone)

		return nil
	},
}

// ShareMoveCmd represents the share move command
var ShareMoveCmd = &cobra.Command{
	Use:   "move <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Mova a share to a different availability zone",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate share

		share := args[0]

		toAZ := viper.GetString("to-az")
		deleteOldReplica := viper.GetBool("delete-old-replica")
		toShareNetworkID := viper.GetString("to-share-network-id")

		// source and destination parameters
		loc, err := getSrcAndDst(toAZ)
		if err != nil {
			return err
		}

		srcProvider, err := newOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
		}

		srcShareClient, err := newSharedFileSystemV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source share client: %s", err)
		}

		// resolve share name to an ID
		if v, err := shares_utils.IDFromName(srcShareClient, share); err == nil {
			share = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		srcShare, err := waitForShare(srcShareClient, share, waitForShareSec)
		if err != nil {
			return fmt.Errorf("failed to wait for a %q share: %s", share, err)
		}

		err = checkShareAvailabilityZone(srcShareClient, srcShare.AvailabilityZone, &toAZ, &loc)
		if err != nil {
			return err
		}

		defer measureTime()

		dstShare, err := moveShare(srcShareClient, srcShare, toShareNetworkID, toAZ, deleteOldReplica)
		if err != nil {
			return err
		}

		log.Printf("Moved target share name is %q (id: %q) to %q availability zone", dstShare.Name, dstShare.ID, dstShare.AvailabilityZone)

		return nil
	},
}

func init() {
	initShareCmdFlags()
	initShareMoveCmdFlags()
	RootCmd.AddCommand(ShareCmd)
	ShareCmd.AddCommand(ShareMoveCmd)
}

func initShareCmdFlags() {
	ShareCmd.Flags().StringP("to-az", "", "", "destination share availability zone")
	ShareCmd.Flags().StringP("to-share-name", "", "", "destination share name")
	ShareCmd.Flags().StringP("to-share-type", "", "", "destination share type")
	ShareCmd.Flags().StringP("to-share-proto", "", "", "destination share proto")
	ShareCmd.Flags().StringP("to-share-network-id", "", "", "destination share network ID")
}

func initShareMoveCmdFlags() {
	ShareMoveCmd.Flags().StringP("to-az", "", "", "destination share availability zone")
	ShareMoveCmd.Flags().StringP("to-share-network-id", "", "", "destination share network ID")
	ShareMoveCmd.Flags().BoolP("delete-old-replica", "", false, "delete old replica after moving a share (in case when there was one)")
}
