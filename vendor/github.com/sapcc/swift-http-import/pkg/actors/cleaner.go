/*******************************************************************************
*
* Copyright 2018 SAP SE
*
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You should have received a copy of the License along with this
* program. If not, you may obtain a copy of the License at
*
*     http://www.apache.org/licenses/LICENSE-2.0
*
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*
*******************************************************************************/

package actors

import (
	"context"
	"sort"

	"github.com/majewsky/schwift"
	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/swift-http-import/pkg/objects"
)

//FileInfoForCleaner contains information about a transferred file for the Cleaner actor.
type FileInfoForCleaner struct {
	objects.File
	Failed bool
}

//Cleaner is an actor that cleans up unknown objects on the target side (i.e.
//those objects which do not exist on the source side).
type Cleaner struct {
	Context context.Context
	Input   <-chan FileInfoForCleaner
	Report  chan<- ReportEvent
}

//Run implements the Actor interface.
func (c *Cleaner) Run() {
	isJobFailed := make(map[*objects.Job]bool)
	isFileTransferred := make(map[*objects.Job]map[string]bool) //string = object name incl. prefix (if any)

	//collect information about transferred files from the transferors
	//(we don't need to check Context.Done in the loop; when the process is
	//interrupted, main() will close our Input and we will move on)
	for info := range c.Input {
		//ignore all files in jobs where no cleanup is configured
		job := info.File.Job
		if job.Cleanup.Strategy == objects.KeepUnknownFiles {
			continue
		}

		if info.Failed {
			isJobFailed[job] = true
		}

		m, exists := isFileTransferred[job]
		if !exists {
			m = make(map[string]bool)
			isFileTransferred[job] = m
		}
		m[info.File.TargetObject().Name()] = true
	}
	if c.Context.Err() != nil {
		logg.Info("skipping cleanup phase: interrupt was received")
		return
	}
	if len(isJobFailed) > 0 {
		logg.Info(
			"skipping cleanup phase for %d job(s) because of failed file transfers",
			len(isJobFailed))
	}

	//perform cleanup if it is safe to do so
	for job, transferred := range isFileTransferred {
		if c.Context.Err() != nil {
			//interrupt received
			return
		}
		if !isJobFailed[job] {
			c.performCleanup(job, transferred)
		}
	}
}

func (c *Cleaner) performCleanup(job *objects.Job, isFileTransferred map[string]bool) {
	//collect objects to cleanup
	var objs []*schwift.Object
	for objectName := range job.Target.FileExists {
		if isFileTransferred[objectName] {
			continue
		}
		objs = append(objs, job.Target.Container.Object(objectName))
	}
	sort.Slice(objs, func(i, j int) bool {
		return objs[i].Name() < objs[j].Name()
	})

	if job.Cleanup.Strategy != objects.KeepUnknownFiles {
		logg.Info("starting cleanup of %d objects on target side", len(objs))
	}

	//perform cleanup according to selected strategy
	switch job.Cleanup.Strategy {
	case objects.ReportUnknownFiles:
		for _, obj := range objs {
			logg.Info("found unknown object on target side: %s", obj.FullName())
		}

	case objects.DeleteUnknownFiles:
		numDeleted, _, err := job.Target.Container.Account().BulkDelete(objs, nil, nil)
		c.Report <- ReportEvent{IsCleanup: true, CleanedUpObjectCount: int64(numDeleted)}
		if err != nil {
			logg.Error("cleanup of %d objects on target side failed: %s", (len(objs) - numDeleted), err.Error())
			if berr, ok := err.(schwift.BulkError); ok {
				for _, oerr := range berr.ObjectErrors {
					logg.Error("DELETE " + oerr.Error())
				}
			}
		}
	}
}
