# cyclone

Clone OpenStack entities easily.

## Why?

In modern clusters compute instances are considered as a Cattle, but there are exceptions, when a compute instance is a Pet and needs care, especially when you need to migrate or clone it to a new OpenStack region or availability zone.

Here comes cyclone (**C**loud **Clone** or cclone) to help you with this task. It takes care about all volumes attached to a VM and clones them with all required intermediate type conversions.

## Help

By default Glance image data will be streamed through cyclone and the traffic will be consumed on the execution side. To enable the Glance V2 [web-download](https://docs.openstack.org/glance/latest/admin/interoperable-image-import.html#image-import-methods) method, set the `--image-web-download` flag. This method allows Glance to download an image using a remote URL. It is not recommended to use **web-download** method for images bigger than 1-10GiB, since Glance service will try to download the image to its intermediate local storage and may cause insufficient disk space error.

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
  completion  Generate the autocompletion script for the specified shell
  help        Help about any command
  image       Clone an image
  secret      Clone a secret
  server      Clone a server
  share       Clone a share
  version     Print version information
  volume      Clone a volume

Flags:
  -d, --debug                                     print out request and response objects
  -h, --help                                      help for cyclone
      --image-web-download                        use Glance web-download image import method
  -n, --no                                        assume "no" to all questions
      --timeout-backup string                     timeout to wait for a backup status (default "24h")
      --timeout-image string                      timeout to wait for an image status (default "24h")
      --timeout-secret string                     timeout to wait for a secret status (default "24h")
      --timeout-server string                     timeout to wait for a server status (default "24h")
      --timeout-share string                      timeout to wait for a share status (default "24h")
      --timeout-share-replica string              timeout to wait for a share replica status (default "24h")
      --timeout-share-snapshot string             timeout to wait for a share snapshot status (default "24h")
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
  -y, --yes                                       assume "yes" to all questions

Use "cyclone [command] --help" for more information about a command.
```

## Examples

### Clone an image between regions

```sh
$ source openrc-of-the-source-project
$ cyclone image 77c125f1-2c7b-473e-a56b-28a9a0bc4787 --to-region eu-de-2 --to-project destination-project-name --to-image-name image-from-source-project-name
```

~> **Note:** Please ensure that your OpenStack user has sufficient permissions (e.g. `image_admin` and `swiftoperator` user roles) before initiating the above command.

### Clone an image between regions using download/upload method

```sh
$ source openrc-of-the-source-project
$ cyclone image 77c125f1-2c7b-473e-a56b-28a9a0bc4787 --to-region eu-de-2 --to-project destination-project-name --to-image-name image-from-source-project-name
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
$ cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --to-az eu-de-2a --to-key-name my-nova-keypair
```

### Clone a server with a local storage to a server with bootable Cinder storage

`--bootable-volume 16` will create a 16 GiB bootable volume from the source VM snapshot and create a new VM using this volume.

```sh
$ source openrc-of-the-source-project
$ cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --bootable-volume 16 --to-key-name my-nova-keypair
```

### Clone a server with a local disk or a bootable volume only

`--bootable-disk-only` flag allows to clone a VM with only a local disk or a bootable volume, ignoring all secondary attached volumes.

```sh
$ source openrc-of-the-source-project
$ cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --bootable-disk-only --to-key-name my-nova-keypair
```

### Clone a server with a bootable volume to a server with a local disk

`--local-disk` allows to clone a VM with a Cinder bootable volume to a VM with a local disk.

```sh
$ source openrc-of-the-source-project
$ cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --local-disk --to-key-name my-nova-keypair
```

### Clone only server artifacts

The `--skip-server-creation` flag clones only images or volumes, which are used or attached to the source server. The destination server won't be created.
The example below will convert the server's local bootable disk to a bootable block storage, which can be attached to some server lately.

```sh
$ source openrc-of-the-source-project
$ cyclone server 6eb76733-95b7-4867-9f83-a6ab19804e2f --bootable-volume 64 --skip-server-creation
```

### Upload a local image file into a backup

Properties must be defined, when a backup supposed to be restored to a bootable volume.

```sh
$ source openrc-of-the-source-project
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

### Upload a remote Glance image into a backup

```sh
$ source openrc-of-the-source-project
$ cyclone backup upload my-glance-image --to-container-name swift-backup-container --volume-size=160 --threads=16
```

### Transfer a big volume from one region to another

~> **Note:** The `cyclone backup upload` command produces high traffic and CPU/RAM usage. It's recommended to run it inside a VM, located in the target region.

In this case you need to convert a volume to an image first:

```sh
$ source openrc-of-the-source-project
$ cyclone volume to-image my-cinder-volume --to-image-name my-glance-image
```

then transfer it within multiple parallel connections to a target backup resource with a further volume restore action.

```sh
$ source openrc-of-the-source-project
$ cyclone backup upload my-glance-image --to-container-name swift-backup-container --to-region my-region-1 \
  --volume-size=160 --threads=16 --restore-volume
```

It's strongly recommended to run the `cyclone backup upload` command inside a VM, located in the source or the target region.

### Create a new volume from an existing backup

```sh
$ source openrc-of-the-source-project
$ cyclone backup restore my-backup
```

### Clone an existing backup to another region

~> **Note:** The `cyclone backup clone` command produces high traffic. It's recommended to run it inside a VM, located in the source or the target region.

```sh
$ source openrc-of-the-source-project
$ cyclone backup clone my-backup --to-region my-region-1 --threads=16
```

### Manila shares support

Manila share type must support replicas, i.e.

```sh
$ openstack share type show default -c optional_extra_specs -f json | jq '.optional_extra_specs.replication_type'
"dr"
```

#### Clone a Manila share to a new share in a new availability zone

```sh
$ source openrc-of-the-source-project
$ cyclone share my-share --to-share-name my-new-share --to-az my-region-1b
```

#### Move an existing Manila share to a new availability zone

```sh
$ source openrc-of-the-source-project
$ cyclone share move my-share --to-az my-region-1b
```

## Build

```sh
$ make
# or within the docker container
$ make docker
```
