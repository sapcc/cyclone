package pkg

import (
	"encoding/hex"
	"encoding/json"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/gophercloud/gophercloud/v2"
)

type sha256file struct {
	sync.Mutex
	BackupDescription *string
	BackupID          string
	BackupName        *string
	ChunkSize         int
	CreatedAt         time.Time
	Version           string
	VolumeID          string
	// using map here, because concurrency may mix up the actual order
	// the actual order will be restored during JSON marshalling
	Sha256s map[int][][32]byte
}

func (r *sha256file) MarshalJSON() ([]byte, error) {
	type s struct {
		BackupDescription *string  `json:"backup_description"`
		BackupID          string   `json:"backup_id"`
		BackupName        *string  `json:"backup_name"`
		ChunkSize         int      `json:"chunk_size"`
		Version           string   `json:"version"`
		VolumeID          string   `json:"volume_id"`
		CreatedAt         string   `json:"created_at"`
		Sha256s           []string `json:"sha256s"`
	}

	// restore the actual order of hashsums
	keys := make([]int, 0, len(r.Sha256s))
	for k := range r.Sha256s {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	var str []string
	for _, k := range keys {
		for _, v := range r.Sha256s[k] {
			str = append(str, hex.EncodeToString(v[:]))
		}
		r.Sha256s[k] = nil
	}
	r.Sha256s = nil

	// clean the r.Sha256s memory
	runtime.GC()

	return json.Marshal(s{
		r.BackupDescription,
		r.BackupID,
		r.BackupName,
		r.ChunkSize,
		r.Version,
		r.VolumeID,
		r.CreatedAt.Format(gophercloud.RFC3339ZNoT),
		str,
	})
}

type backupChunkEntry map[string]map[string]interface{}

type metadata struct {
	sync.Mutex
	BackupDescription *string
	BackupID          string
	BackupName        *string
	CreatedAt         time.Time
	ParentID          *string
	Version           string
	VolumeID          string
	VolumeMeta        string
	// using map here, because concurrency may mix up the actual order
	// the actual order will be restored during JSON marshalling
	Objects map[int]backupChunkEntry
}

func (r *metadata) MarshalJSON() ([]byte, error) {
	type s struct {
		BackupDescription *string            `json:"backup_description"`
		BackupID          string             `json:"backup_id"`
		BackupName        *string            `json:"backup_name"`
		ParentID          *string            `json:"parent_id"`
		Version           string             `json:"version"`
		VolumeID          string             `json:"volume_id"`
		VolumeMeta        string             `json:"volume_meta"`
		CreatedAt         string             `json:"created_at"`
		Objects           []backupChunkEntry `json:"objects"`
	}

	// restore the actual order of hashsums
	keys := make([]int, 0, len(r.Objects))
	for k := range r.Objects {
		keys = append(keys, k)
	}
	sort.Ints(keys)

	obj := make([]backupChunkEntry, 0, len(keys))
	for _, k := range keys {
		obj = append(obj, r.Objects[k])
		r.Objects[k] = nil
	}
	r.Objects = nil

	// clean the r.Objects memory
	runtime.GC()

	return json.Marshal(s{
		r.BackupDescription,
		r.BackupID,
		r.BackupName,
		r.ParentID,
		r.Version,
		r.VolumeID,
		r.VolumeMeta,
		r.CreatedAt.Format(gophercloud.RFC3339ZNoT),
		obj,
	})
}

type volumeMeta struct {
	VolumeBaseMeta       volumeBaseMeta    `json:"volume-base-metadata"`
	Version              int               `json:"version"`
	VolumeGlanceMetadata map[string]string `json:"volume-glance-metadata"`
}

func (r *volumeMeta) MarshalJSON() ([]byte, error) {
	if r.VolumeGlanceMetadata == nil {
		r.VolumeGlanceMetadata = make(map[string]string)
	}
	return json.Marshal(r)
}

type volumeBaseMeta struct {
	MigrationStatus           *string    `json:"migration_status"`
	ProviderID                *string    `json:"provider_id"`
	AvailabilityZone          string     `json:"availability_zone"`
	TerminatedAt              *time.Time `json:"-"`
	UpdatedAt                 time.Time  `json:"-"`
	ProviderGeometry          *string    `json:"provider_geometry"`
	ReplicationExtendedStatus *string    `json:"replication_extended_status"`
	ReplicationStatus         *string    `json:"replication_status"`
	SnapshotID                *string    `json:"snapshot_id"`
	EC2ID                     *string    `json:"ec2_id"`
	DeletedAt                 *time.Time `json:"-"`
	ID                        string     `json:"id"`
	Size                      int        `json:"size"`
	UserID                    string     `json:"user_id"`
	DisplayDescription        *string    `json:"display_description"`
	ClusterName               *string    `json:"cluster_name"`
	ProjectID                 string     `json:"project_id"`
	LaunchedAt                time.Time  `json:"-"`
	ScheduledAt               time.Time  `json:"-"`
	Status                    string     `json:"status"`
	VolumeTypeID              string     `json:"volume_type_id"`
	Multiattach               bool       `json:"multiattach"`
	Deleted                   bool       `json:"deleted"`
	ServiceUUID               string     `json:"service_uuid"`
	ProviderLocation          *string    `json:"provider_location"`
	Host                      string     `json:"host"`
	ConsistencygroupID        *string    `json:"consistencygroup_id"`
	SourceVolID               *string    `json:"source_volid"`
	ProviderAuth              *string    `json:"provider_auth"`
	PreviousStatus            string     `json:"previous_status"`
	DisplayName               string     `json:"display_name"`
	Bootable                  bool       `json:"bootable"`
	CreatedAt                 time.Time  `json:"-"`
	AttachStatus              string     `json:"attach_status"`
	NameID                    *string    `json:"_name_id"`
	EncryptionKeyID           *string    `json:"encryption_key_id"`
	ReplicationDriverData     *string    `json:"replication_driver_data"`
	GroupID                   *string    `json:"group_id"`
	SharedTargets             bool       `json:"shared_targets"`
}

func (r *volumeBaseMeta) MarshalJSON() ([]byte, error) {
	type t volumeBaseMeta
	type s struct {
		t
		UpdatedAt    string  `json:"updated_at"`
		LaunchedAt   string  `json:"launched_at"`
		ScheduledAt  string  `json:"scheduled_at"`
		CreatedAt    string  `json:"created_at"`
		TerminatedAt *string `json:"terminated_at"`
		DeletedAt    *string `json:"deleted_at"`
	}

	v := s{
		t(*r),
		r.UpdatedAt.Format(gophercloud.RFC3339ZNoT),
		r.LaunchedAt.Format(gophercloud.RFC3339ZNoT),
		r.ScheduledAt.Format(gophercloud.RFC3339ZNoT),
		r.CreatedAt.Format(gophercloud.RFC3339ZNoT),
		nil,
		nil,
	}

	if r.TerminatedAt != nil {
		s := r.TerminatedAt.Format(gophercloud.RFC3339ZNoT)
		v.TerminatedAt = &s
	}

	if r.DeletedAt != nil {
		s := r.DeletedAt.Format(gophercloud.RFC3339ZNoT)
		v.DeletedAt = &s
	}

	return json.Marshal(v)
}
