package pkg

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/backups"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/volumeactions"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/volumetransfers"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
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
	waitForVolumeSec   float64
	waitForSnapshotSec float64
)

var volumeNormalStatuses = []string{
	"available",
	"in-use",
}

var snapshotNormalStatuses = []string{
	"available",
}

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

func waitForSnapshot(client *gophercloud.ServiceClient, id string, secs float64) (*snapshots.Snapshot, error) {
	var snapshot *snapshots.Snapshot
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		snapshot, err = snapshots.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Intermediate snapshot status: %s", snapshot.Status)
		if isSliceContainsStr(snapshotNormalStatuses, snapshot.Status) {
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

func createVolumeSpeed(volume *volumes.Volume) {
	// cinder doesn't update the UpdatedAt attribute, when the volume status is updated
	t := time.Since(volume.CreatedAt)
	log.Printf("Time to create a volume: %s", t)
	size := float64(volume.Size * 1024)
	log.Printf("Size of the volume: %.2f Mb", size)
	log.Printf("Speed of the volume creation: %.2f Mb/sec", size/t.Seconds())
}

func waitForVolume(client *gophercloud.ServiceClient, id string, secs float64) (*volumes.Volume, error) {
	var volume *volumes.Volume
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		volume, err = volumes.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Volume status: %s", volume.Status)
		// TODO: specify target states in func params
		if isSliceContainsStr(volumeNormalStatuses, volume.Status) {
			return true, nil
		}

		if strings.Contains(volume.Status, "error") {
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

	reauthClient(srcVolumeClient, "cloneVolume")

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

	reauthClient(srcVolumeClient, "cloneVolume")

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

	newVolumeID := newVolume.ID
	defer func() {
		if err != nil {
			if err := volumes.Delete(srcVolumeClient, newVolumeID, nil).ExtractErr(); err != nil {
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

func volumeToImage(srcImageClient, srcVolumeClient, srcObjectClient *gophercloud.ServiceClient, imageName string, srcVolume *volumes.Volume) (*images.Image, error) {
	createSrcImage := volumeactions.UploadImageOpts{
		ContainerFormat: viper.GetString("container-format"),
		DiskFormat:      viper.GetString("disk-format"),
		Visibility:      string(images.ImageVisibilityPrivate),
		// for some reason this doesn't work, when volume status is in-use
		Force: true,
	}

	if imageName != "" {
		createSrcImage.ImageName = imageName
	} else if v, ok := srcVolume.VolumeImageMetadata["image_name"]; ok && v != "" {
		// preserve source image name
		createSrcImage.ImageName = v
	} else {
		createSrcImage.ImageName = srcVolume.ID
	}

	// preserve source container format
	if v, ok := srcVolume.VolumeImageMetadata["container_format"]; ok && v != "" {
		createSrcImage.ContainerFormat = v
	}

	// preserve source disk format
	if v, ok := srcVolume.VolumeImageMetadata["disk_format"]; ok && v != "" {
		createSrcImage.DiskFormat = v
	}

	reauthClient(srcVolumeClient, "volumeToImage")

	srcVolumeClient.Microversion = "3.1" // required to set the image visibility
	var srcVolumeImage volumeactions.VolumeImage
	srcVolumeImage, err := volumeactions.UploadImage(srcVolumeClient, srcVolume.ID, createSrcImage).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to convert a source volume to an image: %s", err)
	}

	defer func() {
		if err != nil {
			log.Printf("Removing transition image %q", srcVolumeImage.ImageID)
			if err := images.Delete(srcImageClient, srcVolumeImage.ImageID).ExtractErr(); err != nil {
				log.Printf("Failed to delete transition image: %s", err)
			}
		}
	}()

	var srcImage *images.Image
	srcImage, err = waitForImage(srcImageClient, srcObjectClient, srcVolumeImage.ImageID, 0, waitForImageSec)
	if err != nil {
		return nil, fmt.Errorf("failed to convert a volume to an image: %s", err)
	}

	log.Printf("Created %q image", srcImage.ID)

	createImageSpeed(srcImage)

	// sometimes volume can be still in uploading state
	if _, err := waitForVolume(srcVolumeClient, srcVolume.ID, waitForVolumeSec); err != nil {
		// in this case end user can continue the image migration afterwards
		return nil, fmt.Errorf("failed to wait for a cloned volume available status: %s", err)
	}

	log.Printf("Updating image options")
	updateProperties := expandVolumeProperties(srcVolume)
	srcImage, err = images.Update(srcImageClient, srcVolumeImage.ImageID, updateProperties).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to update a transition image properties: %s", err)
	}

	log.Printf("Updated %q image", srcImage.ID)

	return srcImage, nil
}

func migrateVolume(srcImageClient, srcVolumeClient, srcObjectClient, dstImageClient, dstVolumeClient, dstObjectClient *gophercloud.ServiceClient, srcVolume *volumes.Volume, toVolumeName string, toVolumeType, az string, cloneViaSnapshot bool, loc Locations) (*volumes.Volume, error) {
	newVolume, err := cloneVolume(srcVolumeClient, srcObjectClient, srcVolume, toVolumeName, az, cloneViaSnapshot, loc)
	if err != nil {
		return nil, err
	}

	// volume was cloned, now it requires a migration
	srcVolume = newVolume

	if loc.SameAZ ||
		srcVolume.AvailabilityZone == az { // a volume was cloned via backup
		if loc.SameProject {
			// we're done
			return srcVolume, nil
		}

		// just change volume ownership
		// don't remove the source volume in case or err, because customer may
		// transfer the cloned volume afterwards
		return transferVolume(srcVolumeClient, dstVolumeClient, srcVolume)
	}

	defer func() {
		// it is safe to remove the cloned volume on exit
		if err := volumes.Delete(srcVolumeClient, srcVolume.ID, nil).ExtractErr(); err != nil {
			// it is fine, when the volume was already removed.
			if _, ok := err.(gophercloud.ErrDefault404); !ok {
				log.Printf("failed to delete a cloned volume: %s", err)
			}
		}
	}()

	// converting a volume to an image
	srcImage, err := volumeToImage(srcImageClient, srcVolumeClient, srcObjectClient, "", srcVolume)
	if err != nil {
		return nil, err
	}

	//
	volumeName := srcVolume.Name
	if toVolumeName != "" {
		volumeName = toVolumeName
	}
	volumeType := srcVolume.VolumeType
	if toVolumeType != "" {
		volumeType = toVolumeType
	}

	defer func() {
		// remove source region transition image
		if err := images.Delete(srcImageClient, srcImage.ID).ExtractErr(); err != nil {
			log.Printf("Failed to delete destination transition image: %s", err)
		}
	}()

	if !loc.SameRegion {
		// migrate the image/volume within different regions
		dstImage, err := migrateImage(srcImageClient, dstImageClient, srcObjectClient, dstObjectClient, srcImage, srcImage.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to migrate the image: %s", err)
		}
		defer func() {
			// remove destination region transition image
			if err := images.Delete(dstImageClient, dstImage.ID).ExtractErr(); err != nil {
				log.Printf("Failed to delete destination transition image: %s", err)
			}
		}()
		return imageToVolume(dstVolumeClient, dstImageClient, dstImage.ID, volumeName, srcVolume.Description, volumeType, az, srcVolume.Size, srcVolume)
	}

	// migrate the image/volume within the same region
	dstVolume, err := imageToVolume(srcVolumeClient, srcImageClient, srcImage.ID, volumeName, srcVolume.Description, volumeType, az, srcVolume.Size, srcVolume)
	if err != nil {
		return nil, err
	}

	if loc.SameProject {
		// we're done
		return dstVolume, nil
	}

	return transferVolume(srcVolumeClient, dstVolumeClient, dstVolume)
}

func imageToVolume(imgToVolClient, imgDstClient *gophercloud.ServiceClient, imageID, volumeName, volumeDescription, volumeType, az string, volumeSize int, srcVolume *volumes.Volume) (*volumes.Volume, error) {
	reauthClient(imgToVolClient, "imageToVolume")

	dstVolumeCreateOpts := volumes.CreateOpts{
		Size:             volumeSize,
		Name:             volumeName,
		Description:      volumeDescription,
		AvailabilityZone: az,
		ImageID:          imageID,
		VolumeType:       volumeType,
	}
	dstVolume, err := volumes.Create(imgToVolClient, dstVolumeCreateOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to create a destination volume: %s", err)
	}

	dstVolume, err = waitForVolume(imgToVolClient, dstVolume.ID, waitForVolumeSec)
	if err != nil {
		// TODO: delete volume?
		return nil, err
	}

	if srcVolume != nil && srcVolume.Bootable != "" && dstVolume.Bootable != srcVolume.Bootable {
		// when a non-bootable volume is created from a Glance image, it has a bootable flag set
		v, err := strconv.ParseBool(srcVolume.Bootable)
		if err != nil {
			log.Printf("Failed to parse %s to bool: %s", srcVolume.Bootable, err)
		} else {
			bootableOpts := volumeactions.BootableOpts{
				Bootable: v,
			}
			err = volumeactions.SetBootable(imgToVolClient, dstVolume.ID, bootableOpts).ExtractErr()
			if err != nil {
				log.Printf("Failed to update volume bootable options: %s", err)
			}
		}
	}

	createVolumeSpeed(dstVolume)

	// image can still be in "TODO" state, we need to wait for "available" before defer func will delete it
	_, err = waitForImage(imgDstClient, nil, imageID, 0, waitForImageSec)
	if err != nil {
		// TODO: delete volume?
		return nil, err
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
		return nil, fmt.Errorf("failed to create a %q volume transfer request: %s", srcVolume.ID, err)
	}

	_, err = volumetransfers.Accept(dstVolumeClient, transfer.ID, volumetransfers.AcceptOpts{AuthKey: transfer.AuthKey}).Extract()
	if err != nil {
		if err := volumetransfers.Delete(srcVolumeClient, transfer.ID).ExtractErr(); err != nil {
			log.Printf("Failed to delete a %q volume transfer request: %s", srcVolume.ID, err)
		}
		return nil, fmt.Errorf("failed to accept a %q volume transfer request: %s", srcVolume.ID, err)
	}

	return srcVolume, nil
}

// VolumeCmd represents the volume command
var VolumeCmd = &cobra.Command{
	Use:   "volume <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a volume",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		imageWebDownload = viper.GetBool("image-web-download")
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate volume

		volume := args[0]

		toAZ := viper.GetString("to-az")
		toVolumeName := viper.GetString("to-volume-name")
		toVolumeType := viper.GetString("to-volume-type")
		cloneViaSnapshot := viper.GetBool("clone-via-snapshot")

		// source and destination parameters
		loc, err := getSrcAndDst(toAZ)
		if err != nil {
			return err
		}

		srcProvider, err := newOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
		}

		srcImageClient, err := newGlanceV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source image client: %s", err)
		}

		srcVolumeClient, err := newBlockStorageV3Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source volume client: %s", err)
		}

		var srcObjectClient *gophercloud.ServiceClient
		if imageWebDownload {
			srcObjectClient, err = newObjectStorageV1Client(srcProvider, loc.Src.Region)
			if err != nil {
				return fmt.Errorf("failed to create source object storage client: %s", err)
			}
		}

		// resolve volume name to an ID
		if v, err := volumes_utils.IDFromName(srcVolumeClient, volume); err == nil {
			volume = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		dstProvider, err := newOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstImageClient, err := newGlanceV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination image client: %s", err)
		}

		dstVolumeClient, err := newBlockStorageV3Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination volume client: %s", err)
		}

		dstObjectClient, err := newObjectStorageV1Client(dstProvider, loc.Dst.Region)
		if err != nil {
			log.Printf("failed to create destination object storage client, detailed image clone statistics will be unavailable: %s", err)
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

		dstVolume, err := migrateVolume(srcImageClient, srcVolumeClient, srcObjectClient, dstImageClient, dstVolumeClient, dstObjectClient, srcVolume, toVolumeName, toVolumeType, toAZ, cloneViaSnapshot, loc)
		if err != nil {
			return err
		}

		log.Printf("Migrated target volume name is %q (id: %q) to %q availability zone", dstVolume.Name, dstVolume.ID, dstVolume.AvailabilityZone)

		return nil
	},
}

// VolumeToImageCmd represents the volume command
var VolumeToImageCmd = &cobra.Command{
	Use:   "to-image <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Upload a volume to an image",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// convert a volume to an image

		volume := args[0]

		toImageName := viper.GetString("to-image-name")
		cloneViaSnapshot := viper.GetBool("clone-via-snapshot")

		// source and destination parameters
		loc, err := getSrcAndDst("")
		if err != nil {
			return err
		}

		srcProvider, err := newOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
		}

		srcImageClient, err := newGlanceV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source image client: %s", err)
		}

		srcVolumeClient, err := newBlockStorageV3Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source volume client: %s", err)
		}

		srcObjectClient, err := newObjectStorageV1Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source object storage client: %s", err)
		}

		// resolve volume name to an ID
		if v, err := volumes_utils.IDFromName(srcVolumeClient, volume); err == nil {
			volume = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		srcVolume, err := waitForVolume(srcVolumeClient, volume, waitForVolumeSec)
		if err != nil {
			return fmt.Errorf("failed to wait for a %q volume: %s", volume, err)
		}

		var toAZ string
		err = checkAvailabilityZone(nil, srcVolume.AvailabilityZone, &toAZ, &loc)
		if err != nil {
			return err
		}

		defer measureTime()

		if srcVolume.Status == "in-use" {
			// clone the "in-use" volume
			newVolume, err := cloneVolume(srcVolumeClient, srcObjectClient, srcVolume, "", toAZ, cloneViaSnapshot, loc)
			if err != nil {
				return err
			}

			defer func() {
				if err := volumes.Delete(srcVolumeClient, newVolume.ID, nil).ExtractErr(); err != nil {
					log.Printf("Failed to delete a cloned volume: %s", err)
				}
			}()

			// volume was cloned, now we can safely convert it to a volume
			srcVolume = newVolume
		}

		dstImage, err := volumeToImage(srcImageClient, srcVolumeClient, srcObjectClient, toImageName, srcVolume)
		if err != nil {
			return err
		}

		log.Printf("Target image name is %q (id: %q)", dstImage.Name, dstImage.ID)

		return nil
	},
}

func init() {
	initVolumeCmdFlags()
	VolumeCmd.AddCommand(VolumeToImageCmd)
	RootCmd.AddCommand(VolumeCmd)
}

func initVolumeCmdFlags() {
	VolumeCmd.Flags().StringP("to-az", "", "", "destination volume availability zone")
	VolumeCmd.Flags().StringP("to-volume-name", "", "", "destination volume name")
	VolumeCmd.Flags().StringP("to-volume-type", "", "", "destination volume type")
	VolumeCmd.Flags().StringP("container-format", "", "bare", "image container format, when source volume doesn't have this info")
	VolumeCmd.Flags().StringP("disk-format", "", "vmdk", "image disk format, when source volume doesn't have this info")
	VolumeCmd.Flags().BoolP("clone-via-snapshot", "", false, "clone a volume via snapshot")

	VolumeToImageCmd.Flags().StringP("container-format", "", "bare", "image container format, when source volume doesn't have this info")
	VolumeToImageCmd.Flags().StringP("disk-format", "", "vmdk", "image disk format, when source volume doesn't have this info")
	VolumeToImageCmd.Flags().BoolP("clone-via-snapshot", "", false, "clone a volume via snapshot")
	VolumeToImageCmd.Flags().StringP("to-image-name", "", "", "destination image name")
}
