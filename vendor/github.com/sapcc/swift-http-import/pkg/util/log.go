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

package util

import (
	"os"
	"strconv"

	"github.com/sapcc/go-bits/logg"
)

func init() {
	if parseBool(os.Getenv("DEBUG")) {
		logg.ShowDebug = true
	}
}

//LogIndividualTransfers is set to the boolean value of the
//LOG_TRANSFERS environment variable.
var LogIndividualTransfers = parseBool(os.Getenv("LOG_TRANSFERS"))

func parseBool(str string) bool {
	b, err := strconv.ParseBool(str)
	if err != nil {
		b = false
	}
	return b
}
