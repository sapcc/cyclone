# cyclone

Clone OpenStack entities easily.

## Why?

In modern clusters compute instances are considered as Cattles, but there are exceptions, when a compute instance is a Pet and needs care, especially when you need to migrate or clone it to a new OpenStack region or availability zone.

Here comes cyclone (**C**loud **Clone** or cclone) to help you with this task. It takes care about all volumes attached to a VM and clones them with all required intermediate type conversions.

## Help

The tool uses a Glance V2 [web-download](https://docs.openstack.org/glance/latest/admin/interoperable-image-import.html#image-import-methods) method to clone images. This method allows Glance to download an image using a remote URL.

A remote URL can be generated using a Swift [Temporary URL](https://docs.openstack.org/swift/latest/api/temporary_url_middleware.html).

A volume migration is performed by converting a volume to an image and further image migration between regions, then converting an image back to a volume.

A volume migration within the same region is performed using a [Volume Transfer](https://docs.openstack.org/cinder/latest/cli/cli-manage-volumes.html#transfer-a-volume) method.

By default the tool uses the same credentials from environment variables for the source and destination projects, but for the destination you can define different region, domain and project name. It is also possible to override destination credentials via OpenStack environment variables with the `TO_` prefix or via CLI parameters.

~> **Note:** Be aware about the quota, especially the source project quota, when cloning a volume. It requires up to 3x of the source volume size Cinder (Block Storage) quota.

~> **Note:** It is strongly recommended to shut down the VM before you start a VM or its volumes migration.

~> **Note:** By default cyclone writes all OpenStack request/response logs into a `cyclone` directory, located in System Temporary Directory. Define `-d` or `--debug` flag if you want to see these logs in console output.

```sh
Clone OpenStack entities easily

Usage:
  cyclone [command]

Available Commands:
  help        Help about any command
  image       Clone an image
  server      Clone a server
  version     Print version information
  volume      Clone a volume

Flags:
  -d, --debug                                     print out request and response objects
  -h, --help                                      help for cyclone
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

## Build

```sh
$ make
# or within the docker container
$ make docker
```
