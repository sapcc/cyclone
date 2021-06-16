package pkg

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/attachinterfaces"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/availabilityzones"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/bootfromvolume"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/extendedserverattributes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/extendedstatus"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/startstop"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/volumeattach"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/pagination"
	servers_utils "github.com/gophercloud/utils/openstack/compute/v2/servers"
	networks_utils "github.com/gophercloud/utils/openstack/networking/v2/networks"
	subnets_utils "github.com/gophercloud/utils/openstack/networking/v2/subnets"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	waitForServerSec float64
	waitForPortSec   float64 = 60
)

type serverExtended struct {
	servers.Server
	availabilityzones.ServerAvailabilityZoneExt
	extendedserverattributes.ServerAttributesExt
	extendedstatus.ServerExtendedStatusExt
}

var serverNormalStates = []string{
	"active",
	"stopped",
}

var serverNormalStatuses = []string{
	"ACTIVE",
	"SHUTOFF",
}

var portNormalStatuses = []string{
	"ACTIVE",
	"DOWN",
}

var serverWaitStatuses = []string{
	"BUILD",
}

func createServerSpeed(server *serverExtended) {
	t := server.Updated.Sub(server.Created)
	log.Printf("Time to create a server: %s", t)
}

func waitForServer(client *gophercloud.ServiceClient, id string, secs float64) (*serverExtended, error) {
	var server serverExtended
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		var tmp serverExtended
		err = servers.Get(client, id).ExtractInto(&tmp)
		if err != nil {
			return false, err
		}
		// this is needed, because if new data contains a "null", the struct will contain an old data, e.g. `"OS-EXT-STS:task_state": null`
		server = tmp

		if !isSliceContainsStr(serverNormalStates, server.VmState) || server.TaskState != "" {
			log.Printf("Server status: %s (%s)", server.Status, joinSkipEmpty(", ", server.VmState, server.TaskState))
			return false, nil
		}

		log.Printf("Server status: %s", server.Status)
		if isSliceContainsStr(serverNormalStatuses, server.Status) {
			return true, nil
		}

		if !isSliceContainsStr(serverWaitStatuses, server.Status) {
			return false, fmt.Errorf("server status is %q", server.Status)
		}

		// continue status checks
		return false, nil
	})

	return &server, err
}

func waitForPort(client *gophercloud.ServiceClient, id string, secs float64) (*ports.Port, error) {
	var port *ports.Port
	var err error
	err = NewBackoff(int(secs), backoffFactor, backoffMaxInterval).WaitFor(func() (bool, error) {
		port, err = ports.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Port status: %s", port.Status)
		if isSliceContainsStr(portNormalStatuses, port.Status) {
			return true, nil
		}

		// continue status checks
		return false, nil
	})

	return port, err
}

func serverVolumeAttachments(client *gophercloud.ServiceClient, server *serverExtended) ([]string, bool, error) {
	allPages, err := volumeattach.List(client, server.ID).AllPages()
	if err != nil {
		return nil, false, fmt.Errorf("failed to list server volume attachments: %s", err)
	}

	volumes, err := volumeattach.ExtractVolumeAttachments(allPages)
	if err != nil {
		return nil, false, fmt.Errorf("failed to extract server volume attachments: %s", err)
	}

	tmp := make(map[string]string, len(volumes))
	keys := make([]string, len(volumes))
	vols := make([]string, len(volumes))

	bootableVolume := false
	re := regexp.MustCompile("/[a-z]{2}a$")
	for i, v := range volumes {
		// if server.Image is nil, then cinder volume is a bootable volume
		if !bootableVolume {
			if re.MatchString(v.Device) {
				if server.Image == nil {
					bootableVolume = true
				} else {
					imageID := server.Image["id"]
					log.Printf("Detected a bootable %q volume, but the server is booted from the local storage created from %q image", v.Device, imageID)
				}
			}
		}
		tmp[v.Device] = v.VolumeID
		keys[i] = v.Device
	}

	// sort by device name
	sort.Strings(keys)

	for i, k := range keys {
		vols[i] = tmp[k]
	}

	return vols, bootableVolume, nil
}

func createServerSnapshot(srcServerClient, srcImageClient, dstImageClient, srcObjectClient, dstObjectClient *gophercloud.ServiceClient, srcServer *serverExtended, loc Locations) (*images.Image, error) {
	createImageOpts := servers.CreateImageOpts{
		Name: fmt.Sprintf("%s-server-snapshot", srcServer.Name),
	}
	imageID, err := servers.CreateImage(srcServerClient, srcServer.ID, &createImageOpts).ExtractImageID()
	if err != nil {
		return nil, fmt.Errorf("failed to create a %q server snapshot", srcServer.Name)
	}

	deleteSourceOnReturn := func() {
		log.Printf("Removing source server snapshot image %q", imageID)
		if err := images.Delete(srcImageClient, imageID).ExtractErr(); err != nil {
			log.Printf("Error deleting source server snapshot: %s", err)
		}
	}

	var srcImage *images.Image
	srcImage, err = waitForImage(srcImageClient, srcObjectClient, imageID, 0, waitForImageSec)
	if err != nil {
		deleteSourceOnReturn()
		return nil, fmt.Errorf("failed to wait for a %q server snapshot image: %s", imageID, err)
	}

	if loc.SameProject {
		// if it is the same project, then there is no need to migrate
		return srcImage, nil
	}

	// now we can delete the source image on return
	defer deleteSourceOnReturn()

	// TODO: use the actual source image name from the server properties
	var dstImage *images.Image
	dstImage, err = migrateImage(srcImageClient, dstImageClient, srcObjectClient, dstObjectClient, srcImage, srcImage.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate server snapshot: %s", err)
	}

	// image can still be in "TODO" state, we need to wait for "available" before defer func will delete it
	_, err = waitForImage(srcImageClient, nil, imageID, 0, waitForImageSec)
	if err != nil {
		return nil, err
	}

	return dstImage, nil
}

func getPortOpts(client *gophercloud.ServiceClient, networkName, subnetName string) (*ports.CreateOpts, error) {
	// TODO: support multiple destination networks/subnets

	createOpts := &ports.CreateOpts{}

	if networkName == "" && subnetName == "" {
		b := false
		listOpts := external.ListOptsExt{
			ListOptsBuilder: networks.ListOpts{Status: "ACTIVE"},
			External:        &b,
		}

		allPages, err := networks.List(client, listOpts).AllPages()
		if err != nil {
			return nil, err
		}
		networks, err := networks.ExtractNetworks(allPages)
		if err != nil {
			return nil, err
		}

		if len(networks) == 0 {
			return nil, fmt.Errorf("target project doesn't contain networks")
		}

		if len(networks) > 1 {
			return nil, fmt.Errorf("target project has multiple networks, please specify a network name")
		}

		if len(networks[0].Subnets) == 0 {
			return nil, fmt.Errorf("target project has one private network, however it doesn't contain subntes")
		}

		if len(networks[0].Subnets) > 1 {
			return nil, fmt.Errorf("target project has one private network, however it contains multiple subntes, please specify a subnet name")
		}

		createOpts.NetworkID = networks[0].ID
		createOpts.FixedIPs = []ports.IP{
			{
				SubnetID: networks[0].Subnets[0],
			},
		}

		return createOpts, nil
	}

	if networkName != "" {
		networkID, err := networks_utils.IDFromName(client, networkName)
		if err != nil {
			return nil, err
		}
		network, err := networks.Get(client, networkID).Extract()
		if err != nil {
			return nil, err
		}
		createOpts.NetworkID = networkID

		if len(network.Subnets) == 0 {
			return nil, fmt.Errorf("target network doesn't have subnets")
		}
		if len(network.Subnets) > 1 {
			if subnetName == "" {
				return nil, fmt.Errorf("multiple subnets are attached to a network, please specify a subnet name")
			}

			listOpts := subnets.ListOpts{
				Name:      subnetName,
				NetworkID: networkID,
			}

			allPages, err := subnets.List(client, listOpts).AllPages()
			if err != nil {
				return nil, err
			}
			subnets, err := subnets.ExtractSubnets(allPages)
			if err != nil {
				return nil, err
			}

			if len(subnets) == 0 {
				return nil, fmt.Errorf("target project network doesn't contain subntes")
			}

			if len(subnets) > 1 {
				return nil, fmt.Errorf("target project network has one multiple subntes, please specify a subnet name")
			}

			createOpts.FixedIPs = []ports.IP{
				{
					SubnetID: subnets[0].ID,
				},
			}
		}

		return createOpts, nil
	}

	subnetID, err := subnets_utils.IDFromName(client, subnetName)
	if err != nil {
		return nil, err
	}
	subnet, err := subnets.Get(client, subnetID).Extract()
	if err != nil {
		return nil, err
	}

	createOpts.NetworkID = subnet.NetworkID
	createOpts.FixedIPs = []ports.IP{
		{
			SubnetID: subnetID,
		},
	}

	return createOpts, nil
}

func createServerPort(client *gophercloud.ServiceClient, networkName, subnetName string) (*ports.Port, error) {
	// TODO: support multiple destination networks/subnets

	createOpts, err := getPortOpts(client, networkName, subnetName)
	if err != nil {
		return nil, err
	}

	port, err := ports.Create(client, createOpts).Extract()
	if err != nil {
		return nil, fmt.Errorf("error creating destination server port: %s", err)
	}

	port, err = waitForPort(client, port.ID, waitForPortSec)
	if err != nil {
		if err := ports.Delete(client, port.ID).ExtractErr(); err != nil {
			log.Printf("Error deleting destination server port: %s", err)
		}
		return nil, fmt.Errorf("failed to wait for destination server port: %s", err)
	}

	return port, nil
}

func createServerOpts(srcServer *serverExtended, toServerName, flavorID, keyName, toAZ string, network servers.Network, dstVolumes []*volumes.Volume, dstImage *images.Image, bootableVolume bool, deleteVolOnTerm bool) servers.CreateOptsBuilder {
	serverName := toServerName
	if serverName == "" {
		// use original server name
		serverName = srcServer.Name
	}
	var createOpts servers.CreateOptsBuilder
	createOpts = &servers.CreateOpts{
		Name:             serverName,
		FlavorRef:        flavorID,
		AvailabilityZone: toAZ,
		Networks: []servers.Network{
			network,
		},
		Metadata: srcServer.Metadata,
		// TODO: security groups
		// TODO: userdata
		// TODO: tags
		// TODO: scheduler hints
	}

	var blockDeviceOpts []bootfromvolume.BlockDevice
	if dstImage != nil {
		if len(dstVolumes) > 0 {
			bd := bootfromvolume.BlockDevice{
				BootIndex:           0,
				UUID:                dstImage.ID,
				SourceType:          bootfromvolume.SourceImage,
				DestinationType:     bootfromvolume.DestinationLocal,
				DeleteOnTermination: deleteVolOnTerm,
			}
			blockDeviceOpts = append(blockDeviceOpts, bd)
		}
		createOpts.(*servers.CreateOpts).ImageRef = dstImage.ID
	}

	if keyName != "" {
		createOpts = &keypairs.CreateOptsExt{
			CreateOptsBuilder: createOpts,
			KeyName:           keyName,
		}
	}

	for i, v := range dstVolumes {
		bd := bootfromvolume.BlockDevice{
			BootIndex:       -1,
			UUID:            v.ID,
			SourceType:      bootfromvolume.SourceVolume,
			DestinationType: bootfromvolume.DestinationVolume,
		}
		if i == 0 && bootableVolume {
			bd.BootIndex = 0
			bd.DeleteOnTermination = deleteVolOnTerm
		}
		blockDeviceOpts = append(blockDeviceOpts, bd)
	}

	if len(blockDeviceOpts) > 0 {
		return &bootfromvolume.CreateOptsExt{
			CreateOptsBuilder: createOpts,
			BlockDevice:       blockDeviceOpts,
		}
	}

	return createOpts
}

func getFlavorFromName(client *gophercloud.ServiceClient, name string) (*flavors.Flavor, error) {
	allPages, err := flavors.ListDetail(client, nil).AllPages()
	if err != nil {
		return nil, err
	}

	all, err := flavors.ExtractFlavors(allPages)
	if err != nil {
		return nil, err
	}

	var flavor *flavors.Flavor
	count := 0
	for i, f := range all {
		if f.Name == name {
			count++
			flavor = &all[i]
		}
	}

	switch count {
	case 0:
		err := &gophercloud.ErrResourceNotFound{}
		err.ResourceType = "flavor"
		err.Name = name
		return nil, err
	case 1:
		return flavor, nil
	default:
		err := &gophercloud.ErrMultipleResourcesFound{}
		err.ResourceType = "flavor"
		err.Name = name
		err.Count = count
		return nil, err
	}
}

func checkFlavor(dstServerClient *gophercloud.ServiceClient, srcFlavor *flavors.Flavor, toFlavor *string) (*flavors.Flavor, error) {
	// TODO: compare an old flavor to a new one
	if *toFlavor == "" {
		flavor, err := getFlavorFromName(dstServerClient, srcFlavor.Name)
		if err != nil {
			return nil, fmt.Errorf("failed to find destination flavor name (%q): %s", *toFlavor, err)
		}
		*toFlavor = srcFlavor.Name
		return flavor, nil
	}

	flavor, err := getFlavorFromName(dstServerClient, *toFlavor)
	if err != nil {
		return nil, fmt.Errorf("failed to find destination flavor name (%q): %s", *toFlavor, err)
	}

	return flavor, nil
}

func getSrcFlavor(srcServerClient *gophercloud.ServiceClient, srcServer *serverExtended) (*flavors.Flavor, error) {
	if v, ok := srcServer.Flavor["id"]; ok {
		if v, ok := v.(string); ok {
			srcFlavor, err := flavors.Get(srcServerClient, v).Extract()
			if err != nil {
				return nil, fmt.Errorf("failed to get source server flavor details: %s", err)
			}
			return srcFlavor, nil
		}
	}
	return nil, fmt.Errorf("failed to detect source server flavor details")
}

func checKeyPair(client *gophercloud.ServiceClient, keyName string) error {
	if keyName == "" {
		return nil
	}

	return keypairs.List(client).EachPage(func(page pagination.Page) (bool, error) {
		keyPairs, err := keypairs.ExtractKeyPairs(page)
		if err != nil {
			return false, err
		}

		for _, keyPair := range keyPairs {
			if keyPair.Name == keyName {
				return true, nil
			}
		}

		return false, fmt.Errorf("%q key pair was not found", keyName)
	})
}

func getServerInterfaces(client *gophercloud.ServiceClient, id string) ([]attachinterfaces.Interface, error) {
	var interfaces []attachinterfaces.Interface

	pager := attachinterfaces.List(client, id)
	err := pager.EachPage(func(page pagination.Page) (bool, error) {
		s, err := attachinterfaces.ExtractInterfaces(page)
		if err != nil {
			return false, err
		}
		interfaces = append(interfaces, s...)
		return true, nil
	})
	if err != nil {
		return interfaces, err
	}

	return interfaces, nil
}

func getServerNetworkName(srcServerClient *gophercloud.ServiceClient, server *serverExtended) (string, error) {
	ifaces, err := getServerInterfaces(srcServerClient, server.ID)
	if err != nil {
		return "", fmt.Errorf("failed to detect source server interfaces: %s", err)
	}
	if len(ifaces) == 0 {
		return "", fmt.Errorf("source server contains no network interfaces")
	}

	for _, iface := range ifaces {
		for network, data := range server.Addresses {
			if v, ok := data.([]interface{}); ok {
				for _, v := range v {
					if v, ok := v.(map[string]interface{}); ok {
						if v, ok := v["OS-EXT-IPS-MAC:mac_addr"]; ok && v == iface.MACAddr {
							return network, nil
						}
					}
				}
			}
		}
	}

	return "", fmt.Errorf("failed to identify source server interface name")
}

func checkServerStatus(srcServerClient *gophercloud.ServiceClient, srcServer *serverExtended) error {
	if srcServer.Status == "ACTIVE" {
		if no {
			log.Printf("Skipping the VM shutdown")
			return nil
		}

		var ans string
		if !yes {
			fmt.Printf("It is recommended to shut down the VM before the migration\n")
			fmt.Printf("Do you want to shut down the VM? ([y]/n): ")
			var ans string
			_, err := fmt.Scan(&ans)
			if err != nil {
				return err
			}
			ans = strings.ToLower(strings.TrimSpace(ans))
		}

		if yes || ans == "y" || ans == "yes" {
			log.Printf("Shutting down the %q VM", srcServer.ID)
			err := startstop.Stop(srcServerClient, srcServer.ID).ExtractErr()
			if err != nil {
				return fmt.Errorf("failed to stop the %q source VM: %v", srcServer.ID, err)
			}
			srcServer, err = waitForServer(srcServerClient, srcServer.ID, waitForServerSec)
			if err != nil {
				return fmt.Errorf("failed to wait for %q source server: %s", srcServer.ID, err)
			}
		} else {
			log.Printf("Skipping the VM shutdown")
		}
	}

	return nil
}

func bootableToLocal(srcVolumeClient, srcImageClient, srcObjectClient, dstImageClient, dstObjectClient *gophercloud.ServiceClient, cloneViaSnapshot bool, toAZ string, loc Locations, flavor *flavors.Flavor, vols *[]string) (*images.Image, error) {
	log.Printf("Forcing the %q bootable volume to be a local disk", (*vols)[0])

	var err error
	var srcVolume, newVolume *volumes.Volume
	srcVolume, err = waitForVolume(srcVolumeClient, (*vols)[0], waitForVolumeSec)
	if err != nil {
		return nil, fmt.Errorf("failed to wait for a %q volume: %s", (*vols)[0], err)
	}

	if flavor.Disk < srcVolume.Size {
		return nil, fmt.Errorf("target %q flavor minimal disk size is less than a volume size: %d < %d", flavor.Name, flavor.Disk, srcVolume.Size)
	}

	// clone the in-use volume before creating its snapshot
	// it is impossible to convert a volume to a glance image, even with the force argument
	newVolume, err = cloneVolume(srcVolumeClient, srcObjectClient, srcVolume, "", toAZ, cloneViaSnapshot, loc)
	if err != nil {
		return nil, err
	}

	var srcImage, dstImage *images.Image
	// converting a volume to an image
	srcImage, err = volumeToImage(srcImageClient, srcVolumeClient, srcObjectClient, "", newVolume)
	// delete the cloned volume just after the image was created
	if err := volumes.Delete(srcVolumeClient, newVolume.ID, nil).ExtractErr(); err != nil {
		log.Printf("failed to delete a cloned volume: %s", err)
	}
	if err != nil {
		return nil, err
	}

	if loc.SameProject {
		dstImage = srcImage
	} else {
		// migrate the image/volume within different regions
		dstImage, err = migrateImage(srcImageClient, dstImageClient, srcObjectClient, dstObjectClient, srcImage, srcImage.Name)
		// remove source region transition image just after it was migrated
		if err := images.Delete(srcImageClient, srcImage.ID).ExtractErr(); err != nil {
			log.Printf("Failed to delete destination transition image: %s", err)
		}
		if err != nil {
			return nil, fmt.Errorf("failed to migrate the image: %s", err)
		}
	}

	// pop the bootable volume from the volume array
	if len(*vols) > 1 {
		*vols = (*vols)[1:]
	} else {
		*vols = nil
	}

	return dstImage, nil
}

// ServerCmd represents the server command
var ServerCmd = &cobra.Command{
	Use:   "server <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a server",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		imageWebDownload = viper.GetBool("image-web-download")
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// migrate server
		server := args[0]
		toName := viper.GetString("to-server-name")
		toKeyName := viper.GetString("to-key-name")
		toFlavor := viper.GetString("to-flavor-name")
		toNetworkName := viper.GetString("to-network-name")
		toSubnetName := viper.GetString("to-subnet-name")
		toAZ := viper.GetString("to-az")
		toVolumeType := viper.GetString("to-volume-type")
		cloneViaSnapshot := viper.GetBool("clone-via-snapshot")
		forceBootable := viper.GetUint("bootable-volume")
		forceLocal := viper.GetBool("local-disk")
		deleteVolOnTerm := viper.GetBool("delete-volume-on-termination")
		bootableDiskOnly := viper.GetBool("bootable-disk-only")
		skipServerCreation := viper.GetBool("skip-server-creation")

		if forceBootable > 0 && forceLocal {
			return fmt.Errorf("cannot use both --bootable-volume and --local-disk flags")
		}

		// source and destination parameters
		loc, err := getSrcAndDst(toAZ)
		if err != nil {
			return err
		}

		srcProvider, err := newOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
		}

		srcServerClient, err := newComputeV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source server client: %s", err)
		}

		srcImageClient, err := newGlanceV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source image client: %s", err)
		}

		var srcObjectClient *gophercloud.ServiceClient
		if imageWebDownload {
			srcObjectClient, err = newObjectStorageV1Client(srcProvider, loc.Src.Region)
			if err != nil {
				return fmt.Errorf("failed to create source object storage client: %s", err)
			}
		}

		srcVolumeClient, err := newBlockStorageV3Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source volume client: %s", err)
		}

		// resolve server name to an ID
		if v, err := servers_utils.IDFromName(srcServerClient, server); err == nil {
			server = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		dstProvider, err := newOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstServerClient, err := newComputeV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination server client: %s", err)
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

		dstNetworkClient, err := newNetworkV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination network client: %s", err)
		}

		srcServer, err := waitForServer(srcServerClient, server, waitForServerSec)
		if err != nil {
			return fmt.Errorf("failed to wait for %q source server: %s", server, err)
		}

		err = checkServerStatus(srcServerClient, srcServer)
		if err != nil {
			return err
		}

		// check server flavors
		srcFlavor, err := getSrcFlavor(srcServerClient, srcServer)
		if err != nil {
			return err
		}
		flavor, err := checkFlavor(dstServerClient, srcFlavor, &toFlavor)
		if err != nil {
			return err
		}

		// check availability zones
		err = checkAvailabilityZone(dstServerClient, srcServer.AvailabilityZone, &toAZ, &loc)
		if err != nil {
			return err
		}

		// check destintation server keypair name
		err = checKeyPair(dstServerClient, toKeyName)
		if err != nil {
			return err
		}

		// check destination network, create a port for a server
		var port *ports.Port
		var networkID string
		var network servers.Network
		// TODO: detect network settings from the source VM, when the same project is used
		// TODO: do this only when specific subnet name was provided, otherwise use auto-allocation

		if !skipServerCreation {
			if toSubnetName != "" {
				// TODO: a regular server deletion doesn't delete the port, find the way to hard bind server and port
				port, err = createServerPort(dstNetworkClient, toNetworkName, toSubnetName)
				if err != nil {
					return err
				}
				network.Port = port.ID
				defer func() {
					if err != nil {
						// delete the port only on error
						if err := ports.Delete(dstNetworkClient, port.ID).ExtractErr(); err != nil {
							log.Printf("Error deleting target server port: %s", err)
						}
					}
				}()
			} else {
				if toNetworkName == "" {
					log.Printf("New server network name is empty, detecting the network name from the source server")
					toNetworkName, err = getServerNetworkName(srcServerClient, srcServer)
					if err != nil {
						return err
					}
					log.Printf("Detected %q network name from the source server", toNetworkName)
				}
				networkID, err = networks_utils.IDFromName(dstNetworkClient, toNetworkName)
				if err != nil {
					return err
				}
				network.UUID = networkID
			}
		}

		defer measureTime()

		var vols []string
		var bootableVolume bool
		vols, bootableVolume, err = serverVolumeAttachments(srcServerClient, srcServer)
		if err != nil {
			return fmt.Errorf("failed to detect server volume attachments: %s", err)
		}

		log.Printf("Detected %q attached volumes", vols)
		if bootableDiskOnly && len(vols) > 1 {
			if bootableVolume {
				vols := vols[:1]
				log.Printf("Processing only the bootable disk: %s", vols)
			} else {
				vols = nil
				log.Printf("Processing only the local disk")
			}
		}

		var dstVolumes []*volumes.Volume
		var dstImage *images.Image
		if bootableVolume {
			log.Printf("The %q volume is a bootable volume", vols[0])

			if forceLocal {
				dstImage, err = bootableToLocal(srcVolumeClient, srcImageClient, srcObjectClient, dstImageClient, dstObjectClient, cloneViaSnapshot, toAZ, loc, flavor, &vols)
				if err != nil {
					return err
				}

				// TODO: add an option to keep artifacts on failure
				dstImageID := dstImage.ID
				if !skipServerCreation {
					defer func() {
						if err := images.Delete(dstImageClient, dstImageID).ExtractErr(); err != nil {
							log.Printf("Error deleting migrated server snapshot: %s", err)
						}
					}()
				}

				// set bootable volume flag to false, because we set a proper dstImage var
				bootableVolume = false
			}
		} else {
			if forceBootable > 0 && uint(srcFlavor.Disk) > forceBootable {
				return fmt.Errorf("cannot create a bootable volume with a size less than original disk size: %d", srcFlavor.Disk)
			}

			// TODO: image name must represent the original server source image name
			dstImage, err = createServerSnapshot(srcServerClient, srcImageClient, dstImageClient, srcObjectClient, dstObjectClient, srcServer, loc)
			if err != nil {
				return err
			}

			// TODO: add an option to keep artifacts on failure
			dstImageID := dstImage.ID
			if !skipServerCreation || forceBootable > 0 {
				defer func() {
					if err := images.Delete(dstImageClient, dstImageID).ExtractErr(); err != nil {
						log.Printf("Error deleting migrated server snapshot: %s", err)
					}
				}()
			}

			if forceBootable > 0 {
				if uint(srcFlavor.Disk) > forceBootable {
					return fmt.Errorf("cannot create a bootable volume with a size less than original disk size: %d", srcFlavor.Disk)
				}
				log.Printf("Forcing %s image to be converted to a bootable volume", dstImageID)
				bootableVolume = true
				var newBootableVolume *volumes.Volume
				newBootableVolume, err = imageToVolume(dstVolumeClient, dstImageClient, dstImage.ID, "", fmt.Sprintf("bootable for %s", dstImage.Name), "", toAZ, int(forceBootable), nil)
				if err != nil {
					return fmt.Errorf("failed to create a bootable volume for a VM: %s", err)
				}
				dstVolumes = append(dstVolumes, newBootableVolume)

				log.Printf("Cloned %q server local storage to %q volume in %q availability zone", srcServer.ID, newBootableVolume.ID, toAZ)

				// release dstImage pointer
				dstImage = nil
			}
		}

		for i, v := range vols {
			var srcVolume, dstVolume *volumes.Volume
			srcVolume, err = waitForVolume(srcVolumeClient, v, waitForVolumeSec)
			if err != nil {
				return fmt.Errorf("failed to wait for a %q volume: %s", v, err)
			}

			dstVolume, err = migrateVolume(srcImageClient, srcVolumeClient, srcObjectClient, dstImageClient, dstVolumeClient, dstObjectClient, srcVolume, srcVolume.Name, toVolumeType, toAZ, cloneViaSnapshot, loc)
			if err != nil {
				// if we don't fail here, then the resulting VM may not boot because of insuficient of volumes
				return fmt.Errorf("failed to clone the %q volume: %s", srcVolume.ID, err)
			}

			dstVolumes = append(dstVolumes, dstVolume)
			// when destination availability zone is not specified, then we pick one of the target volume
			if toAZ == "" {
				toAZ = dstVolumes[i].AvailabilityZone
			}
			log.Printf("Cloned %q volume to %q volume in %q availability zone", srcVolume.ID, dstVolume.ID, toAZ)
			// TODO: defer delete volumes on failure?
		}

		if skipServerCreation {
			log.Printf("Server artifacts were cloned to %q availability zone", toAZ)
			return nil
		}

		createOpts := createServerOpts(srcServer, toName, flavor.ID, toKeyName, toAZ, network, dstVolumes, dstImage, bootableVolume, deleteVolOnTerm)
		dstServer := new(serverExtended)
		err = servers.Create(dstServerClient, createOpts).ExtractInto(dstServer)
		if err != nil {
			return fmt.Errorf("failed to create a destination server: %s", err)
		}

		dstServer, err = waitForServer(dstServerClient, dstServer.ID, waitForServerSec)
		if err != nil {
			// nil an error and don't delete the port
			retErr := fmt.Errorf("failed to wait for %q target server: %s", dstServer.ID, err)
			err = nil
			return retErr
		}

		createServerSpeed(dstServer)

		if dstImage != nil {
			// image can still be in "TODO" state, we need to wait for "available" before defer func will delete it
			if _, err := waitForImage(dstImageClient, nil, dstImage.ID, 0, waitForImageSec); err != nil {
				log.Printf("Error waiting for %q image: %s", dstImage.ID, err)
			}
		}

		log.Printf("Server cloned to %q (%q) using %s flavor to %q availability zone", dstServer.Name, dstServer.ID, toFlavor, dstServer.AvailabilityZone)
		if port != nil {
			log.Printf("The %q port in the %q subnet was created", port.ID, toSubnetName)
		}

		return err
	},
}

func init() {
	initServerCmdFlags()
	RootCmd.AddCommand(ServerCmd)
}

func initServerCmdFlags() {
	ServerCmd.Flags().StringP("to-server-name", "", "", "destination server name")
	ServerCmd.Flags().StringP("to-key-name", "", "", "destination server key name")
	ServerCmd.Flags().StringP("to-flavor-name", "", "", "destination server flavor name")
	ServerCmd.Flags().StringP("to-network-name", "", "", "destination server network name")
	ServerCmd.Flags().StringP("to-subnet-name", "", "", "destination server subnet name")
	ServerCmd.Flags().StringP("to-az", "", "", "destination availability zone")
	ServerCmd.Flags().StringP("to-volume-type", "", "", "destination volume type")
	ServerCmd.Flags().StringP("container-format", "", "bare", "image container format, when source volume doesn't have this info")
	ServerCmd.Flags().StringP("disk-format", "", "vmdk", "image disk format, when source volume doesn't have this info")
	ServerCmd.Flags().BoolP("clone-via-snapshot", "", false, "clone a volume, attached to a server, via snapshot")
	ServerCmd.Flags().UintP("bootable-volume", "b", 0, "force a VM with a local storage to be cloned to a VM with a bootable volume with a size specified in GiB")
	ServerCmd.Flags().BoolP("local-disk", "", false, "convert the attached bootable volume to a local disk")
	ServerCmd.Flags().BoolP("delete-volume-on-termination", "", true, "specifies whether or not to delete the attached bootable volume when the server is terminated")
	ServerCmd.Flags().BoolP("bootable-disk-only", "", false, "clone only the bootable disk/volume, skipping the rest attached volumes")
	ServerCmd.Flags().BoolP("skip-server-creation", "", false, "skip server creation, clone only server's artifacts: image and volumes")
}
