/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package testutil

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WaitFor polls condFn at the given interval until it returns true or
// the timeout expires. If condFn returns an error, the test fails
// immediately with the error. On timeout, the test fails with msg.
func WaitFor(t *testing.T, timeout, interval time.Duration, msg string, condFn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		ok, err := condFn()
		if err != nil {
			t.Fatalf("waiting for %s: %v", msg, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(interval)
	}
}

// WaitForCreated polls until the object exists. The obj parameter is
// populated with the result on success. NotFound errors are retried;
// other errors fail the test immediately.
func WaitForCreated(t *testing.T, timeout, interval time.Duration, c client.Client, key client.ObjectKey, obj client.Object) {
	t.Helper()
	WaitFor(t, timeout, interval, key.String()+" to be created", func() (bool, error) {
		err := c.Get(context.Background(), key, obj)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	})
}

// WaitForDeleted polls until the object no longer exists. NotFound
// means success; other errors fail the test immediately.
func WaitForDeleted(t *testing.T, timeout, interval time.Duration, c client.Client, key client.ObjectKey, obj client.Object) {
	t.Helper()
	WaitFor(t, timeout, interval, key.String()+" to be deleted", func() (bool, error) {
		err := c.Get(context.Background(), key, obj)
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	})
}
