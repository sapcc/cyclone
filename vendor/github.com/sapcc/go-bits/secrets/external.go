// Copyright 2023 SAP SE
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package secrets

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// GetPasswordFromCommandIfRequested evaluates the $OS_PW_CMD environment
// variable if it exists and $OS_PASSWORD has not been provided.
func GetPasswordFromCommandIfRequested() error {
	pwCmd := os.Getenv("OS_PW_CMD")
	if pwCmd == "" || os.Getenv("OS_PASSWORD") != "" {
		return nil
	}
	// Retrieve user's password from external command.
	cmd := exec.Command("sh", "-c", pwCmd)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("could not execute OS_PW_CMD: %w", err)
	}
	os.Setenv("OS_PASSWORD", strings.TrimSuffix(string(out), "\n"))
	return nil
}
