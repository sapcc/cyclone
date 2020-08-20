package pkg

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"time"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/imagedata"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/imageimport"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/tasks"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/containers"
	"github.com/gophercloud/gophercloud/openstack/objectstorage/v1/objects"
	"github.com/gophercloud/gophercloud/pagination"
	images_utils "github.com/gophercloud/utils/openstack/imageservice/v2/images"
	"github.com/machinebox/progress"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	waitForImageSec  float64
	swiftTempURLTTL  int = 10 // 10 seconds is enough
	imageWebDownload bool
)

var imageWaitStatuses = []string{
	string(images.ImageStatusSaving),
	string(images.ImageStatusQueued),
	string(images.ImageStatusImporting),
}

func createImageSpeed(image *images.Image) {
	t := image.UpdatedAt.Sub(image.CreatedAt)
	log.Printf("Time to create a image: %s", t)
	size := float64(image.SizeBytes / (1024 * 1024))
	log.Printf("Size of the image: %.2f Mb", size)
	log.Printf("Speed of the image creation: %.2f Mb/sec", size/t.Seconds())
}

func waitForImageTask(client, swiftClient *gophercloud.ServiceClient, id string, srcSizeBytes int64, secs float64) (*images.Image, error) {
	// initial image status
	img, err := images.Get(client, id).Extract()
	if err != nil {
		return nil, err
	}

	updateStatus := func(task tasks.Task) (bool, error) {
		var err error
		if task.Status == string(tasks.TaskStatusSuccess) {
			// update image status
			img, err = images.Get(client, id).Extract()
			if err != nil {
				return false, err
			}
		}
		return false, nil
	}

	var taskID string
	err = NewArithmeticBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		var taskStatus string
		err = tasks.List(client, tasks.ListOpts{}).EachPage(func(page pagination.Page) (bool, error) {
			tl, err := tasks.ExtractTasks(page)
			if err != nil {
				return false, fmt.Errorf("failed to list image tasks: %s", err)
			}

			for _, task := range tl {
				if taskID != "" && task.Status != string(tasks.TaskStatusFailure) {
					taskStatus = fmt.Sprintf("Target image task status is: %s", task.Status)
					return updateStatus(task)
				}

				tid := task.ID
				if taskID != "" {
					// we know the task ID
					tid = taskID
				}

				t, err := tasks.Get(client, tid).Extract()
				if err != nil {
					// TODO: return an error?
					log.Printf("Failed to get %q task details: %s", tid, err)
					return false, nil
				}

				if v, ok := t.Input["image_id"]; ok {
					if v, ok := v.(string); ok {
						if v == id {
							taskStatus = fmt.Sprintf("Target image task status is: %s", t.Status)

							// save the correcsponding task id for next calls
							taskID = t.ID
							if t.Status == string(tasks.TaskStatusFailure) {
								// set failed image status
								img.Status = images.ImageStatus(t.Status)
								return false, fmt.Errorf("target image import failed: %s", t.Message)
							}
							return updateStatus(*t)
						}
					}
				}
			}

			// continue listing
			return true, nil
		})

		if err != nil {
			return false, err
		}

		// show user friendly status
		containerSize := getContainerSize(swiftClient, id, srcSizeBytes)
		if containerSize == "" {
			log.Printf("Target image status: %s, %s", img.Status, taskStatus)
		} else {
			log.Printf("Target image status: %s, %s, %s", img.Status, taskStatus, containerSize)
		}

		if img.Status == images.ImageStatusActive {
			return true, nil
		} else {
			// continue status checks
			return false, nil
		}
	})

	return img, err
}

// this function may show confused size results due to Swift eventual consistency
func getContainerSize(client *gophercloud.ServiceClient, id string, srcSizeBytes int64) string {
	if client != nil {
		container, err := containers.Get(client, "glance_"+id, nil).Extract()
		if err != nil {
			if _, ok := err.(gophercloud.ErrDefault404); !ok {
				log.Printf("Failed to get Swift container status: %s", err)
			}
			return ""
		}

		var containerSize, percent int64
		if container != nil {
			containerSize = container.BytesUsed
		}

		if srcSizeBytes > 0 {
			percent = 100 * containerSize / srcSizeBytes
			return fmt.Sprintf("image size: %d/%d (%d%%)", containerSize, srcSizeBytes, percent)
		}

		// container size in Mb
		return fmt.Sprintf("image size: %.2f Mb", float64(containerSize/(1024*1024)))
	}
	return ""
}

func waitForImage(client, swiftClient *gophercloud.ServiceClient, id string, srcSizeBytes int64, secs float64) (*images.Image, error) {
	var image *images.Image
	var err error
	err = NewArithmeticBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		image, err = images.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		// show user friendly status
		containerSize := getContainerSize(swiftClient, id, srcSizeBytes)
		if containerSize == "" {
			log.Printf("Transition image status: %s", image.Status)
		} else {
			log.Printf("Transition image status: %s, %s", image.Status, containerSize)
		}
		if image.Status == images.ImageStatusActive {
			return true, nil
		}

		if !isSliceContainsStr(imageWaitStatuses, string(image.Status)) {
			return false, fmt.Errorf("transition image status is %q", image.Status)
		}

		// continue status checks
		return false, nil
	})

	return image, err
}

var skipImageAttributes = []string{
	"direct_url",
	"boot_roles",
	"os_hash_algo",
	"os_hash_value",
}

func expandImageProperties(v map[string]interface{}) map[string]string {
	properties := map[string]string{}
	for key, value := range v {
		if isSliceContainsStr(skipImageAttributes, key) {
			continue
		}
		if v, ok := value.(string); ok && v != "" {
			properties[key] = v
		}
	}

	return properties
}

func generateTmpUrlKey(n int) string {
	var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")

	rand.Seed(time.Now().UnixNano())

	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func migrateImage(srcImageClient, dstImageClient, srcObjectClient, dstObjectClient *gophercloud.ServiceClient, srcImg *images.Image, toImageName string) (*images.Image, error) {
	var url string
	containerName := "glance_" + srcImg.ID
	objectName := srcImg.ID

	if imageWebDownload {
		tempUrlKey := containers.UpdateOpts{
			TempURLKey: generateTmpUrlKey(20),
		}
		_, err := containers.Update(srcObjectClient, containerName, tempUrlKey).Extract()
		if err != nil {
			return nil, fmt.Errorf("unable to set container temporary url key: %s", err)
		}

		tmpUrlOptions := objects.CreateTempURLOpts{
			Method: "GET",
			TTL:    swiftTempURLTTL,
		}

		url, err = objects.CreateTempURL(srcObjectClient, containerName, objectName, tmpUrlOptions)
		if err != nil {
			return nil, fmt.Errorf("unable to generate a temporary url for the %q container: %s", containerName, err)
		}

		log.Printf("Generated Swift Temp URL: %s", url)
	}

	imageName := srcImg.Name
	if toImageName != "" {
		imageName = toImageName
	}

	// create an empty image
	visibility := images.ImageVisibilityPrivate
	createOpts := &images.CreateOpts{
		Name:            imageName,
		ContainerFormat: srcImg.ContainerFormat,
		DiskFormat:      srcImg.DiskFormat,
		MinDisk:         srcImg.MinDiskGigabytes,
		MinRAM:          srcImg.MinRAMMegabytes,
		Visibility:      &visibility,
		Properties:      expandImageProperties(srcImg.Properties),
	}

	dstImg, err := images.Create(dstImageClient, createOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("error creating destination Image: %s", err)
	}

	defer func() {
		if err != nil {
			log.Printf("Deleting target %q image", dstImg.ID)
			if err := images.Delete(dstImageClient, dstImg.ID).ExtractErr(); err != nil {
				log.Printf("Error deleting target image: %s", err)
			}
		}
	}()

	if imageWebDownload {
		if !isSliceContainsStr(dstImg.OpenStackImageImportMethods, string(imageimport.WebDownloadMethod)) {
			return nil, fmt.Errorf("the %q import method is not supported, supported import methods: %q", imageimport.WebDownloadMethod, dstImg.OpenStackImageImportMethods)
		}

		// import
		importOpts := &imageimport.CreateOpts{
			Name: imageimport.WebDownloadMethod,
			URI:  url,
		}

		err = imageimport.Create(dstImageClient, dstImg.ID, importOpts).ExtractErr()
		if err != nil {
			return nil, fmt.Errorf("error while importing url %q: %s", url, err)

		}

		dstImg, err = waitForImageTask(dstImageClient, dstObjectClient, dstImg.ID, srcImg.SizeBytes, waitForImageSec)
		if err != nil {
			return nil, fmt.Errorf("error while importing url %q: %s", url, err)
		}
	} else {
		// get the source reader
		var imageReader io.ReadCloser
		imageReader, err = imagedata.Download(srcImageClient, srcImg.ID).Extract()
		if err != nil {
			return nil, fmt.Errorf("error getting the source image reader: %s", err)
		}

		progressReader := progress.NewReader(imageReader)
		go func() {
			for p := range progress.NewTicker(context.Background(), progressReader, srcImg.SizeBytes, 1*time.Second) {
				log.Printf("Image size: %d/%d (%.2f%%), remaining: %s", p.N(), p.Size(), p.Percent(), p.Remaining().Round(time.Second))
			}
		}()

		// write the source to the destination
		err = imagedata.Upload(dstImageClient, dstImg.ID, progressReader).ExtractErr()
		if err != nil {
			return nil, fmt.Errorf("failed to upload an image: %s", err)
		}
		imageReader.Close()

		dstImg, err = waitForImage(dstImageClient, dstObjectClient, dstImg.ID, srcImg.SizeBytes, waitForImageSec)
		if err != nil {
			return nil, fmt.Errorf("error while waiting for an image to be uploaded: %s", err)
		}
	}

	createImageSpeed(dstImg)

	log.Printf("Migrated target image name is %q (id: %q)", dstImg.Name, dstImg.ID)

	// verify destination image size and hash
	if srcImg.SizeBytes != dstImg.SizeBytes {
		return dstImg, fmt.Errorf("image was migrated, but the source size doesn't correspond the destination size: %d != %d", srcImg.SizeBytes, dstImg.SizeBytes)
	}

	if srcImg.Checksum != dstImg.Checksum {
		return dstImg, fmt.Errorf("image was migrated, but the source checksum doesn't correspond the destination checksum: %s != %s", srcImg.Checksum, dstImg.Checksum)
	}

	if srcImg.Properties["os_hash_algo"] != dstImg.Properties["os_hash_algo"] {
		return dstImg, fmt.Errorf("image was migrated, but the source hash also doesn't correspond the destination hash algo: %s != %s", srcImg.Properties["os_hash_algo"], dstImg.Properties["os_hash_algo"])
	}

	if srcImg.Properties["os_hash_value"] != dstImg.Properties["os_hash_value"] {
		return dstImg, fmt.Errorf("image was migrated, but the source hash doesn't correspond the destination hash: %s != %s", srcImg.Properties["os_hash_value"], dstImg.Properties["os_hash_value"])
	}

	return dstImg, nil
}

// ImageCmd represents the image command
var ImageCmd = &cobra.Command{
	Use:   "image <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone an image",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		imageWebDownload = viper.GetBool("image-web-download")
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate image
		image := args[0]
		toName := viper.GetString("to-image-name")

		// source and destination parameters
		loc, err := getSrcAndDst("")
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

		var srcObjectClient *gophercloud.ServiceClient
		if imageWebDownload {
			srcObjectClient, err = NewObjectStorageV1Client(srcProvider, loc.Src.Region)
			if err != nil {
				return fmt.Errorf("failed to create source object storage client: %s", err)
			}
		}

		// resolve image name to an ID
		if v, err := images_utils.IDFromName(srcImageClient, image); err == nil {
			image = v
		}

		dstProvider, err := NewOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstImageClient, err := NewGlanceV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination image client: %s", err)
		}

		dstObjectClient, err := NewObjectStorageV1Client(dstProvider, loc.Dst.Region)
		if err != nil {
			log.Printf("failed to create destination object storage client, detailed image clone statistics will be unavailable: %s", err)
		}

		srcImg, err := waitForImage(srcImageClient, nil, image, 0, waitForImageSec)
		if err != nil {
			return fmt.Errorf("failed to wait for %q source image: %s", image, err)
		}

		if imageWebDownload {
			// check whether current user scope belongs to the image owner
			userProjectID, err := getAuthProjectID(srcImageClient.ProviderClient)
			if err != nil {
				return fmt.Errorf("failed to extract user project ID scope: %s", err)
			}
			if userProjectID != srcImg.Owner {
				return fmt.Errorf("cannot clone an image using web download import method, when an image belongs to another project (%s), try to set --image-web-download=false", srcImg.Owner)
			}
		}

		defer measureTime()

		_, err = migrateImage(srcImageClient, dstImageClient, srcObjectClient, dstObjectClient, srcImg, toName)

		return err
	},
}

func init() {
	initImageCmdFlags()
	RootCmd.AddCommand(ImageCmd)
}

func initImageCmdFlags() {
	ImageCmd.Flags().StringP("to-image-name", "", "", "destination image name")
}
