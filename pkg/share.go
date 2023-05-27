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

var replicaNormalStates = []string{
	"in_sync",
}

/*
func createShareSnapshotSpeed(snapshot *snapshots.Snapshot) {
	t := snapshot.UpdatedAt.Sub(snapshot.CreatedAt)
	log.Printf("Time to create a snapshot: %s", t)
	size := float64(snapshot.Size * 1024)
	log.Printf("Size of the snapshot: %.2f Mb", size)
	log.Printf("Speed of the snapshot creation: %.2f Mb/sec", size/t.Seconds())
}
*/

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

func createShareSpeed(share *shares.Share) {
	// cinder doesn't update the UpdatedAt attribute, when the share status is updated
	t := time.Since(share.CreatedAt)
	log.Printf("Time to create a share: %s", t)
	size := float64(share.Size * 1024)
	log.Printf("Size of the share: %.2f Mb", size)
	log.Printf("Speed of the share creation: %.2f Mb/sec", size/t.Seconds())
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

func findShareActiveReplica(srcShareClient *gophercloud.ServiceClient, shareID string) (*replicas.Replica, error) {
	listReplicasOpts := replicas.ListOpts{
		ShareID: shareID,
	}
	pages, err := replicas.List(srcShareClient, listReplicasOpts).AllPages()
	if err != nil {
		return nil, fmt.Errorf("failed to list %s share replicas: %s", shareID, err)
	}
	allReplicas, err := replicas.ExtractReplicas(pages)
	if err != nil {
		return nil, fmt.Errorf("failed to extract %s share replicas: %s", shareID, err)
	}
	if len(allReplicas) != 1 {
		return nil, fmt.Errorf("failed to find a single replica for a share: %d", len(allReplicas))
	}
	for _, v := range allReplicas {
		if v.Status == "available" && v.State == "active" {
			return &v, nil
		}
	}
	return nil, fmt.Errorf("failed to find a replica for a %q share", shareID)
}

func cloneShare(srcShareClient *gophercloud.ServiceClient, srcShare *shares.Share, name, shareType, netID, az string, loc Locations) (*shares.Share, error) {
	snapshotOpts := snapshots.CreateOpts{
		ShareID:     srcShare.ID,
		Description: fmt.Sprintf("Transition snapshot to clone a %q share", srcShare.ID),
	}
	srcSnapshot, err := snapshots.Create(srcShareClient, snapshotOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a source share snapshot: %s", err)
	}
	log.Printf("Intermediate snapshot %q created", srcSnapshot.ID)

	defer func() {
		if err := snapshots.Delete(srcShareClient, srcSnapshot.ID).ExtractErr(); err != nil {
			log.Printf("Failed to delete a transition snapshot: %s", err)
		}
	}()

	srcSnapshot, err = waitForShareSnapshot(srcShareClient, srcSnapshot.ID, waitForShareSnapshotSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a snapshot: %s", err)
	}

	//createShareSnapshotSpeed(srcSnapshot)

	if shareType == "" {
		shareType = srcShare.ShareType
	}
	if name == "" {
		name = srcShare.Name + " clone"
	}
	if netID == "" {
		netID = srcShare.ShareNetworkID
	}
	shareOpts := &shares.CreateOpts{
		Name:             name,
		SnapshotID:       srcSnapshot.ID,
		ShareNetworkID:   netID,
		ShareProto:       srcShare.ShareProto,
		Size:             srcShare.Size,
		ShareType:        shareType,
		Metadata:         srcShare.Metadata,
		AvailabilityZone: srcShare.AvailabilityZone,
	}

	reauthClient(srcShareClient, "cloneShare")

	newShare, err := shares.Create(srcShareClient, shareOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a source share from a snapshot: %s", err)
	}
	newShare, err = waitForShare(srcShareClient, newShare.ID, waitForShareSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q share status: %s", newShare.ID, err)
	}

	// detect current share replica
	srcShareClient.Microversion = "2.60"
	tmpReplica, err := findShareActiveReplica(srcShareClient, newShare.ID)
	if err != nil {
		return nil, err
	}
	// create replica in a new AZ
	replicaOpts := &replicas.CreateOpts{
		ShareID:          newShare.ID,
		AvailabilityZone: az,
		//ShareNetworkID:
	}
	replica, err := replicas.Create(srcShareClient, replicaOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a new replica for a %q share: %s", newShare.ID, err)
	}
	replica, err = waitForShareReplica(srcShareClient, replica.ID, waitForShareReplicaSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q share replica status: %s", newShare.ID, err)
	}
	// activate replica in a new AZ
	err = replicas.Resync(srcShareClient, replica.ID).ExtractErr()
	if err != nil {
	}
	replica, err = waitForShareReplicaState(srcShareClient, replica.ID, "in_sync", waitForShareReplicaSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q share replica state: %s", newShare.ID, err)
	}
	err = replicas.Promote(srcShareClient, replica.ID, replicas.PromoteOpts{}).ExtractErr()
	if err != nil {
	}
	replica, err = waitForShareReplicaState(srcShareClient, replica.ID, "active", waitForShareReplicaSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q share replica state: %s", newShare.ID, err)
	}
	// remove old replica
	err = replicas.Delete(srcShareClient, tmpReplica.ID).ExtractErr()
	if err != nil {
		return nil, fmt.Errorf("failed to delete an old tmp %q replica: %s", tmpReplica.ID, err)
	}

	newShare, err = waitForShare(srcShareClient, newShare.ID, waitForShareSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a share: %s", err)
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
		toShareNetworkID := viper.GetString("-to-share-network-id")

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

		// srcShareClient, srcObjectClient *gophercloud.ServiceClient, srcShare *shares.Share, name, az string, loc Locations
		dstShare, err := cloneShare(srcShareClient, srcShare, toShareName, toShareType, toShareNetworkID, toAZ, loc)
		if err != nil {
			return err
		}

		log.Printf("Migrated target share name is %q (id: %q) to %q availability zone", dstShare.Name, dstShare.ID, dstShare.AvailabilityZone)

		return nil
	},
}

func init() {
	initShareCmdFlags()
	RootCmd.AddCommand(ShareCmd)
}

func initShareCmdFlags() {
	ShareCmd.Flags().StringP("to-az", "", "", "destination share availability zone")
	ShareCmd.Flags().StringP("to-share-name", "", "", "destination share name")
	ShareCmd.Flags().StringP("to-share-type", "", "", "destination share type")
	ShareCmd.Flags().StringP("to-share-network-id", "", "", "destination share network ID")
}
