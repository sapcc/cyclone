package pkg

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/backups"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/volumeactions"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/volumetransfers"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/containers"
	volumes_utils "github.com/gophercloud/utils/openstack/blockstorage/v3/volumes"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var skipVolumeAttributes = []string{
	"direct_url",
	"boot_roles",
	"os_hash_algo",
	"os_hash_value",
	"checksum",
	"size",
	"container_format",
	"disk_format",
	"image_id",
	// these integer values have to be set separately
	"min_disk",
	"min_ram",
}

var (
	waitForVolumeSec   uint = 3600
	waitForSnapshotSec uint = 3600
	waitForBackupSec   uint = 3600
)

func expandVolumeProperties(srcVolume *volumes.Volume) images.UpdateOpts {
	// set min_disk and min_ram from a source volume
	imgAttrUpdateOpts := images.UpdateOpts{
		images.ReplaceImageMinDisk{NewMinDisk: srcVolume.Size},
	}
	if s, ok := srcVolume.VolumeImageMetadata["min_ram"]; ok {
		if minRam, err := strconv.Atoi(s); err == nil {
			imgAttrUpdateOpts = append(imgAttrUpdateOpts, images.ReplaceImageMinRam{NewMinRam: minRam})
		} else {
			log.Printf("Cannot convert %q to integer: %s", s, err)
		}
	}
	for key, value := range srcVolume.VolumeImageMetadata {
		if isSliceContainsStr(skipVolumeAttributes, key) || value == "" {
			continue
		}
		imgAttrUpdateOpts = append(imgAttrUpdateOpts, images.UpdateImageProperty{
			Op:    images.AddOp,
			Name:  key,
			Value: value,
		})
	}
	return imgAttrUpdateOpts
}

func createSnapshotSpeed(snapshot *snapshots.Snapshot) {
	t := snapshot.UpdatedAt.Sub(snapshot.CreatedAt)
	log.Printf("Time to create a snapshot: %s", t)
	size := float64(snapshot.Size * 1024)
	log.Printf("Size of the snapshot: %.2f Mb", size)
	log.Printf("Speed of the snapshot creation: %.2f Mb/sec", size/t.Seconds())
}

func waitForSnapshot(client *gophercloud.ServiceClient, id string, secs uint) (*snapshots.Snapshot, error) {
	var snapshot *snapshots.Snapshot
	var err error
	err = gophercloud.WaitFor(int(secs), func() (bool, error) {
		snapshot, err = snapshots.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Intermediate snapshot status: %s", snapshot.Status)
		if snapshot.Status == "available" {
			return true, nil
		}

		if snapshot.Status == "error" {
			return false, fmt.Errorf("intermediate snapshot status is %q", snapshot.Status)
		}

		// continue status checks
		return false, nil
	})

	return snapshot, err
}

func createBackupSpeed(client *gophercloud.ServiceClient, backup *backups.Backup) {
	container, err := containers.Get(client, backup.Container, nil).Extract()
	if err != nil {
		log.Printf("Failed to detect a backup container size: %s", err)
		return
	}
	t := backup.UpdatedAt.Sub(backup.CreatedAt)
	log.Printf("Time to create a backup: %s", t)
	size := float64(container.BytesUsed / (1024 * 1024))
	log.Printf("Size of the backup: %.2f Mb", size)
	log.Printf("Speed of the backup creation: %.2f Mb/sec", size/t.Seconds())
}

func waitForBackup(client *gophercloud.ServiceClient, id string, secs uint) (*backups.Backup, error) {
	var backup *backups.Backup
	var err error
	err = gophercloud.WaitFor(int(secs), func() (bool, error) {
		backup, err = backups.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Intermediate backup status: %s", backup.Status)
		if backup.Status == "available" {
			return true, nil
		}

		if backup.Status == "error" {
			return false, fmt.Errorf("intermediate backup status is %q", backup.Status)
		}

		// continue status checks
		return false, nil
	})

	return backup, err
}

func createVolumeSpeed(volume *volumes.Volume) {
	// cinder doesn't update the UpdatedAt attribute, when the volume status is updated
	t := time.Now().Sub(volume.CreatedAt)
	log.Printf("Time to create a volume: %s", t)
	size := float64(volume.Size * 1024)
	log.Printf("Size of the volume: %.2f Mb", size)
	log.Printf("Speed of the volume creation: %.2f Mb/sec", size/t.Seconds())
}

func waitForVolume(client *gophercloud.ServiceClient, id string, secs uint) (*volumes.Volume, error) {
	var volume *volumes.Volume
	var err error
	err = gophercloud.WaitFor(int(secs), func() (bool, error) {
		volume, err = volumes.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Volume status: %s", volume.Status)
		// TODO: specify target states in func params
		if volume.Status == "available" || volume.Status == "in-use" {
			return true, nil
		}

		if volume.Status == "error" {
			return false, fmt.Errorf("volume status is %q", volume.Status)
		}

		// continue status checks
		return false, nil
	})

	return volume, err
}

func cloneVolume(srcVolumeClient, srcObjectClient *gophercloud.ServiceClient, srcVolume *volumes.Volume, name, az string, cloneViaSnapshot bool, loc Locations) (*volumes.Volume, error) {
	volOpts := volumes.CreateOpts{
		Name:        name,
		Size:        srcVolume.Size,
		Description: fmt.Sprintf("clone of the %q volume", srcVolume.ID),
		VolumeType:  srcVolume.VolumeType,
	}

	if cloneViaSnapshot {
		// clone via snapshot using cinder storage, because it was explicitly set
		log.Printf("Cloning a %q volume using volume snapshot", srcVolume.ID)

		snapshotOpts := snapshots.CreateOpts{
			VolumeID:    srcVolume.ID,
			Description: fmt.Sprintf("Transition snapshot to clone a %q volume", srcVolume.ID),
			Metadata:    srcVolume.VolumeImageMetadata,
			Force:       true,
		}
		srcSnapshot, err := snapshots.Create(srcVolumeClient, snapshotOpts).Extract()
		if err != nil {
			return nil, fmt.Errorf("failed to create a source volume snapshot: %s", err)
		}
		log.Printf("Intermediate snapshot %q created", srcSnapshot.ID)

		defer func() {
			if err := snapshots.Delete(srcVolumeClient, srcSnapshot.ID).ExtractErr(); err != nil {
				log.Printf("Failed to delete a transition snapshot: %s", err)
			}
		}()

		srcSnapshot, err = waitForSnapshot(srcVolumeClient, srcSnapshot.ID, waitForSnapshotSec)
		if err != nil {
			return nil, fmt.Errorf("failed to wait for a snapshot: %s", err)
		}

		createSnapshotSpeed(srcSnapshot)

		volOpts.SnapshotID = srcSnapshot.ID
	} else {
		if !loc.SameRegion || loc.SameAZ {
			// clone the volume directly, because we don't care about the availability zone
			volOpts.SourceVolID = srcVolume.ID
		} else {
			// clone via backup using swift storage

			// save initial microversion
			mv := srcVolumeClient.Microversion
			srcVolumeClient.Microversion = "3.47"

			defer func() {
				// restore initial microversion
				srcVolumeClient.Microversion = mv
			}()

			backupOpts := backups.CreateOpts{
				VolumeID:    srcVolume.ID,
				Description: fmt.Sprintf("Transition backup to clone a %q volume", srcVolume.ID),
				Container:   fmt.Sprintf("%s_%d", srcVolume.ID, time.Now().Unix()),
				Force:       true,
			}
			srcBackup, err := backups.Create(srcVolumeClient, backupOpts).Extract()
			if err != nil {
				return nil, fmt.Errorf("failed to create a source volume backup: %s", err)
			}
			log.Printf("Intermediate backup %q created", srcBackup.ID)

			defer func() {
				if err := backups.Delete(srcVolumeClient, srcBackup.ID).ExtractErr(); err != nil {
					log.Printf("failed to delete a transition backup: %s", err)
				}
			}()

			srcBackup, err = waitForBackup(srcVolumeClient, srcBackup.ID, waitForBackupSec)
			if err != nil {
				return nil, fmt.Errorf("failed to wait for a backup: %s", err)
			}

			createBackupSpeed(srcObjectClient, srcBackup)

			// restoring a volume backup supports non-original availability zone
			volOpts.AvailabilityZone = az
			volOpts.BackupID = srcBackup.ID
		}
	}

	var newVolume *volumes.Volume
	var err error
	newVolume, err = volumes.Create(srcVolumeClient, volOpts).Extract()
	if err != nil {
		if volOpts.SnapshotID != "" {
			return nil, fmt.Errorf("failed to create a source volume from a snapshot: %s", err)
		}
		if volOpts.SourceVolID != "" {
			return nil, fmt.Errorf("failed to create a volume clone: %s", err)
		}
		return nil, fmt.Errorf("failed to create a source volume from a backup: %s", err)
	}

	defer func() {
		if err != nil {
			if err := volumes.Delete(srcVolumeClient, newVolume.ID, nil).ExtractErr(); err != nil {
				log.Printf("Failed to delete a cloned volume: %s", err)
			}
		}
	}()

	newVolume, err = waitForVolume(srcVolumeClient, newVolume.ID, waitForVolumeSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a volume: %s", err)
	}

	createVolumeSpeed(newVolume)

	return newVolume, nil
}

func migrateVolume(srcImageClient, srcVolumeClient, srcObjectClient, dstImageClient, dstVolumeClient *gophercloud.ServiceClient, srcVolume *volumes.Volume, toVolumeName string, az string, cloneViaSnapshot bool, loc Locations) (*volumes.Volume, error) {
	newVolume, err := cloneVolume(srcVolumeClient, srcObjectClient, srcVolume, toVolumeName, az, cloneViaSnapshot, loc)
	if err != nil {
		return nil, err
	}

	// volume was cloned, now it requires migration
	srcVolume = newVolume

	volDeleted := false
	defer func() {
		if err != nil && !volDeleted {
			if err := volumes.Delete(srcVolumeClient, srcVolume.ID, nil).ExtractErr(); err != nil {
				log.Printf("failed to delete a cloned volume: %s", err)
			}
		}
	}()

	if loc.SameAZ ||
		newVolume.AvailabilityZone == az { // a volume was cloned via backup
		if loc.SameProject {
			// we're done
			return newVolume, nil
		}
		// just change volume ownership
		var v *volumes.Volume
		v, err = transferVolume(srcVolumeClient, dstVolumeClient, newVolume)
		return v, err
	}

	// create an image from volume

	// preserve original image name
	srcImageName, _ := srcVolume.VolumeImageMetadata["image_name"]
	if srcImageName == "" {
		srcImageName = srcVolume.ID
	}

	createSrcImage := volumeactions.UploadImageOpts{
		ContainerFormat: viper.GetString("container-format"),
		DiskFormat:      viper.GetString("disk-format"),
		ImageName:       srcImageName,
		Visibility:      string(images.ImageVisibilityPrivate),
	}
	if v, ok := srcVolume.VolumeImageMetadata["container_format"]; ok && v != "" {
		createSrcImage.ContainerFormat = v
	}
	if v, ok := srcVolume.VolumeImageMetadata["disk_format"]; ok && v != "" {
		createSrcImage.DiskFormat = v
	}

	srcVolumeClient.Microversion = "3.1" // required to set the image visibility
	var srcVolumeImage volumeactions.VolumeImage
	srcVolumeImage, err = volumeactions.UploadImage(srcVolumeClient, srcVolume.ID, createSrcImage).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to convert a source volume to an image: %s", err)
	}

	defer func() {
		log.Printf("Removing transition image %q", srcVolumeImage.ImageID)
		if err := images.Delete(srcImageClient, srcVolumeImage.ImageID).ExtractErr(); err != nil {
			log.Printf("Failed to delete transition image: %s", err)
		}
	}()

	srcImage, err := waitForImage(srcImageClient, srcVolumeImage.ImageID, waitForImageSec)
	if err != nil {
		return nil, fmt.Errorf("failed to convert a volume to an image: %s", err)
	}

	log.Printf("Created %q image", srcImage.ID)

	createImageSpeed(srcImage)

	// sometimes volume can be still in uploading state
	if _, err := waitForVolume(srcVolumeClient, srcVolume.ID, waitForVolumeSec); err != nil {
		return nil, fmt.Errorf("failed to wait for a cloned volume available status: %s", err)
	}
	volDeleted = true
	if err := volumes.Delete(srcVolumeClient, srcVolume.ID, nil).ExtractErr(); err != nil {
		return nil, fmt.Errorf("failed to delete a cloned volume: %s", err)
	}

	log.Printf("Updating image options")
	updateProperties := expandVolumeProperties(srcVolume)
	srcImage, err = images.Update(srcImageClient, srcVolumeImage.ImageID, updateProperties).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to update a transition image properties: %s", err)
	}

	log.Printf("Updated %q image", srcImage.ID)

	var dstImage *images.Image
	imgToVolClient := dstVolumeClient
	imgDstClient := dstImageClient
	if loc.SameRegion {
		// same region, but requested a new availability zone
		dstImage = srcImage
		imgToVolClient = srcVolumeClient
		imgDstClient = srcImageClient
	} else {
		// sourceImageName, _ := srcVolume.VolumeImageMetadata["image_name"] // TODO: check when migrate to a new region
		dstImage, err = migrateImage(srcImageClient, dstImageClient, srcObjectClient, srcImage, srcImageName)
		if err != nil {
			return nil, fmt.Errorf("failed to migrate the image: %s", err)
		}
	}

	defer func() {
		if err := images.Delete(imgDstClient, dstImage.ID).ExtractErr(); err != nil {
			log.Printf("Failed to delete destination transition image: %s", err)
		}
	}()

	// now we have to convert the destination image to a volume, bootable if it was bootable
	volumeName := srcVolume.Name
	if toVolumeName != "" {
		volumeName = toVolumeName
	}
	dstVolumeCreateOpts := volumes.CreateOpts{
		Size:             srcVolume.Size,
		Name:             volumeName,
		AvailabilityZone: az,
		ImageID:          dstImage.ID,
		VolumeType:       srcVolume.VolumeType,
	}
	var dstVolume *volumes.Volume
	dstVolume, err = volumes.Create(imgToVolClient, dstVolumeCreateOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("Failed to create a destination volume: %s", err)
	}

	dstVolume, err = waitForVolume(imgToVolClient, dstVolume.ID, waitForVolumeSec)
	if err != nil {
		// TODO: delete volume?
		return nil, err
	}

	createVolumeSpeed(dstVolume)

	// image can still be in "TODO" state, we need to wait for "available" before defer func will delete it
	_, err = waitForImage(imgDstClient, dstImage.ID, waitForImageSec)
	if err != nil {
		// TODO: delete volume?
		return nil, err
	}

	if loc.SameRegion {
		if loc.SameProject {
			// we're done
			return dstVolume, nil
		}
		// volume
		var v *volumes.Volume
		v, err = transferVolume(imgToVolClient, dstVolumeClient, dstVolume)
		return v, err
	}

	return dstVolume, nil
}

func transferVolume(srcVolumeClient, dstVolumeClient *gophercloud.ServiceClient, srcVolume *volumes.Volume) (*volumes.Volume, error) {
	// change volume ownership
	transferOpts := volumetransfers.CreateOpts{
		VolumeID: srcVolume.ID,
	}
	transfer, err := volumetransfers.Create(srcVolumeClient, transferOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a volume transfer request: %s", err)
	}

	_, err = volumetransfers.Accept(dstVolumeClient, transfer.ID, volumetransfers.AcceptOpts{AuthKey: transfer.AuthKey}).Extract()
	if err != nil {
		if err := volumetransfers.Delete(srcVolumeClient, transfer.ID).ExtractErr(); err != nil {
			log.Printf("Failed to delete a volume transfer request: %s", err)
		}
		return nil, fmt.Errorf("failed to accept a volume transfer request: %s", err)
	}

	return srcVolume, nil
}

// VolumeCmd represents the volume command
var VolumeCmd = &cobra.Command{
	Use:   "volume <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a volume",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate volume

		volume := args[0]

		toAZ := viper.GetString("to-az")
		toName := viper.GetString("to-volume-name")
		cloneViaSnapshot := viper.GetBool("clone-via-snapshot")

		// source and destination parameters
		loc, err := getSrcAndDst(toAZ)
		if err != nil {
			return err
		}

		srcProvider, err := NewOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
		}

		srcImageClient, err := NewGlanceV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source image client: %s", err)
		}

		srcVolumeClient, err := NewBlockStorageV3Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source volume client: %s", err)
		}

		srcObjectClient, err := NewObjectStorageV1Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source object storage client: %s", err)
		}

		// resolve volume name to an ID
		if v, err := volumes_utils.IDFromName(srcVolumeClient, volume); err == nil {
			volume = v
		}

		dstProvider, err := NewOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstImageClient, err := NewGlanceV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination image client: %s", err)
		}

		dstVolumeClient, err := NewBlockStorageV3Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination volume client: %s", err)
		}

		srcVolume, err := waitForVolume(srcVolumeClient, volume, waitForVolumeSec)
		if err != nil {
			return fmt.Errorf("failed to wait for a %q volume: %s", volume, err)
		}

		err = checkAvailabilityZone(dstVolumeClient, srcVolume.AvailabilityZone, &toAZ, &loc)
		if err != nil {
			return err
		}

		defer measureTime()

		dstVolume, err := migrateVolume(srcImageClient, srcVolumeClient, srcObjectClient, dstImageClient, dstVolumeClient, srcVolume, toName, toAZ, cloneViaSnapshot, loc)
		if err != nil {
			return err
		}

		log.Printf("Migrated target volume name is %q (id: %q) to %q availability zone", dstVolume.Name, dstVolume.ID, dstVolume.AvailabilityZone)

		return nil
	},
}

func init() {
	initVolumeCmdFlags()
	RootCmd.AddCommand(VolumeCmd)
}

func initVolumeCmdFlags() {
	VolumeCmd.Flags().StringP("to-az", "", "", "destination volume availability zone")
	VolumeCmd.Flags().StringP("to-volume-name", "", "", "destination volume name")
	VolumeCmd.Flags().StringP("container-format", "", "bare", "image container format, when source volume doesn't have this info")
	VolumeCmd.Flags().StringP("disk-format", "", "vmdk", "image disk format, when source volume doesn't have this info")
	VolumeCmd.Flags().BoolP("clone-via-snapshot", "", false, "clone a volume via snapshot")
}
