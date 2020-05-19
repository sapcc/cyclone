/*******************************************************************************
*
* Copyright 2016-2017 SAP SE
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

	"github.com/sapcc/go-bits/logg"
	"github.com/sapcc/swift-http-import/pkg/objects"
)

//Transferor is an actor that transfers files from a Source to a target SwiftLocation.
//
//Files to transfer are read from the `Input` channel until it is closed.
//For each input file, a report is sent into the `Report` channel.
type Transferor struct {
	Context context.Context
	Input   <-chan objects.File
	Output  chan<- FileInfoForCleaner
	Report  chan<- ReportEvent
}

//Run implements the Actor interface.
func (t *Transferor) Run() {
	done := t.Context.Done()

	//main transfer loop - report successful and skipped transfers immediately,
	//but push back failed transfers for later retry
	aborted := false
	var filesToRetry []objects.File
LOOP:
	for {
		select {
		case <-done:
			aborted = true
			break LOOP
		case file, ok := <-t.Input:
			if !ok {
				break LOOP
			}
			result, size := file.PerformTransfer()
			if result == objects.TransferFailed {
				filesToRetry = append(filesToRetry, file)
			} else {
				t.Output <- FileInfoForCleaner{File: file, Failed: false}
				t.Report <- ReportEvent{IsFile: true, FileTransferResult: result, FileTransferBytes: size}
			}
		}
	}

	//retry transfer of failed files one more time
	if !aborted && len(filesToRetry) > 0 {
		logg.Info("retrying %d failed file transfers...", len(filesToRetry))
	}
	for _, file := range filesToRetry {
		result := objects.TransferFailed
		var size int64
		//...but only if we were not aborted (this is checked in every loop
		//iteration because the abort signal (i.e. Ctrl-C) could also happen
		//during this loop)
		if !aborted && t.Context.Err() == nil {
			result, size = file.PerformTransfer()
		}
		t.Output <- FileInfoForCleaner{File: file, Failed: result == objects.TransferFailed}
		t.Report <- ReportEvent{IsFile: true, FileTransferResult: result, FileTransferBytes: size}
	}

	//if interrupt was received, consume all remaining input to get the Scraper
	//moving (it might be stuck trying to send into the File channel while the
	//channel's buffer is full)
	for range t.Input {
	}
}
