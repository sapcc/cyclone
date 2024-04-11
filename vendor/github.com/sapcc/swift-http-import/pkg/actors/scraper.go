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
	"sync"

	"github.com/sapcc/go-bits/logg"

	"github.com/sapcc/swift-http-import/pkg/objects"
)

// Scraper is an actor that reads directory listings on the source side to
// enumerate all files that need to be transferred.
//
// Scraping starts from the root directories of each job in the `Jobs` list.
// For each input file, a File struct is sent into the `Output` channel.
// For each directory, a report is sent into the `Report` channel.
type Scraper struct {
	Jobs   []*objects.Job
	Output chan<- objects.File
	Report chan<- ReportEvent
}

// Run implements the Actor interface.
func (s *Scraper) Run(ctx context.Context) {
	// push jobs in *reverse* order so that the first job will be processed first
	stack := make(directoryStack, 0, len(s.Jobs))
	for idx := range s.Jobs {
		stack = stack.Push(objects.Directory{
			Job:  s.Jobs[len(s.Jobs)-idx-1],
			Path: "/",
		})
	}

	for !stack.IsEmpty() {
		// check if state.Context.Done() is closed
		if ctx.Err() != nil {
			break
		}

		// fetch next directory from stack
		var directory objects.Directory
		stack, directory = stack.Pop()
		job := directory.Job // shortcut

		// handle file/subdirectory that was found
		handleFileSpec := func(fs objects.FileSpec) {
			excludeReason := job.Matcher.CheckFile(fs)
			if excludeReason != nil {
				logg.Debug("skipping %s: %s", fs.Path, excludeReason.Error())
				return
			}

			if fs.IsDirectory {
				stack = stack.Push(objects.Directory{
					Job:  directory.Job,
					Path: fs.Path,
				})
			} else {
				s.Output <- objects.File{
					Job:  job,
					Spec: fs,
				}
			}
		}

		// list entries for the directory. At the top level, try ListAllFiles if
		// supported by job.Source.
		var err *objects.ListEntriesError
		switch {
		case directory.Path == "/":
			c := make(chan objects.FileSpec, 10)
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				for entry := range c {
					handleFileSpec(entry)
				}
				wg.Done()
			}()

			err = job.Source.ListAllFiles(ctx, c)
			close(c)  // terminate receiver loop
			wg.Wait() // wait for receiver goroutine to finish
			if err != objects.ErrListAllFilesNotSupported {
				break
			}
			fallthrough // try ListEntries() if err == objects.ErrListAllFilesNotSupported
		default:
			var entries []objects.FileSpec
			entries, err = job.Source.ListEntries(ctx, directory.Path)
			if err == nil {
				for _, entry := range entries {
					handleFileSpec(entry)
				}
			}
		}

		// if listing failed, maybe retry later
		if err != nil {
			if err.Message == objects.ErrMessageGPGVerificationFailed {
				logg.Error("skipping job for source %s: %s", err.Location, err.FullMessage())
				job.IsScrapingIncomplete = true
				// report that a job was skipped
				s.Report <- ReportEvent{IsJob: true, JobSkipped: true}
				continue
			}
			if directory.RetryCounter >= 2 {
				logg.Error("giving up on %s: %s", err.Location, err.FullMessage())
				job.IsScrapingIncomplete = true
				s.Report <- ReportEvent{IsDirectory: true, DirectoryFailed: true}
				continue
			}
			logg.Error("skipping %s for now: %s", err.Location, err.FullMessage())
			directory.RetryCounter++
			stack = stack.PushBack(directory)
			continue
		}

		// report that a directory was successfully scraped
		s.Report <- ReportEvent{IsDirectory: true}
	}

	// signal to consumers that we're done
	close(s.Output)
}

// directoryStack is a []objects.Directory that implements LIFO semantics.
type directoryStack []objects.Directory

func (s directoryStack) IsEmpty() bool {
	return len(s) == 0
}

func (s directoryStack) Push(d objects.Directory) directoryStack {
	return append(s, d)
}

func (s directoryStack) Pop() (directoryStack, objects.Directory) {
	l := len(s)
	return s[:l-1], s[l-1]
}

func (s directoryStack) PushBack(d objects.Directory) directoryStack {
	return append([]objects.Directory{d}, s...)
}
