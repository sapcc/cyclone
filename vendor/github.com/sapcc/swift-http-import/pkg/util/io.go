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

package util

import "io"

//FullReader is an io.ReadCloser whose Read() implementation always fills the read
//buffer as much as possible by calling Base.Read() repeatedly.
type FullReader struct {
	Base io.ReadCloser
}

//Read implements the io.Reader interface.
func (r *FullReader) Read(buf []byte) (int, error) {
	numRead := 0
	for numRead < len(buf) {
		n, err := r.Base.Read(buf[numRead:])
		numRead += n
		if err != nil { //including io.EOF
			return numRead, err
		}
	}
	return numRead, nil
}

//Close implements the io.Reader interface.
func (r *FullReader) Close() error {
	return r.Base.Close()
}
