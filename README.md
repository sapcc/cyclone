# cyclone

Clone OpenStack entities easily.

## Why?

In modern clusters compute instances are considered as Cattles, but there are exceptions, when a compute instance is a Pet and needs care, especially when you need to migrate or clone it to a new OpenStack region or availability zone.

Here comes cyclone (**C**loud **Clone** or cclone) to help you with this task. It takes care about all volumes attached to a VM and clones them with all required intermediate type conversions.

## Help

By default the tool uses a Glance V2 [web-download](https://docs.openstack.org/glance/latest/admin/interoperable-image-import.html#image-import-methods) method to clone images. This method allows Glance to download an image using a remote URL. Set the `--image-web-download` option to **false** to use the default download/upload method. In this case the whole image data will be streamed through cyclone.

A remote URL can be generated using a Swift [Temporary URL](https://docs.openstack.org/swift/latest/api/temporary_url_middleware.html).

A volume migration is performed by converting a volume to an image and further image migration between regions, then converting an image back to a volume.

A volume migration within the same region is performed using a [Volume Transfer](https://docs.openstack.org/cinder/latest/cli/cli-manage-volumes.html#transfer-a-volume) method.

By default the tool uses the same credentials from environment variables for the source and destination projects, but for the destination you can define different region, domain and project name. It is also possible to override destination credentials via OpenStack environment variables with the `TO_` prefix or via CLI parameters.

~> **Note:** Be aware about the quota, especially the source project quota, when cloning a volume. It requires up to 2x source volume size Cinder (Block Storage) quota. If a `--clone-via-snapshot` flag is specified, the quota requirement increases up to 3x source volume size.

~> **Note:** Cloning a volume within the same region, but different availability zones requires an extra Swift storage quota. If you don't have an ability to use Swift in this case, you can specify a `--clone-via-snapshot` flag.

~> **Note:** It is strongly recommended to shut down the VM before you start a migration of a VM or its volumes.

~> **Note:** By default cyclone writes all OpenStack request/response logs into a `cyclone` directory, located in System Temporary Directory. Define `-d` or `--debug` flag if you want to see these logs in console output.

```sh
Clone OpenStack entities easily

Usage:
  cyclone [command]

Available Commands:
  backup      
  help        Help about any command
  image       Clone an image
  server      Clone a server
  version     Print version information
  volume      Clone a volume

Flags:
  -d, --debug                                     print out request and response objects
  -h, --help                                      help for cyclone
      --image-web-download                        use Glance web-download image import method (default true)
      --timeout-backup string                     timeout to wait for a backup status (default "24h")
      --timeout-image string                      timeout to wait for an image status (default "24h")
      --timeout-server string                     timeout to wait for a server status (default "24h")
      --timeout-snapshot string                   timeout to wait for a snapshot status (default "24h")
      --timeout-volume string                     timeout to wait for a volume status (default "24h")
      --to-application-credential-id string       destination application credential ID
      --to-application-credential-name string     destination application credential name
      --to-application-credential-secret string   destination application credential secret
      --to-auth-url string                        destination auth URL (if not provided, detected automatically from the source auth URL and destination region)
      --to-domain string                          destination domain
      --to-password string                        destination username password
      --to-project string                         destination project
      --to-region string                          destination region
      --to-username string                        destination username

Use "cyclone [command] --help" for more information about a command.
```

## Examples

### Clone an image between regions

```sh
$ source openrc-of-the-source-project
$ cyclone image 77c125f1-2c7b-473e-a56b-28a9a0bc4787 --to-region eu-de-2 --to-project destination-project-name --to-image-name image-from-source-project-name
```

### Clone an image between regions using download/upload method

Pay attention that the image data will be streamed through cyclone. It is recommended to use this method, when cyclone is executed directly on the VM, located in the source or destination region.

```sh
$ source openrc-of-the-source-project
$ cyclone image 77c125f1-2c7b-473e-a56b-28a9a0bc4787 --to-region eu-de-2 --to-project destination-project-name --to-image-name image-from-source-project-name --image-web-download=false
```

### Clone a bootable volume between regions

```sh
$ source openrc-of-the-source-project
$ cyclone volume c4c18329-b124-4a23-8546-cf1ca502ef95 --to-region eu-de-2 --to-project destination-project-name --to-volume-name volume-from-source-project-name
```

### Clone a volume within the same project, but different availability zones

```sh
$ source openrc-of-the-source-project
$ cyclone volume 97d682ae-840f-461f-b956-98af30533a22 --to-az eu-de-2a
```

### Clone a server with all attached volumes to a specific availability zone

```sh
$ source openrc-of-the-source-project
$ cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --to-az eu-de-2a
```

### Clone a server with a local storage to a server with bootable Cinder storage

`--bootable-volume 16` will create a 16 GiB bootable volume from the source VM snapshot and create a new VM using this volume.

```sh
$ source openrc-of-the-source-project
$ cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --bootable-volume 16
```

### Clone a server with a local disk or a bootable volume only

`--bootable-disk-only` flag allows to clone a VM with only a local disk or a bootable volume, ignoring all secondary attached volumes.

```sh
cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --bootable-disk-only
```

### Clona a server with a bootable volume to a server with a local disk

`--local-disk` allows to clone a VM with a Cinder bootable volume to a VM with a local disk.

```sh
cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --local-disk
```

### Upload a local image file into a backup

Properties must be defined, when a backup supposed to be restored to a bootable volume.

```sh
$ cyclone backup upload my-file.vmdk --to-container-name swift-backup-container --volume-size=160 --threads=16 \
  -p hw_vif_model=VirtualVmxnet3 \
  -p vmware_ostype=sles12_64Guest \
  -p hypervisor_type=vmware \
  -p min_ram=1008 \
  -p vmware_disktype=streamOptimized \
  -p disk_format=vmdk \
  -p hw_video_ram=16 \
  -p vmware_adaptertype=paraVirtual \
  -p container_format=bare \
  -p min_disk=10 \
  -p architecture=x86_64 \
  -p hw_disk_bus=scsi
```

### Upload the remote Glance image into a backup

```sh
$ cyclone backup upload my-glance-image --to-container-name swift-backup-container --volume-size=160 --threads=16
```

### Create a new volume from an existing backup

```sh
$ cyclone backup restore my-backup
```

## Build

```sh
$ make
# or within the docker container
$ make docker
```
