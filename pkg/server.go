package pkg

import (
	"fmt"
	"regexp"
	"sort"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/v3/volumes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/availabilityzones"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/bootfromvolume"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/extendedserverattributes"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/extendedstatus"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/keypairs"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/extensions/volumeattach"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/flavors"
	"github.com/gophercloud/gophercloud/openstack/compute/v2/servers"
	"github.com/gophercloud/gophercloud/openstack/imageservice/v2/images"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/extensions/external"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/networks"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/ports"
	"github.com/gophercloud/gophercloud/openstack/networking/v2/subnets"
	"github.com/gophercloud/gophercloud/pagination"
	flavors_utils "github.com/gophercloud/utils/openstack/compute/v2/flavors"
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

var serverWaitStatuses = []string{
	string("BUILD"),
}

func createServerSpeed(server *serverExtended) {
	t := server.Updated.Sub(server.Created)
	log.Printf("Time to create a server: %s", t)
}

func waitForServer(client *gophercloud.ServiceClient, id string, secs float64) (*serverExtended, error) {
	var server serverExtended
	var err error
	err = gophercloud.WaitFor(int(secs), func() (bool, error) {
		var tmp serverExtended
		err = servers.Get(client, id).ExtractInto(&tmp)
		if err != nil {
			return false, err
		}
		// this is needed, because if new data contains a "null", the struct will contain an old data, e.g. `"OS-EXT-STS:task_state": null`
		server = tmp

		if server.VmState != "active" {
			if server.TaskState != "" {
				log.Printf("Server status: %s (%s, %s)", server.Status, server.VmState, server.TaskState)
				return false, nil
			}
			log.Printf("Server status: %s (%s)", server.Status, server.VmState)
			return false, nil
		}

		log.Printf("Server status: %s", server.Status)
		if server.Status == "ACTIVE" {
			return true, nil
		}

		if !isSliceContainsStr(serverWaitStatuses, string(server.Status)) {
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
	err = gophercloud.WaitFor(int(secs), func() (bool, error) {
		port, err = ports.Get(client, id).Extract()
		if err != nil {
			return false, err
		}

		log.Printf("Port status: %s", port.Status)
		if port.Status == "ACTIVE" || port.Status == "DOWN" {
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
	for i, v := range volumes {
		// if server.Image is nil, then cinder volume is a bootable volume
		if !bootableVolume {
			if ok, _ := regexp.MatchString("/[a-z]{2}a$", v.Device); ok {
				if server.Image == nil {
					bootableVolume = true
				} else {
					imageID, _ := server.Image["id"]
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

func createServerSnapshot(srcServerClient, srcImageClient, dstImageClient, srcObjectClient *gophercloud.ServiceClient, srcServer *serverExtended, loc Locations) (*images.Image, error) {
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
	srcImage, err = waitForImage(srcImageClient, imageID, waitForImageSec)
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
	dstImage, err = migrateImage(srcImageClient, dstImageClient, srcObjectClient, srcImage, srcImage.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to migrate server snapshot: %s", err)
	}

	// image can still be in "TODO" state, we need to wait for "available" before defer func will delete it
	_, err = waitForImage(srcImageClient, imageID, waitForImageSec)
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

func createServerOpts(srcServer *serverExtended, toServerName, flavorID, keyName, toAZ string, network servers.Network, dstVolumes []*volumes.Volume, dstImage *images.Image, bootableVolume bool) servers.CreateOptsBuilder {
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
		// TODO: security groups
		// TODO: metadata
		// TODO: userdata
		// TODO: tags
		// TODO: scheduler hints
	}

	var blockDeviceOpts []bootfromvolume.BlockDevice
	if dstImage != nil {
		if len(dstVolumes) > 0 {
			bd := bootfromvolume.BlockDevice{
				UUID:                dstImage.ID,
				SourceType:          bootfromvolume.SourceImage,
				DestinationType:     bootfromvolume.DestinationLocal,
				BootIndex:           0,
				DeleteOnTermination: true,
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
			bd.DeleteOnTermination = true
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

func checkFlavor(srcServerClient, dstServerClient *gophercloud.ServiceClient, srcServer *serverExtended, toFlavor *string) (string, error) {
	// TODO: compare an old flavor to a new one
	if *toFlavor == "" {
		if v, ok := srcServer.Flavor["id"]; ok {
			if v, ok := v.(string); ok {
				flavor, err := flavors.Get(srcServerClient, v).Extract()
				if err != nil {
					return "", fmt.Errorf("failed to get an info about the source server flavor: %s", err)
				}
				flavorID, err := flavors_utils.IDFromName(dstServerClient, flavor.Name)
				if err != nil {
					return "", fmt.Errorf("failed to find destination flavor name (%q): %s", *toFlavor, err)
				}
				*toFlavor = flavor.Name
				return flavorID, nil
			}
		} else {
			return "", fmt.Errorf("failed to detect source server flavor")
		}
	}

	flavorID, err := flavors_utils.IDFromName(dstServerClient, *toFlavor)
	if err != nil {
		return "", fmt.Errorf("failed to find destination flavor name (%q): %s", *toFlavor, err)
	}

	return flavorID, nil
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

// ServerCmd represents the server command
var ServerCmd = &cobra.Command{
	Use:   "server <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a server",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
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

		srcServerClient, err := NewComputeV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source server client: %s", err)
		}

		srcImageClient, err := NewGlanceV2Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source image client: %s", err)
		}

		srcObjectClient, err := NewObjectStorageV1Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source object storage client: %s", err)
		}

		srcVolumeClient, err := NewBlockStorageV3Client(srcProvider, loc.Src.Region)
		if err != nil {
			return fmt.Errorf("failed to create source volume client: %s", err)
		}

		// resolve server name to an ID
		if v, err := servers_utils.IDFromName(srcServerClient, server); err == nil {
			server = v
		}

		dstProvider, err := NewOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstServerClient, err := NewComputeV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination server client: %s", err)
		}

		dstImageClient, err := NewGlanceV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination image client: %s", err)
		}

		dstVolumeClient, err := NewBlockStorageV3Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination volume client: %s", err)
		}

		dstNetworkClient, err := NewNetworkV2Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination network client: %s", err)
		}

		srcServer, err := waitForServer(srcServerClient, server, waitForServerSec)
		if err != nil {
			return fmt.Errorf("failed to wait for %q source server: %s", server, err)
		}

		// check server flavors
		flavorID, err := checkFlavor(srcServerClient, dstServerClient, srcServer, &toFlavor)
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
			networkID, err = networks_utils.IDFromName(dstNetworkClient, toNetworkName)
			if err != nil {
				return err
			}
			network.UUID = networkID
		}

		defer measureTime()

		var vols []string
		var bootableVolume bool
		vols, bootableVolume, err = serverVolumeAttachments(srcServerClient, srcServer)
		if err != nil {
			return fmt.Errorf("failed to detect server volume attachments: %s", err)
		}

		log.Printf("Detected %q attached volumes", vols)
		//log.Printf("The %q volume is a bootable volume", vols[0])

		var dstImage *images.Image
		if bootableVolume {
			log.Printf("The %q volume is a bootable volume", vols[0])
		} else {
			// TODO: image name must represent the original server source image name
			dstImage, err = createServerSnapshot(srcServerClient, srcImageClient, dstImageClient, srcObjectClient, srcServer, loc)
			if err != nil {
				return err
			}

			// TODO: add an option to keep artifacts on failure
			defer func() {
				if err := images.Delete(dstImageClient, dstImage.ID).ExtractErr(); err != nil {
					log.Printf("Error deleting migrated server snapshot: %s", err)
				}
			}()
		}

		var dstVolumes []*volumes.Volume
		for i, v := range vols {
			var srcVolume, dstVolume *volumes.Volume
			srcVolume, err = waitForVolume(srcVolumeClient, v, waitForVolumeSec)
			if err != nil {
				return fmt.Errorf("failed to wait for a %q volume: %s", v, err)
			}

			dstVolume, err = migrateVolume(srcImageClient, srcVolumeClient, srcObjectClient, dstImageClient, dstVolumeClient, srcVolume, srcVolume.Name, toAZ, cloneViaSnapshot, loc)
			if err != nil {
				// if we don't fail here, then the resulting VM may not boot because of insuficient of volumes
				return fmt.Errorf("Failed to clone the %q volume: %s", srcVolume.ID, err)
			}

			dstVolumes = append(dstVolumes, dstVolume)
			// when destination availability zone is not specified, then we pick one of the target volume
			if toAZ == "" {
				toAZ = dstVolumes[i].AvailabilityZone
			}
			// TODO: defer delete volumes on failure?
		}

		createOpts := createServerOpts(srcServer, toName, flavorID, toKeyName, toAZ, network, dstVolumes, dstImage, bootableVolume)
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
			if _, err := waitForImage(dstImageClient, dstImage.ID, waitForImageSec); err != nil {
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
	ServerCmd.Flags().StringP("container-format", "", "bare", "image container format, when source volume doesn't have this info")
	ServerCmd.Flags().StringP("disk-format", "", "vmdk", "image disk format, when source volume doesn't have this info")
	ServerCmd.Flags().BoolP("clone-via-snapshot", "", false, "clone a volume, attached to a server, via snapshot")
}
