/*******************************************************************************
*
* Copyright 2017 SAP SE
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

import "sync"

//Actor is something that can be run in its own goroutine.
//This package contains various structs that satisfy this interface,
//and which make up the bulk of the behavior of swift-http-import.
type Actor interface {
	Run()
}

//Start runs the given Actor in its own goroutine.
func Start(a Actor, wgs ...*sync.WaitGroup) {
	for _, wg := range wgs {
		wg.Add(1)
	}
	go func() {
		defer func() {
			for _, wg := range wgs {
				wg.Done()
			}
		}()
		a.Run()
	}()
}
