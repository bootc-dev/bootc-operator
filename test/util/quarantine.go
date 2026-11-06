// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"fmt"
	"os"
	"strconv"
	"testing"
)

// runQuarantinedEnv is the environment variable that opts in to running
// quarantined tests.
const runQuarantinedEnv = "E2E_RUN_QUARANTINED"

// Quarantine skips t unless the E2E_RUN_QUARANTINED environment variable
// is set to a truthy value. It marks a test as quarantined: known-flaky
// or failing for reasons outside the operator's control, so it does not
// block CI while the underlying issue is investigated and fixed. reason
// must describe the failure and reference the tracking issue, since that
// is what tells us when the quarantine can be lifted.
//
// Quarantine is temporary by design: remove the call once the test is
// reliably green again. To run quarantined tests on demand, e.g.
//
//	E2E_RUN_QUARANTINED=1 make e2e RUN=TestMidRolloutImageChange
func Quarantine(t *testing.T, reason string) {
	t.Helper()

	run, err := parseQuarantineOverride(os.Getenv(runQuarantinedEnv))
	if err != nil {
		t.Fatal(err)
	}
	if !run {
		t.Skipf(
			"quarantined: %s (set %s=1 to run it anyway)",
			reason,
			runQuarantinedEnv,
		)
	}
}

// parseQuarantineOverride interprets the E2E_RUN_QUARANTINED value. An
// empty value means quarantined tests are skipped.
func parseQuarantineOverride(value string) (bool, error) {
	if value == "" {
		return false, nil
	}
	run, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf(
			"invalid %s value %q: must be a boolean (e.g. 1 or true)",
			runQuarantinedEnv,
			value,
		)
	}
	return run, nil
}
