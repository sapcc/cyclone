/*******************************************************************************
*
* Copyright 2020 SAP SE
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

// Package secrets provides convenience functions for working with auth
// credentials.
package secrets

import (
	"github.com/sapcc/go-bits/osext"
)

// FromEnv holds either a plain text value or a key for the environment
// variable from which the value can be retrieved.
// The key has the format: `{ fromEnv: ENVIRONMENT_VARIABLE }`.
type FromEnv string

// UnmarshalYAML implements the yaml.Unmarshaler interface.
func (p *FromEnv) UnmarshalYAML(unmarshal func(interface{}) error) error {
	//plain text value
	var plainTextInput string
	err := unmarshal(&plainTextInput)
	if err == nil {
		*p = FromEnv(plainTextInput)
		return nil
	}

	//retrieve value from the given environment variable key
	var envVariableInput struct {
		Key string `yaml:"fromEnv"`
	}
	err = unmarshal(&envVariableInput)
	if err != nil {
		return err
	}

	valFromEnv, err := osext.NeedGetenv(envVariableInput.Key)
	if err != nil {
		return err
	}

	*p = FromEnv(valFromEnv)

	return nil
}
