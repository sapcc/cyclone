package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/blockstorage/extensions/backups"
	backups_utils "github.com/gophercloud/utils/openstack/blockstorage/extensions/backups"
	"github.com/majewsky/schwift/gopherschwift"
	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/swift-http-import/pkg/actors"
	"github.com/sapcc/swift-http-import/pkg/objects"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func prepareSwiftConfig(srcObjectClient, dstObjectClient *gophercloud.ServiceClient, srcContainerName, dstContainerName, prefix string, threads uint) (*objects.Configuration, error) {
	srcSchwift, err := gopherschwift.Wrap(srcObjectClient, &gopherschwift.Options{
		UserAgent: srcObjectClient.ProviderClient.UserAgent.Join(),
	})
	if err != nil {
		return nil, err
	}
	srcContainer, err := srcSchwift.Container(srcContainerName).EnsureExists()
	if err != nil {
		return nil, err
	}

	dstSchwift, err := gopherschwift.Wrap(dstObjectClient, &gopherschwift.Options{
		UserAgent: dstObjectClient.ProviderClient.UserAgent.Join(),
	})
	if err != nil {
		return nil, err
	}
	dstContainer, err := dstSchwift.Container(dstContainerName).EnsureExists()
	if err != nil {
		return nil, err
	}

	source := objects.SwiftLocation{
		Account:          srcSchwift,
		Container:        srcContainer,
		ContainerName:    srcContainerName,
		ObjectNamePrefix: filepath.Dir(prefix),
	}

	target := objects.SwiftLocation{
		Account:          dstSchwift,
		Container:        dstContainer,
		ContainerName:    dstContainerName,
		ObjectNamePrefix: filepath.Dir(prefix),
	}

	// TODO: fail, when target file exists
	rx, _ := regexp.Compile(fmt.Sprintf("%s.*", filepath.Base(prefix)))
	config := &objects.Configuration{
		Jobs: []*objects.Job{
			{
				Source: objects.SourceUnmarshaler{
					Source: &source,
				},
				Target: &target,
				Matcher: objects.Matcher{
					IncludeRx: rx,
				},
			},
		},
	}
	config.WorkerCounts.Transfer = threads

	return config, nil
}

func transferObjects(config *objects.Configuration) (int, actors.Stats) {
	//setup the Report actor
	reportChan := make(chan actors.ReportEvent)
	report := &actors.Report{
		Input:     reportChan,
		Statsd:    config.Statsd,
		StartTime: startTime,
	}
	wgReport := &sync.WaitGroup{}
	actors.Start(report, wgReport)

	//do the work
	runPipeline(config, reportChan)

	//shutdown Report actor
	close(reportChan)
	wgReport.Wait()

	return report.ExitCode, report.Stats()
}

func runPipeline(config *objects.Configuration, report chan<- actors.ReportEvent) {
	//receive SIGINT/SIGTERM signals
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	//setup a context that shuts down all pipeline actors when one of the signals above is received
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	go func() {
		<-sigs
		logg.Error("Interrupt received! Shutting down...")
		cancelFunc()
	}()

	//start the pipeline actors
	var wg sync.WaitGroup
	var wgTransfer sync.WaitGroup
	queue1 := make(chan objects.File, 10)              //will be closed by scraper when it's done
	queue2 := make(chan actors.FileInfoForCleaner, 10) //will be closed by us when all transferors are done

	actors.Start(&actors.Scraper{
		Context: ctx,
		Jobs:    config.Jobs,
		Output:  queue1,
		Report:  report,
	}, &wg)

	for i := uint(0); i < config.WorkerCounts.Transfer; i++ {
		actors.Start(&actors.Transferor{
			Context: ctx,
			Input:   queue1,
			Output:  queue2,
			Report:  report,
		}, &wg, &wgTransfer)
	}

	actors.Start(&actors.Cleaner{
		Context: ctx,
		Input:   queue2,
		Report:  report,
	}, &wg)

	//wait for transfer phase to finish
	wgTransfer.Wait()
	//signal to cleaner to start its work
	close(queue2)
	//wait for remaining workers to finish
	wg.Wait()

	// signal.Reset(os.Interrupt, syscall.SIGTERM)
}

func cloneBackup(srcVolumeClient, srcObjectClient, dstVolumeClient, dstObjectClient *gophercloud.ServiceClient, srcBackup *backups.Backup, toBackupName string, toContainerName string, threads uint) (*backups.Backup, error) {
	backupExport, err := backups.Export(srcVolumeClient, srcBackup.ID).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to export a %q backup: %s", srcBackup.ID, err)
	}

	backupRecord := backups.ImportBackup{}
	err = json.Unmarshal(backupExport.BackupURL, &backupRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal a %q backup record: %s", srcBackup.ID, err)
	}

	if backupRecord.ObjectCount == nil {
		return nil, fmt.Errorf("backup record contains nil object_count")
	}

	if toContainerName == "" {
		toContainerName = *backupRecord.Container
	}
	description := fmt.Sprintf("cloned from %q backup (%q project ID)", srcBackup.ID, backupRecord.ProjectID)
	backupRecord.DisplayDescription = &description
	if toBackupName != "" {
		backupRecord.DisplayName = &toBackupName
	}

	// TODO: recursive
	config, err := prepareSwiftConfig(srcObjectClient, dstObjectClient, *backupRecord.Container, toContainerName, *backupRecord.ServiceMetadata, threads)
	if err != nil {
		return nil, err
	}

	ret, stats := transferObjects(config)
	if ret != 0 {
		return nil, fmt.Errorf("error while transferring objects")
	}

	expectedObjectsCount := int64(*backupRecord.ObjectCount) + 1 // + metadata
	// TODO: stats.FilesFound vs stats.FilesTransferred
	if stats.FilesFound != expectedObjectsCount {
		return nil, fmt.Errorf("error while transferring objects: an amount of transferred files doesn't correspond to an amount of file in the record: %d != %d", stats.FilesFound, expectedObjectsCount)
	}

	// generate a new backup UUID
	if v, err := uuid.NewUUID(); err != nil {
		return nil, fmt.Errorf("failed to generate a new backup UUID: %s", err)
	} else {
		backupRecord.ID = v.String()
	}
	backupRecord.Container = &toContainerName

	backupURL, err := json.Marshal(backupRecord)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal a backup record: %s", err)
	}

	backupImport := backups.ImportOpts{
		BackupService: backupExport.BackupService,
		BackupURL:     backupURL,
	}

	importResponse, err := backups.Import(dstVolumeClient, backupImport).Extract()
	if err != nil {
		return nil, fmt.Errorf("failed to import a backup: %s", err)
	}

	return waitForBackup(dstVolumeClient, importResponse.ID, waitForBackupSec)
}

// BackupCmd represents the volume command
var BackupCloneCmd = &cobra.Command{
	Use:   "clone <name|id>",
	Args:  cobra.ExactArgs(1),
	Short: "Clone a backup",
	PreRunE: func(cmd *cobra.Command, args []string) error {
		if err := parseTimeoutArgs(); err != nil {
			return err
		}
		return viper.BindPFlags(cmd.Flags())
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// clone backup
		backup := args[0]

		toName := viper.GetString("to-backup-name")
		toContainerName := viper.GetString("to-container-name")
		threads := viper.GetUint("threads")

		if threads == 0 {
			return fmt.Errorf("an amount of threads cannot be zero")
		}

		if toContainerName == "" {
			return fmt.Errorf("swift container name connot be empty")
		}

		// source and destination parameters
		loc, err := getSrcAndDst("")
		if err != nil {
			return err
		}

		srcProvider, err := newOpenStackClient(loc.Src)
		if err != nil {
			return fmt.Errorf("failed to create a source OpenStack client: %s", err)
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
		if v, err := backups_utils.IDFromName(srcVolumeClient, backup); err == nil {
			backup = v
		} else if err, ok := err.(gophercloud.ErrMultipleResourcesFound); ok {
			return err
		}

		dstProvider, err := newOpenStackClient(loc.Dst)
		if err != nil {
			return fmt.Errorf("failed to create a destination OpenStack client: %s", err)
		}

		dstVolumeClient, err := newBlockStorageV3Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to create destination volume client: %s", err)
		}

		dstObjectClient, err := newObjectStorageV1Client(dstProvider, loc.Dst.Region)
		if err != nil {
			return fmt.Errorf("failed to destination source object storage client: %s", err)
		}

		srcBackup, err := waitForBackup(srcVolumeClient, backup, waitForBackupSec)
		if err != nil {
			return fmt.Errorf("failed to wait for a %q backup: %s", backup, err)
		}

		if srcBackup.IsIncremental {
			return fmt.Errorf("incremental backups are not supported")
		}

		defer measureTime()

		dstBackup, err := cloneBackup(srcVolumeClient, srcObjectClient, dstVolumeClient, dstObjectClient, srcBackup, toName, toContainerName, threads)
		if err != nil {
			return err
		}

		log.Printf("Migrated target backup name is %q (id: %q)", dstBackup.Name, dstBackup.ID)

		return nil
	},
}
