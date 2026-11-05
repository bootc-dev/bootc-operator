// SPDX-License-Identifier: Apache-2.0

package bootc

import (
	"fmt"
	"strings"
)

// maxVersionLen matches the MaxLength of BootcNode status.bootcVersion.
const maxVersionLen = 256

// ParseVersion parses raw bootc --version output (e.g. "bootc 1.8.0")
// and returns the version string (e.g. "1.8.0"). Trailing fields are
// ignored to tolerate future output additions.
func ParseVersion(data []byte) (string, error) {
	fields := strings.Fields(string(data))
	if len(fields) < 2 || fields[0] != "bootc" {
		return "", fmt.Errorf("unexpected bootc --version output: %q", string(data))
	}
	if len(fields[1]) > maxVersionLen {
		return "", fmt.Errorf("bootc version exceeds %d characters", maxVersionLen)
	}
	return fields[1], nil
}
